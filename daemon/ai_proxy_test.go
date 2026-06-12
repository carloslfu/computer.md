// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// ai_proxy_test.go pins the load-bearing behaviors of the usage-credit
// proxy. The whole point of the proxy is that NONE of these can
// silently regress without burning the platform's budget or leaking
// the platform's key.

// ─── Token validation ───────────────────────────────────────────────

func TestAICreditsToken_Validate(t *testing.T) {
	tok := &AICreditsToken{value: "vc-abc123"}
	if !tok.validate("vc-abc123") {
		t.Error("exact match should validate")
	}
	if tok.validate("vc-abc124") {
		t.Error("near-miss must not validate")
	}
	if tok.validate("") {
		t.Error("empty string must not validate")
	}

	// Nil receiver: defensive. A daemon that failed to load its
	// token-file at startup should reject EVERY request, not crash.
	var nilTok *AICreditsToken
	if nilTok.validate("anything") {
		t.Error("nil receiver must not validate anything")
	}

	// Zero-value receiver: same. An uninitialized token field on the
	// Server struct is the failure mode we want to fail closed.
	empty := &AICreditsToken{}
	if empty.validate("anything") {
		t.Error("empty value must not validate anything")
	}
}

func TestRequestPresentedToken_RecognizesAllSDKShapes(t *testing.T) {
	// The Vercel AI SDK / Anthropic SDK / OpenAI SDK each set the
	// auth header differently:
	//   - Anthropic SDK → x-api-key
	//   - OpenAI SDK → Authorization: Bearer <key>
	//   - Vercel AI SDK → either, depending on provider
	// The proxy accepts all of them and pulls the same bearer string
	// out — the customer's app shouldn't care which SDK we're sitting
	// in front of.
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"x-api-key (Anthropic style)", map[string]string{"x-api-key": "vc-abc"}, "vc-abc"},
		{"X-Api-Key (canonical case)", map[string]string{"X-Api-Key": "vc-abc"}, "vc-abc"},
		{"Bearer (OpenAI style)", map[string]string{"Authorization": "Bearer vc-abc"}, "vc-abc"},
		{"raw Authorization", map[string]string{"Authorization": "vc-abc"}, "vc-abc"},
		{"no header", map[string]string{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			for k, v := range c.header {
				r.Header.Set(k, v)
			}
			got := requestPresentedToken(r)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ─── Budget gate ────────────────────────────────────────────────────

func TestBudget_AllowAndCharge(t *testing.T) {
	bs := &budgetService{
		monthlyCap: 100, // $1
		ledger:     aiLedger{Month: utcMonth(time.Now())},
	}
	bs.persistFunc = func() error { return nil } // no disk in tests

	if err := bs.allow(); err != nil {
		t.Errorf("fresh budget should allow: %v", err)
	}

	bs.charge("model-x", 30)
	if bs.ledger.SpentCents != 30 {
		t.Errorf("after charging 30c, spent=%d", bs.ledger.SpentCents)
	}
	if err := bs.allow(); err != nil {
		t.Errorf("30c of 100c budget should still allow: %v", err)
	}

	bs.charge("model-x", 70)
	if bs.ledger.SpentCents != 100 {
		t.Errorf("after charging 70c more, spent=%d", bs.ledger.SpentCents)
	}

	// At-cap: gate. Going exactly at cap blocks the next request
	// (admitting "one more then over" would over-shoot every plan
	// because a single response can cost dollars).
	if err := bs.allow(); err == nil {
		t.Error("at-cap budget must reject (got allow)")
	}
}

func TestBudget_RollOverOnMonthChange(t *testing.T) {
	// A daemon that's been running for >1 month should see the ledger
	// reset on the 1st UTC. We simulate by stuffing an old month into
	// the ledger and calling rollOver — it should reset to current.
	bs := &budgetService{
		monthlyCap: 100,
		ledger: aiLedger{
			Month:      "2024-01",
			SpentCents: 9999,
			ByModel:    map[string]int{"old": 9999},
		},
	}
	bs.persistFunc = func() error { return nil }

	bs.rollOverIfNewMonth()

	if bs.ledger.Month == "2024-01" {
		t.Error("month did not roll over")
	}
	if bs.ledger.SpentCents != 0 {
		t.Errorf("spent_cents not reset after rollover: %d", bs.ledger.SpentCents)
	}
	if len(bs.ledger.ByModel) != 0 {
		t.Errorf("by_model not reset after rollover: %+v", bs.ledger.ByModel)
	}
}

// errBudgetExhausted carries the diagnostic fields we surface to the
// app as JSON. Pin them so the app can rely on the shape.
func TestErrBudgetExhausted_Surface(t *testing.T) {
	e := errBudgetExhausted{Month: "2026-05", SpentCents: 5000, BudgetCents: 5000}
	msg := e.Error()
	for _, want := range []string{"$50.00", "2026-05", "exhausted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error string %q missing %q", msg, want)
		}
	}
}

