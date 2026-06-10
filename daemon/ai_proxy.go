// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
)

// ai_proxy.go is the platform's "included AI credits" surface. Apps the
// manager deploys on this machine can ask the daemon to make AI calls
// on their behalf without ever touching the platform's API key.
//
// The threat model the proxy closes:
//
//   On Managed machines, the platform's OpenAI API key lives at
//   /etc/vibecraft/openai.key (0600 root). The root daemon reads it
//   for the manager and, on Managed only, for included app AI credits.
//   Hosted apps deployed via /api/daemon/install-app-service run as
//   the vibecraft user on the host. If those apps could read the key
//   directly (root-only, so they can't) OR if we ever wrote the key
//   into their environment, a single `cat $APP_HOME/.env` or
//   `journalctl -u my-app` would leak it.
//
// Customer-owned keys are a separate concern: the customer can put
// them directly in app env or pull from the vault. This proxy is
// exclusively for the included monthly AI budget that the platform
// pays for. In operator-owned BYOM/self-host key mode, this local
// included-credit proxy is disabled unless a future relay is added.
//
// Design — deliberately dumb passthrough:
//
//   1. App calls http://127.0.0.1:8420/api/ai/credits/<provider>/<path>
//      with a per-machine bearer token in `x-api-key` or
//      `Authorization: Bearer ...`. The token is auto-injected into
//      every install-app-service unit's Environment= lines, so the
//      app gets it for free without any vault dance.
//
//   2. Daemon validates the bearer against /etc/vibecraft/ai_credits.token
//      (constant-time compare).
//
//   3. Daemon strips any incoming `x-api-key` / `Authorization` /
//      `anthropic-api-key` headers — never trust the app's claim
//      about which key to use upstream.
//
//   4. Daemon injects the platform's real key from openai.key (and
//      any other explicitly configured managed-provider key), forwards
//      the request to the canonical upstream host with the original
//      method, body, and non-auth headers intact.
//
//   5. Streaming responses (SSE) are passed through with Flush per
//      chunk; non-streaming responses are buffered just long enough
//      to parse usage for the ledger before being written to the
//      caller.
//
//   6. After the response, the ledger is charged for the call's
//      token usage. If the per-machine monthly budget is exceeded,
//      subsequent calls return 429 until the next UTC month.
//
// What we deliberately do NOT do:
//
//   - Rewrite the request body. The proxy is transport-only. Apps
//     speak the upstream's native API; we just inject the key and
//     count tokens.
//   - Cache responses. AI calls are not cacheable in any sane way
//     and caching would change failure semantics.
//   - Mask the customer's prompts from audit. The audit log records
//     per-call metadata (provider, model, tokens, cost), never the
//     prompt body.

const (
	aiCreditsTokenPath = "/etc/vibecraft/ai_credits.token"
	aiBudgetPath       = "/etc/vibecraft/ai_budget_cents"
	aiLedgerPath       = "/var/lib/vibecraft/ai_ledger.json"

	// Default monthly budget if /etc/vibecraft/ai_budget_cents is
	// missing or unreadable. $50 — matches the Starter plan AI budget
	// in lib/config.ts; the cloud-init writes the real plan value at
	// provision time. Defensive default so a misconfigured machine
	// doesn't accidentally grant unlimited AI.
	defaultMonthlyBudgetCents = 5000

	// Upstream timeouts. AI responses can be long-running (especially
	// for large reasoning tasks), so we err generous. The customer's
	// app can cancel mid-stream via context if it wants.
	upstreamHeaderTimeout = 30 * time.Second
	upstreamTotalTimeout  = 600 * time.Second // 10min hard cap

	anthropicAPIBase = "https://api.anthropic.com"
	openaiAPIBase    = "https://api.openai.com"
)

// providerSpec is the per-upstream knowledge the proxy needs: where
// to forward, which header carries the API key for that provider,
// and (for budget accounting) how to find usage in the response.
//
// Anthropic and OpenAI use different conventions and both speak SSE.
// Encoding the differences here keeps the handler itself uniform.
type providerSpec struct {
	name           string            // "anthropic" / "openai"
	upstreamBase   string            // "https://api.anthropic.com"
	authHeaderName string            // header the upstream wants the key in
	authHeaderFmt  string            // "" → raw key; "Bearer %s" → bearer scheme
	keyPath        string            // /etc/vibecraft/anthropic.key
	extraHeaders   map[string]string // e.g. anthropic-version
	parseUsage     func([]byte) (in, out int, model string)
	parseSSEUsage  func([]byte) (in, out int, model string)
}

var anthropicSpec = providerSpec{
	name:           "anthropic",
	upstreamBase:   anthropicAPIBase,
	authHeaderName: "x-api-key",
	authHeaderFmt:  "%s",
	keyPath:        "/etc/vibecraft/anthropic.key",
	extraHeaders: map[string]string{
		"anthropic-version": "2023-06-01",
	},
	parseUsage:    parseAnthropicJSONUsage,
	parseSSEUsage: parseAnthropicSSEUsage,
}

var openaiSpec = providerSpec{
	name:           "openai",
	upstreamBase:   openaiAPIBase,
	authHeaderName: "Authorization",
	authHeaderFmt:  "Bearer %s",
	keyPath:        "/etc/vibecraft/openai.key",
	parseUsage:     parseOpenAIJSONUsage,
	parseSSEUsage:  parseOpenAISSEUsage,
}

func (s *Server) platformAIProxyEnabled() bool {
	if s == nil || s.cfg == nil || s.cfg.ManagerKeyMode == "" {
		return true
	}
	return s.cfg.ManagerKeyMode == "platform"
}

// loadOrCreateAICreditsToken is the daemon-startup hook: read the
// existing per-machine bearer token, or generate one and persist
// before any traffic can arrive.
//
// The token file is 0640 root:vibecraft — root creates and the
// vibecraft-user-systemd apps need it in env (we read it as root and
// inject into the unit's Environment= line at install-app-service
// time, so the app process itself never opens this file). Per-machine
// regeneration would invalidate every running app's env, so we
// generate exactly once.
func loadOrCreateAICreditsToken() (string, error) {
	if b, err := os.ReadFile(aiCreditsTokenPath); err == nil {
		t := strings.TrimSpace(string(b))
		if len(t) >= 32 {
			return t, nil
		}
		// Existing file is too short to be a credible token. Treat as
		// missing and regenerate — better to invalidate running apps
		// once than to keep a weak token in service.
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating ai credits token: %w", err)
	}
	tok := "vc-" + hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(aiCreditsTokenPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir for ai credits token: %w", err)
	}
	// Write 0600 first, then chgrp to vibecraft. The token is
	// vibecraft-readable so the install-app-service env injection can
	// still pull it under a uid drop in the future; today we read it
	// as root and inject into the unit file.
	if err := os.WriteFile(aiCreditsTokenPath, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing ai credits token: %w", err)
	}
	_ = chownToVibecraft(aiCreditsTokenPath) // best effort; dev machines without vibecraft user keep 0600 root
	return tok, nil
}

// AICreditsToken caches the token in memory after first read. Lookup
// happens on every request; the file read happens once per daemon
// lifetime.
type AICreditsToken struct{ value string }

func (t *AICreditsToken) validate(presented string) bool {
	if t == nil || t.value == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(t.value)) == 1
}