// ─── Pricing ────────────────────────────────────────────────────────

func TestCostCents_KnownModels(t *testing.T) {
	// OpenAI GPT-5.4 mini: $0.75/M input, $4.50/M output.
	// 1M input + 1M output = $0.75 + $4.50 = $5.25 = 525c.
	if got := costCents("gpt-5.4-mini-2026-03-17", 1_000_000, 1_000_000); got != 525 {
		t.Errorf("GPT-5.4 mini 1M+1M = %d cents, want 525", got)
	}
	// Same model, dated suffix variations should still match and small
	// non-zero calls round up to one cent.
	if got := costCents("gpt-5.4-mini-2026-03-17", 1_000, 500); got < 1 {
		t.Errorf("GPT-5.4 mini small call should round up to >= 1c, got %d", got)
	}
}

func TestCostCents_UnknownModelFallsBackConservatively(t *testing.T) {
	// An unknown model string falls back to the most expensive
	// (Opus-level) rate — better to over-charge the platform's
	// budget than under-charge.
	known := costCents("claude-opus-4-5", 1_000_000, 1_000_000) // $15 + $75 = 9000c
	fallback := costCents("totally-made-up-model", 1_000_000, 1_000_000)
	if fallback != known {
		t.Errorf("unknown model should charge Opus-rate (%d), got %d", known, fallback)
	}
}

func TestCostCents_ZeroTokensIsZero(t *testing.T) {
	if got := costCents("claude-sonnet-4-5-20250929", 0, 0); got != 0 {
		t.Errorf("0+0 tokens should cost 0, got %d", got)
	}
}