// requestPresentedToken pulls the bearer from any of the standard
// header shapes the AI SDKs use. We accept whichever the SDK happened
// to set — the app doesn't have to know which provider is downstream.
func requestPresentedToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if strings.HasPrefix(v, "Bearer ") {
			return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// ─── Budget ledger ───────────────────────────────────────────────────

// aiLedger is the persistent record of how much we've spent on the
// platform's behalf this month. Lives at aiLedgerPath; one entry per
// UTC month, rolled over on the 1st.
type aiLedger struct {
	// Month is the UTC month this entry covers, formatted "2026-01".
	// We compare strings directly — calendar math via time.Parse is
	// overkill for "did the month change since last write."
	Month string `json:"month"`

	// SpentCents is the running total for this month across ALL apps
	// on this machine. Integer cents — float dollars would accumulate
	// rounding error over thousands of calls.
	SpentCents int `json:"spent_cents"`

	// ByModel is a humans-debugging-the-bill breakdown, not load-
	// bearing. Recorded for the future "what's eating my budget" UI.
	ByModel map[string]int `json:"by_model,omitempty"`

	// LastUpdated for staleness diagnostics.
	LastUpdated string `json:"last_updated,omitempty"`
}

// budgetService owns the in-memory ledger + the budget cap. All
// charge / check operations go through it so the mutex is the only
// concurrency boundary.
type budgetService struct {
	mu     sync.Mutex
	ledger aiLedger

	// reservedCents is the sum of worst-case costs for calls that have
	// passed the gate but whose upstream response (and therefore real
	// cost) hasn't landed yet. Counting it against the cap is what makes
	// the budget hold under concurrency: N parallel calls can no longer
	// each see "room for one more" while the others are still in flight.
	// Reset to 0 on month rollover (in-flight calls reconcile to the new
	// month's tally via settle()).
	reservedCents int

	monthlyCap  int          // cents — static fallback cap (pre-budget-fetch)
	persistFunc func() error // override for tests

	// pooled, when set, returns the plan-aware pooled budget from the
	// BudgetTracker: remainingCents (already net of reported spend across all
	// the account's machines, incl. this proxy's reported spend), whether the
	// plan is unmetered, and whether a budget has been fetched yet. It is the
	// authoritative ceiling — plan-sized and top-up-aware — and supersedes the
	// static monthlyCap whenever a budget is available. Before the first fetch
	// (have=false) the static cap applies as a startup safety net. This closes
	// the bug where the cap was a hardcoded $50 for every plan because nothing
	// ever wrote /etc/vibecraft/ai_budget_cents.
	pooled func() (remainingCents int, unmetered bool, have bool)
}

// reserveCents is the worst-case cost we hold against the budget for a
// single in-flight call before its real usage is known. Sized so a
// pathological large reasoning response can't blow the cap while admitted:
// 200k input + 200k output at the Opus-level fallback rate
// ($15/$75 per Mtok) ≈ $18 → 1800c. allow() admits a call only when the
// committed + already-reserved spend leaves room for one more reservation,
// so the worst-case overshoot is bounded by this single reservation rather
// than (concurrent calls) × (per-call cost).
const reserveCents = 1800

func newBudgetService() *budgetService {
	bs := &budgetService{
		monthlyCap: readBudgetCap(),
	}
	bs.ledger = bs.loadLedger()
	bs.rollOverIfNewMonth()
	bs.persistFunc = bs.persistLedger
	return bs
}

func readBudgetCap() int {
	b, err := os.ReadFile(aiBudgetPath)
	if err != nil {
		return defaultMonthlyBudgetCents
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return defaultMonthlyBudgetCents
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return defaultMonthlyBudgetCents
	}
	return n
}

func (bs *budgetService) loadLedger() aiLedger {
	b, err := os.ReadFile(aiLedgerPath)
	if err != nil {
		return aiLedger{Month: utcMonth(time.Now())}
	}
	var L aiLedger
	if err := json.Unmarshal(b, &L); err != nil {
		// Corrupt ledger — start fresh rather than over-charge a customer
		// because of a parse error. Slight under-charge is the safer
		// failure mode here.
		return aiLedger{Month: utcMonth(time.Now())}
	}
	if L.Month == "" {
		L.Month = utcMonth(time.Now())
	}
	return L
}

func (bs *budgetService) persistLedger() error {
	bs.ledger.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(bs.ledger, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(aiLedgerPath), 0o755); err != nil {
		return err
	}
	tmp := aiLedgerPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, aiLedgerPath)
}

// rollOverIfNewMonth resets the ledger when the UTC month changes.
// Called on startup and on every charge attempt so we don't accumulate
// across months for a long-running daemon.
func (bs *budgetService) rollOverIfNewMonth() {
	now := utcMonth(time.Now())
	if bs.ledger.Month != now {
		bs.ledger = aiLedger{Month: now}
		// In-flight reservations from the prior month still settle (their
		// real cost lands in the new month's tally). Drop the prior month's
		// reservation total so it doesn't suppress the fresh budget.
		bs.reservedCents = 0
	}
}

// allow returns nil if the budget has headroom, or an error describing
// why the call should be rejected. It accounts for BOTH committed spend
// and in-flight reservations, so a request is rejected once committed +
// reserved spend has reached the cap. This is a read-only gate (it does
// not reserve); reserve() is the atomic admit-and-hold the handler uses.
func (bs *budgetService) allow() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.rollOverIfNewMonth()
	room, unmetered, ceiling := bs.roomCents()
	if !unmetered && room <= 0 {
		return errBudgetExhausted{
			Month:       bs.ledger.Month,
			SpentCents:  bs.ledger.SpentCents,
			BudgetCents: ceiling,
		}
	}
	return nil
}

// SetPooledBudget wires the plan-aware pooled-budget source (the BudgetTracker
// snapshot). Safe to call before serving; nil leaves the static cap in force.
func (bs *budgetService) SetPooledBudget(fn func() (remainingCents int, unmetered bool, have bool)) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.pooled = fn
}

// currentSpentCents reports this month's committed proxy spend so the
// BudgetTracker can include hosted-tool AI in the spend it reports to the
// platform (otherwise that spend never bills back). Caller-safe (locks).
func (bs *budgetService) currentSpentCents() int {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.rollOverIfNewMonth()
	return bs.ledger.SpentCents
}

// roomCents returns the cents available for a new reservation, whether the
// budget is unmetered, and a ceiling figure for the over-budget error. Caller
// must hold bs.mu. Prefers the plan-aware pooled budget (authoritative); falls
// back to the static per-machine cap only before the first budget fetch.
func (bs *budgetService) roomCents() (room int, unmetered bool, ceiling int) {
	if bs.pooled != nil {
		if remaining, um, have := bs.pooled(); have {
			if um {
				return 1 << 30, true, -1
			}
			return remaining - bs.reservedCents, false, bs.ledger.SpentCents + remaining
		}
	}
	return bs.monthlyCap - bs.ledger.SpentCents - bs.reservedCents, false, bs.monthlyCap
}

// reserve atomically admits a call and holds reserveCents against the
// budget for the duration of the upstream round-trip. It returns the
// reserved amount (to pass to settle) and an error when the budget — with
// committed spend AND all currently in-flight reservations counted — has no
// room for one more call. Because the check and the reservation happen under
// a single lock acquisition, N concurrent callers can never all pass the
// gate while the budget is near the cap: each reservation is visible to the
// next caller. The worst-case overshoot is one reserveCents, not N×cost.
func (bs *budgetService) reserve() (int, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.rollOverIfNewMonth()
	room, unmetered, ceiling := bs.roomCents()
	if !unmetered && room <= 0 {
		return 0, errBudgetExhausted{
			Month:       bs.ledger.Month,
			SpentCents:  bs.ledger.SpentCents,
			BudgetCents: ceiling,
		}
	}
	bs.reservedCents += reserveCents
	return reserveCents, nil
}

// settle releases a reservation taken by reserve() and commits the call's
// real cost. Called exactly once per successful reserve(), after the
// upstream response is parsed. The reservation is always released (even when
// the real cost is 0, e.g. an upstream error), so a failed call doesn't
// permanently pin budget. The real cost is committed to the ledger and the
// breakdown, mirroring charge().
func (bs *budgetService) settle(reserved int, model string, costCents int) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.rollOverIfNewMonth()
	bs.reservedCents -= reserved
	if bs.reservedCents < 0 {
		bs.reservedCents = 0
	}
	if costCents > 0 {
		bs.ledger.SpentCents += costCents
		if bs.ledger.ByModel == nil {
			bs.ledger.ByModel = map[string]int{}
		}
		if model != "" {
			bs.ledger.ByModel[model] += costCents
		}
	}
	if err := bs.persistFunc(); err != nil {
		fmt.Fprintf(os.Stderr, "[ai_proxy] persisting ledger failed: %v\n", err)
	}
}