// TestCostCents_BareAliasAndDatedPriceIdentically is the regression
// guard for the pricing-table key bug. Anthropic's Messages API echoes
// back EITHER the bare alias ("claude-sonnet-4-5") OR the dated
// snapshot ("claude-sonnet-4-5-20250929") in the response `model`
// field, depending on API version. The proxy charges off whatever it
// gets, so both shapes MUST price the same.
//
// They didn't: the table was keyed on the dated Sonnet id but the bare
// Opus/Haiku ids, and the date-stripping scan collapsed "-4-5" along
// with the date — so "claude-sonnet-4-5" resolved to the Opus-rate
// fallback and over-charged the customer's budget 5x on the single
// most common model.
func TestCostCents_BareAliasAndDatedPriceIdentically(t *testing.T) {
	pairs := []struct{ alias, dated string }{
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929"},
		{"claude-opus-4-5", "claude-opus-4-5-20251101"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251015"},
		{"gpt-5.4-mini", "gpt-5.4-mini-2026-03-17"},
	}
	for _, p := range pairs {
		a := costCents(p.alias, 1_000_000, 1_000_000)
		d := costCents(p.dated, 1_000_000, 1_000_000)
		if a != d {
			t.Errorf("%s = %dc but %s = %dc — alias and dated snapshot must price identically",
				p.alias, a, p.dated, d)
		}
	}

	// The bare Sonnet alias must price at the real Sonnet rate, NOT the
	// Opus-level fallback. 1M input only: Sonnet $3 = 300c, Opus $15 = 1500c.
	sonnet := costCents("claude-sonnet-4-5", 1_000_000, 0)
	if sonnet != 300 {
		t.Errorf("bare Sonnet alias = %dc for 1M input, want 300c ($3/Mtok) — was it resolving to the Opus fallback?", sonnet)
	}
}

// TestStripSnapshotDateSuffix pins the tail-matcher: it must strip real
// snapshot dates and nothing else. In particular it must NOT touch the
// "-4-5" version part of a bare alias.
func TestStripSnapshotDateSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5"}, // strip real date
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},          // bare alias untouched
		{"claude-opus-4-5", "claude-opus-4-5"},              // version part survives
		{"gpt-5.4-mini-2026-03-17", "gpt-5.4-mini"},         // OpenAI snapshot
		{"gpt-4o", "gpt-4o"},                                // short id untouched
		{"gpt-4o-mini", "gpt-4o-mini"},                      // non-date tail untouched
		{"claude-x-2025092", "claude-x-2025092"},            // 7 digits ≠ date, keep
		{"claude-x-202509299", "claude-x-202509299"},        // 9 digits ≠ date, keep
	}
	for _, c := range cases {
		if got := stripSnapshotDateSuffix(c.in); got != c.want {
			t.Errorf("stripSnapshotDateSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─── Provider parsers ──────────────────────────────────────────────

func TestParseAnthropicJSONUsage(t *testing.T) {
	body := []byte(`{
		"id":"msg_01",
		"model":"claude-sonnet-4-5-20250929",
		"role":"assistant",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":12,"output_tokens":3}
	}`)
	in, out, model := parseAnthropicJSONUsage(body)
	if in != 12 || out != 3 || model != "claude-sonnet-4-5-20250929" {
		t.Errorf("got in=%d out=%d model=%q", in, out, model)
	}
}

func TestParseAnthropicSSEUsage(t *testing.T) {
	// Real-shaped Anthropic SSE stream — message_start with
	// input_tokens, message_delta with final output_tokens.
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":42,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	in, out, model := parseAnthropicSSEUsage([]byte(stream))
	if in != 42 {
		t.Errorf("input_tokens = %d, want 42", in)
	}
	if out != 17 {
		t.Errorf("output_tokens = %d, want 17", out)
	}
	if model != "claude-sonnet-4-5-20250929" {
		t.Errorf("model = %q, want claude-sonnet-4-5-20250929", model)
	}
}

func TestParseOpenAIJSONUsage(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-01","object":"chat.completion","model":"gpt-4o-mini",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}
	}`)
	in, out, model := parseOpenAIJSONUsage(body)
	if in != 11 || out != 4 || model != "gpt-4o-mini" {
		t.Errorf("got in=%d out=%d model=%q", in, out, model)
	}
}

func TestParseOpenAIResponsesJSONUsage(t *testing.T) {
	body := []byte(`{
		"id":"resp_01","object":"response","model":"gpt-5.4-mini",
		"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],
		"usage":{"input_tokens":21,"output_tokens":8,"total_tokens":29}
	}`)
	in, out, model := parseOpenAIJSONUsage(body)
	if in != 21 || out != 8 || model != "gpt-5.4-mini" {
		t.Errorf("got in=%d out=%d model=%q", in, out, model)
	}
}

// ─── Auth/hop header taxonomies ──────────────────────────────────────

func TestIsAuthHeader(t *testing.T) {
	// These MUST be stripped from incoming requests. If any of them
	// slipped through, an app could supply a key the daemon would
	// accidentally forward, defeating the whole point of the proxy.
	mustStrip := []string{"authorization", "x-api-key", "anthropic-api-key", "openai-api-key"}
	for _, h := range mustStrip {
		if !isAuthHeader(h) {
			t.Errorf("must-strip header %q not classified as auth", h)
		}
	}
	// Headers we want to pass through (Anthropic-version, content-type, etc.).
	mustPass := []string{"anthropic-version", "content-type", "user-agent", "accept"}
	for _, h := range mustPass {
		if isAuthHeader(h) {
			t.Errorf("non-auth header %q wrongly classified as auth", h)
		}
	}
}

func TestIsHopHeader(t *testing.T) {
	// RFC 7230 §6.1 hop-by-hop headers + the usual extras.
	for _, h := range []string{"connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade"} {
		if !isHopHeader(h) {
			t.Errorf("hop header %q not detected", h)
		}
	}
	if isHopHeader("content-type") {
		t.Error("content-type wrongly classified as hop")
	}
}

// ─── End-to-end handler test ─────────────────────────────────────────

// TestAIProxyHandler_ForwardsAndCharges spins up a fake upstream that
// stands in for api.anthropic.com, points the proxy at it, and walks
// the full round-trip: token validates, headers swapped, body
// forwarded, response streamed back, ledger charged.
//
// This is the test that would catch a regression where the proxy
// stops doing one of: stripping the app's auth header, injecting
// the platform key, charging the ledger, or returning the upstream
// body intact. Each of those would be a customer-visible failure
// (or a platform key leak) in production.
func TestAIProxyHandler_ForwardsAndCharges(t *testing.T) {
	platformKeyFile := t.TempDir() + "/anthropic.key"
	if err := writeTempFile(platformKeyFile, "PLATFORM_KEY_VALUE"); err != nil {
		t.Fatal(err)
	}

	// Capture the upstream-side request so we can assert on what the
	// proxy actually forwarded.
	var gotAuth, gotXApiKey, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXApiKey = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_01","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":20}}`)
	}))
	defer upstream.Close()

	spec := providerSpec{
		name:           "anthropic",
		upstreamBase:   upstream.URL,
		authHeaderName: "x-api-key",
		authHeaderFmt:  "%s",
		keyPath:        platformKeyFile,
		extraHeaders:   map[string]string{"anthropic-version": "2023-06-01"},
		parseUsage:     parseAnthropicJSONUsage,
		parseSSEUsage:  parseAnthropicSSEUsage,
	}

	srv := mustNewServerForAIProxy(t, "vc-test-tok", 10000)
	h := srv.aiProxyHandler(spec)

	body := `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/api/ai/credits/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "vc-test-tok") // app sets bearer in x-api-key
	req = req.WithContext(context.Background())

	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handler returned %d: %s", w.Code, w.Body.String())
	}
	// Body intact through the proxy.
	if !strings.Contains(w.Body.String(), `"input_tokens":10`) {
		t.Errorf("upstream body not preserved: %q", w.Body.String())
	}

	// Critical: the app's bearer (vc-...) MUST be stripped and the
	// PLATFORM key injected before forwarding. If gotXApiKey still
	// reads "vc-..." we've forwarded the customer's bearer upstream;
	// Anthropic would 401, but worse, we'd have leaked the bearer.
	if gotXApiKey != "PLATFORM_KEY_VALUE" {
		t.Errorf("upstream x-api-key = %q, want PLATFORM_KEY_VALUE (the app's bearer must be stripped, platform key injected)", gotXApiKey)
	}
	if gotAuth != "" && strings.Contains(gotAuth, "vc-test-tok") {
		t.Errorf("upstream Authorization carried the app bearer through: %q", gotAuth)
	}

	// Body must round-trip unchanged.
	if !strings.Contains(gotBody, `claude-sonnet-4-5-20250929`) {
		t.Errorf("upstream request body lost the model field: %q", gotBody)
	}

	// Ledger should have been charged after the response. Sonnet
	// 10in + 20out = (10 * $3 + 20 * $15) / 1M * 100c = ~0.0033c
	// → rounds up to 1c per costCents() rule.
	if srv.budget.ledger.SpentCents < 1 {
		t.Errorf("ledger did not charge for the call: spent=%d", srv.budget.ledger.SpentCents)
	}
}

func TestAIProxyHandler_RejectsBadToken(t *testing.T) {
	srv := mustNewServerForAIProxy(t, "vc-real", 10000)
	h := srv.aiProxyHandler(anthropicSpec)

	req := httptest.NewRequest("POST", "/api/ai/credits/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "vc-WRONG")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token → %d, want 401", w.Code)
	}
}

func TestAIProxyHandler_DisabledForOperatorKeyMode(t *testing.T) {
	srv := mustNewServerForAIProxy(t, "vc-tok", 10000)
	srv.cfg.ManagerKeyMode = "operator"
	h := srv.aiProxyHandler(openaiSpec)

	req := httptest.NewRequest("POST", "/api/ai/credits/openai/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer vc-tok")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("operator key mode → %d, want 403: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "operator-owned") {
		t.Fatalf("403 should explain operator-owned mode: %s", w.Body.String())
	}
}

func TestAIProxyHandler_429WhenBudgetExhausted(t *testing.T) {
	srv := mustNewServerForAIProxy(t, "vc-tok", 100)
	srv.budget.ledger.SpentCents = 100 // already at cap
	h := srv.aiProxyHandler(anthropicSpec)

	req := httptest.NewRequest("POST", "/api/ai/credits/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "vc-tok")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("budget exhausted → %d, want 429", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if _, ok := resp["spent_cents"]; !ok {
		t.Error("429 response missing spent_cents")
	}
	if _, ok := resp["budget_cents"]; !ok {
		t.Error("429 response missing budget_cents")
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────

func writeTempFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// mustNewServerForAIProxy returns a Server with just the fields the
// AI proxy handler touches: token, budget, audit logger. Sidesteps
// the full daemon-startup dance from auth_test's newTestServer for
// scope reasons — the handler doesn't need a DB, route manager,
// claude client, or any of that.
func mustNewServerForAIProxy(t *testing.T, token string, monthlyBudgetCents int) *Server {
	t.Helper()
	// Audit logger needs a DB, even if we never read its rows back.
	tmp := t.TempDir()
	encKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	db, err := persistence.Open(filepath.Join(tmp, "test.db"), encKey)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := &Server{
		auditLog: audit.NewLogger(db),
		cfg:      &Config{ManagerKeyMode: "platform"},
	}
	srv.aiCreditsToken = &AICreditsToken{value: token}
	srv.budget = &budgetService{
		monthlyCap:  monthlyBudgetCents,
		ledger:      aiLedger{Month: utcMonth(time.Now())},
		persistFunc: func() error { return nil },
	}
	return srv
}