// charge adds the cost of a completed call to the ledger and
// persists. Best-effort: a persist failure is logged but does not
// roll back the in-memory tally (rolling back risks losing every
// charge in this daemon's lifetime to a transient disk error).
func (bs *budgetService) charge(model string, costCents int) {
	if costCents <= 0 {
		return
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.rollOverIfNewMonth()
	bs.ledger.SpentCents += costCents
	if bs.ledger.ByModel == nil {
		bs.ledger.ByModel = map[string]int{}
	}
	if model != "" {
		bs.ledger.ByModel[model] += costCents
	}
	if err := bs.persistFunc(); err != nil {
		// Caller (the daemon) is what logs to audit; we surface to its
		// logger if available. Don't take down the request for a
		// ledger-write error.
		fmt.Fprintf(os.Stderr, "[ai_proxy] persisting ledger failed: %v\n", err)
	}
}

func utcMonth(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month()))
}

// errBudgetExhausted is returned by allow(); the HTTP handler turns
// it into a 429 with a JSON body the app can surface to the customer.
type errBudgetExhausted struct {
	Month       string
	SpentCents  int
	BudgetCents int
}

func (e errBudgetExhausted) Error() string {
	return fmt.Sprintf("monthly AI budget exhausted: spent $%.2f of $%.2f in %s",
		float64(e.SpentCents)/100, float64(e.BudgetCents)/100, e.Month)
}

// ─── Pricing ─────────────────────────────────────────────────────────

// pricePerMillion is the per-Mtoken price in DOLLARS for each model
// we recognize. Unknown models are charged at the most-expensive rate
// we know about — better to over-charge a customer's budget against
// our budget than under-charge and find out at the bank.
//
// These prices reflect Anthropic + OpenAI as of mid-2026. Keep them
// in sync with the providers' published rates; a stale entry here
// either eats our margin or unfairly penalizes a customer's budget.
type modelPricing struct {
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// Keyed on the BARE model alias, consistently. costCents strips a
// trailing -YYYYMMDD snapshot date before lookup, so the alias and the
// dated snapshot — the two shapes Anthropic's Messages API echoes back
// in the response `model` field depending on API version — resolve to
// the same row. Keep every key here in bare-alias form; a dated key
// would only match one of the two shapes and silently mis-price the
// other.
var pricingTable = map[string]modelPricing{
	// Anthropic Claude family
	"claude-sonnet-4-5": {3.00, 15.00},
	"claude-opus-4-5":   {15.00, 75.00},
	"claude-haiku-4-5":  {1.00, 5.00},

	// OpenAI family — kept in same map for one-stop pricing lookup
	"gpt-5.4-mini": {0.75, 4.50},
	"gpt-5.4":      {2.50, 15.00},
	"gpt-5":        {5.00, 15.00},
	"gpt-4o":       {2.50, 10.00},
	"gpt-4o-mini":  {0.15, 0.60},
	"gpt-5-mini":   {0.25, 1.00},

	// Conservative fallback used for unknown model strings.
	"": {15.00, 75.00},
}

// stripSnapshotDateSuffix removes a trailing "-YYYYMMDD" or "-YYYY-MM-DD"
// snapshot date from a model id, so dated snapshots price identically to
// their bare alias ("gpt-5.4-mini-2026-03-17" → "gpt-5.4-mini").
//
// Only an exact date tail is stripped. The earlier
// "first digit after a dash" scan was wrong: it chewed into the
// version part ("-4-5"), collapsing "claude-sonnet-4-5" to
// "claude-sonnet", which is in no table — so the single most common
// model fell through to the Opus-rate fallback and over-charged the
// budget 5x. A precise tail match cannot make that mistake.
func stripSnapshotDateSuffix(model string) string {
	const dashedTail = 11 // len("-2026-03-17")
	if len(model) > dashedTail {
		suf := model[len(model)-dashedTail:]
		if suf[0] == '-' && suf[5] == '-' && suf[8] == '-' {
			ok := true
			for _, idx := range []int{1, 2, 3, 4, 6, 7, 9, 10} {
				if suf[idx] < '0' || suf[idx] > '9' {
					ok = false
					break
				}
			}
			if ok {
				return model[:len(model)-dashedTail]
			}
		}
	}
	const tail = 9 // len("-20250929")
	if len(model) <= tail {
		return model
	}
	suf := model[len(model)-tail:]
	if suf[0] != '-' {
		return model
	}
	for i := 1; i < tail; i++ {
		if suf[i] < '0' || suf[i] > '9' {
			return model
		}
	}
	return model[:len(model)-tail]
}

// costCents calculates whole-cent cost for a call. Rounds UP so we
// never under-account ourselves; a 0-token call costs 0, anything
// non-zero costs at least 1 cent.
func costCents(model string, inputTokens, outputTokens int) int {
	norm := stripSnapshotDateSuffix(strings.ToLower(strings.TrimSpace(model)))
	p, ok := pricingTable[norm]
	if !ok {
		// Unknown model → conservative (Opus-level) rate: over-charge
		// the budget rather than under-charge the platform.
		p = pricingTable[""]
	}
	usd := (float64(inputTokens)*p.InputUSDPerMTok + float64(outputTokens)*p.OutputUSDPerMTok) / 1_000_000.0
	cents := usd * 100
	if cents > 0 && cents < 1 {
		return 1
	}
	return int(cents + 0.999)
}

// ─── Provider usage parsers ──────────────────────────────────────────

// parseAnthropicJSONUsage finds the usage field in a non-streaming
// Anthropic response. Body shape:
//
//	{ "model": "claude-sonnet-4-5-...", "usage": {"input_tokens": N, "output_tokens": M}, ... }
func parseAnthropicJSONUsage(body []byte) (in, out int, model string) {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, ""
	}
	return resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Model
}

// parseAnthropicSSEUsage walks a buffered SSE stream looking for
// message_start (input tokens + model) and message_delta (cumulative
// output tokens). The "stop" usage event has the totals we need.
func parseAnthropicSSEUsage(streamed []byte) (in, out int, model string) {
	lines := strings.Split(string(streamed), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(l, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var evt struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "message_start":
			if evt.Message.Model != "" {
				model = evt.Message.Model
			}
			if evt.Message.Usage.InputTokens > 0 {
				in = evt.Message.Usage.InputTokens
			}
			if evt.Message.Usage.OutputTokens > 0 {
				out = evt.Message.Usage.OutputTokens
			}
		case "message_delta":
			if evt.Usage.OutputTokens > out {
				out = evt.Usage.OutputTokens
			}
			if evt.Usage.InputTokens > in {
				in = evt.Usage.InputTokens
			}
		}
	}
	return
}

// parseOpenAIJSONUsage handles non-streaming OpenAI Chat Completions or
// Responses API bodies. Shapes:
//
//	{ "model": "gpt-...", "usage": {"prompt_tokens":N, "completion_tokens":M}, ... }
//	{ "model": "gpt-...", "usage": {"input_tokens":N, "output_tokens":M}, ... }
func parseOpenAIJSONUsage(body []byte) (in, out int, model string) {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, ""
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		return resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Model
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Model
}

// parseOpenAISSEUsage scans an OpenAI SSE stream for the final usage event.
// It handles BOTH streaming surfaces the proxy forwards to:
//
//   - Chat Completions (/v1/chat/completions): the stream ends with a chunk
//     that has empty choices but a top-level `usage` object.
//
//   - Responses (/v1/responses): usage is NOT top-level. It is nested as
//     `response.usage.{input_tokens,output_tokens}` inside the terminal
//     `response.completed` (or `response.incomplete`) event. Missing this
//     nesting meant every streamed Responses call parsed to in=0/out=0 and
//     was billed $0 against the per-machine ledger, so the budget cap never
//     engaged for that traffic.
//
// Both top-level and nested usage are consulted on every data line; whichever
// the stream provides wins.
func parseOpenAISSEUsage(streamed []byte) (in, out int, model string) {
	lines := strings.Split(string(streamed), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(l, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var evt struct {
			Model string `json:"model"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
			} `json:"usage"`
			// Responses API terminal event: type=response.completed with the
			// full response object (model + usage) nested under `response`.
			Response struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			continue
		}
		if evt.Model != "" {
			model = evt.Model
		} else if evt.Response.Model != "" {
			model = evt.Response.Model
		}
		// Top-level usage (Chat Completions).
		if evt.Usage.InputTokens > 0 {
			in = evt.Usage.InputTokens
		} else if evt.Usage.PromptTokens > 0 {
			in = evt.Usage.PromptTokens
		}
		if evt.Usage.OutputTokens > 0 {
			out = evt.Usage.OutputTokens
		} else if evt.Usage.CompletionTokens > 0 {
			out = evt.Usage.CompletionTokens
		}
		// Nested usage (Responses API). Only override when present so a later
		// non-terminal event can't zero out a value we already captured.
		if evt.Response.Usage.InputTokens > 0 {
			in = evt.Response.Usage.InputTokens
		}
		if evt.Response.Usage.OutputTokens > 0 {
			out = evt.Response.Usage.OutputTokens
		}
	}
	return
}

// ─── HTTP handler ────────────────────────────────────────────────────

// aiProxyHandler builds the http.HandlerFunc for a specific provider.
// One handler per provider is registered in main.go.
func (s *Server) aiProxyHandler(spec providerSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 1. Auth — bearer / x-api-key against the machine-local token.
		presented := requestPresentedToken(r)
		if !s.aiCreditsToken.validate(presented) {
			jsonError(w, "ai credits token required", http.StatusUnauthorized)
			return
		}

		if !s.platformAIProxyEnabled() {
			jsonError(w, "included AI credits proxy is disabled for operator-owned manager key mode", http.StatusForbidden)
			return
		}

		// 2. Budget gate. Atomically admit-and-reserve before we burn
		//    upstream time/$. The reservation is held against the cap for
		//    the whole upstream round-trip and reconciled to the real cost
		//    in settle() below, so concurrent calls can't each slip past a
		//    near-cap budget while the others are still in flight.
		reserved, err := s.budget.reserve()
		if err != nil {
			var ex errBudgetExhausted
			if errors.As(err, &ex) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":         err.Error(),
					"month":         ex.Month,
					"spent_cents":   ex.SpentCents,
					"budget_cents":  ex.BudgetCents,
					"resets_at_utc": "first of next month",
					"hint":          "the included AI budget for this machine is used up for the month; switch to a customer-owned key or upgrade the plan to continue",
				})
				return
			}
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// settled guards against a double-settle on any return path. The
		// reservation MUST be released exactly once, or it permanently pins
		// budget. Charge the real cost only on a successful upstream
		// response (set below); errors settle with cost 0.
		settled := false
		var settleModel string
		var settleCost int
		defer func() {
			if !settled {
				bs := s.budget
				bs.settle(reserved, settleModel, settleCost)
			}
		}()

		// 3. Resolve the upstream path. Our prefix is /api/ai/credits/<provider>;
		//    whatever the app appended after that is what the upstream
		//    expects (e.g. /v1/messages).
		prefix := "/api/ai/credits/" + spec.name
		upstreamPath := strings.TrimPrefix(r.URL.Path, prefix)
		if upstreamPath == "" || upstreamPath[0] != '/' {
			jsonError(w, "missing upstream path; expected /api/ai/credits/"+spec.name+"/v1/...", http.StatusBadRequest)
			return
		}

		// 4. Read the platform's key for this provider. Missing key →
		//    return a clear 503 so the manager (or app developer) knows
		//    this provider isn't configured on this machine.
		key, err := os.ReadFile(spec.keyPath)
		if err != nil || strings.TrimSpace(string(key)) == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "platform key not configured for provider " + spec.name,
				"provider": spec.name,
				"hint":     "this machine doesn't have an included " + spec.name + " key; bring a customer-owned key instead",
			})
			return
		}
		keyVal := strings.TrimSpace(string(key))

		// 5. Build the upstream request. Body is streamed through (no
		//    full buffer) so large prompts don't blow memory.
		var bodyForUpstream io.Reader
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				jsonError(w, "reading request body: "+err.Error(), http.StatusBadRequest)
				return
			}
			bodyForUpstream = bytes.NewReader(body)
			r.Body.Close()
		}
		ureq, err := http.NewRequestWithContext(r.Context(), r.Method, spec.upstreamBase+upstreamPath, bodyForUpstream)
		if err != nil {
			jsonError(w, "constructing upstream request: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// 6. Copy through non-auth headers, then inject the platform
		//    key. Auth-bearing headers from the app are deliberately
		//    NOT copied — never trust the app's claim about which key
		//    to use upstream.
		for h, vals := range r.Header {
			low := strings.ToLower(h)
			if isAuthHeader(low) || isHopHeader(low) {
				continue
			}
			for _, v := range vals {
				ureq.Header.Add(h, v)
			}
		}
		ureq.Header.Set(spec.authHeaderName, fmt.Sprintf(spec.authHeaderFmt, keyVal))
		for k, v := range spec.extraHeaders {
			if ureq.Header.Get(k) == "" {
				ureq.Header.Set(k, v)
			}
		}

		// 7. Fire upstream. The default Transport handles keep-alives
		//    sensibly; we override timeouts via the request context
		//    (already bound to the customer's request lifetime).
		client := &http.Client{
			Timeout: upstreamTotalTimeout,
		}
		resp, err := client.Do(ureq)
		if err != nil {
			s.auditLog.Log(audit.Entry{
				Action: "ai_credits_upstream_error", Category: "ai_credits",
				Details: fmt.Sprintf("provider=%s err=%v", spec.name, err), RiskLevel: "low",
			})
			jsonError(w, "upstream "+spec.name+" unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// 8. Copy upstream headers to the response. Hop-by-hop and
		//    connection-management headers stay with the proxy.
		for h, vals := range resp.Header {
			if isHopHeader(strings.ToLower(h)) {
				continue
			}
			for _, v := range vals {
				w.Header().Add(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// 9. Stream the body to the caller. For SSE we Flush on every
		//    chunk so the AI SDK's stream consumer sees deltas in real
		//    time. For non-SSE we tee through a buffer so we can parse
		//    usage after the response finishes.
		flusher, _ := w.(http.Flusher)
		isStream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

		var inTok, outTok int
		var model string

		if isStream {
			var teeBuf bytes.Buffer
			buf := make([]byte, 16*1024)
			for {
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						break
					}
					teeBuf.Write(buf[:n])
					if flusher != nil {
						flusher.Flush()
					}
				}
				if rerr != nil {
					break
				}
			}
			inTok, outTok, model = spec.parseSSEUsage(teeBuf.Bytes())
		} else {
			body, _ := io.ReadAll(resp.Body)
			_, _ = w.Write(body)
			inTok, outTok, model = spec.parseUsage(body)
		}

		// 10. Reconcile the reservation to the call's real cost. Charge the
		//     ledger only on successful upstream responses — 4xx/5xx upstream
		//     errors aren't billable, so they settle with cost 0 (releasing
		//     the reservation without spending budget). The defer performs
		//     the single settle for every return path.
		if resp.StatusCode < 400 {
			settleModel = model
			settleCost = costCents(model, inTok, outTok)
			// Defensive: a 200 with no parseable usage (e.g. an upstream
			// shape we don't recognize) still consumed platform tokens.
			// Charge a conservative floor rather than billing $0, which
			// would let unparseable streamed traffic bypass the cap.
			if settleCost <= 0 {
				settleCost = 1
			}
			s.auditLog.Log(audit.Entry{
				Action:   "ai_credits_call",
				Category: "ai_credits",
				Details: fmt.Sprintf("provider=%s model=%s input_tokens=%d output_tokens=%d cost_cents=%d",
					spec.name, model, inTok, outTok, settleCost),
				RiskLevel: "low",
			})
		}
	}
}

// isAuthHeader is the set of headers we strip from the incoming
// request before forwarding upstream. The app provided OUR bearer in
// one of these; the upstream needs the PLATFORM's key, which we
// inject after stripping.
func isAuthHeader(low string) bool {
	switch low {
	case "authorization", "x-api-key", "anthropic-api-key", "openai-api-key":
		return true
	}
	return false
}

// isHopHeader is the set of headers that don't survive a proxy hop
// (RFC 7230 §6.1 + a few common extras). Keep them off both the
// upstream request and the proxied response.
func isHopHeader(low string) bool {
	switch low {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
