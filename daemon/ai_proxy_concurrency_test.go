// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ai_proxy_concurrency_test.go pins the two budget-correctness fixes:
//
//  1. The cap holds under concurrency. Reserve+settle around the upstream
//     call means N parallel requests at a near-cap budget can no longer all
//     pass the gate while each other's spend is still in flight.
//
//  2. Streamed OpenAI Responses-API usage is counted. The terminal
//     response.completed event nests usage under `response.usage`; missing
//     it billed those calls $0 and the cap never engaged.

// TestParseOpenAISSEUsage_ResponsesAPI_NestedUsage is the unit-level red→green
// for the under-counting bug. A streamed /v1/responses call reports usage
// nested as response.usage inside the response.completed event, NOT top-level.
// The old parser only read a top-level usage object, so it returned 0/0 and
// the call was never charged.
func TestParseOpenAISSEUsage_ResponsesAPI_NestedUsage(t *testing.T) {
	// Real-shaped OpenAI Responses API SSE stream. Usage lands only in the
	// terminal response.completed event, nested under "response".
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4-mini","usage":null}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hel"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4-mini","usage":{"input_tokens":321,"output_tokens":654,"total_tokens":975}}}`,
		``,
	}, "\n")

	in, out, model := parseOpenAISSEUsage([]byte(stream))
	if in != 321 {
		t.Errorf("input_tokens = %d, want 321 (nested response.usage.input_tokens ignored?)", in)
	}
	if out != 654 {
		t.Errorf("output_tokens = %d, want 654 (nested response.usage.output_tokens ignored?)", out)
	}
	if model != "gpt-5.4-mini" {
		t.Errorf("model = %q, want gpt-5.4-mini", model)
	}
}

// TestParseOpenAISSEUsage_ChatCompletions_StillWorks guards the
// non-regression: the Chat Completions top-level usage path must keep
// working after adding the nested Responses handling.
func TestParseOpenAISSEUsage_ChatCompletions_StillWorks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"gpt-4o-mini","choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"id":"c1","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	in, out, model := parseOpenAISSEUsage([]byte(stream))
	if in != 11 || out != 4 || model != "gpt-4o-mini" {
		t.Errorf("chat completions stream: got in=%d out=%d model=%q, want 11/4/gpt-4o-mini", in, out, model)
	}
}

// TestAIProxyHandler_ChargesStreamedResponsesUsage is the handler-level
// red→green: a streamed Responses call must decrement the ledger. On the
// buggy parser the call parses to 0/0, the charge is skipped, and the ledger
// stays at 0 — letting streamed traffic bypass the cap entirely.
func TestAIProxyHandler_ChargesStreamedResponsesUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4-mini"}}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4-mini","usage":{"input_tokens":1000000,"output_tokens":1000000}}}`,
			``,
		}, "\n")))
	}))
	defer upstream.Close()

	keyFile := t.TempDir() + "/openai.key"
	if err := writeTempFile(keyFile, "PLATFORM_OPENAI_KEY"); err != nil {
		t.Fatal(err)
	}
	spec := providerSpec{
		name:           "openai",
		upstreamBase:   upstream.URL,
		authHeaderName: "Authorization",
		authHeaderFmt:  "Bearer %s",
		keyPath:        keyFile,
		parseUsage:     parseOpenAIJSONUsage,
		parseSSEUsage:  parseOpenAISSEUsage,
	}

	srv := mustNewServerForAIProxy(t, "vc-tok", 1_000_000)
	h := srv.aiProxyHandler(spec)

	req := httptest.NewRequest("POST", "/api/ai/credits/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4-mini","stream":true,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vc-tok")
	req = req.WithContext(context.Background())
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handler returned %d: %s", w.Code, w.Body.String())
	}
	// gpt-5.4-mini: $0.75/M in + $4.50/M out. 1M+1M = 525c.
	if got := srv.budget.ledger.SpentCents; got != 525 {
		t.Errorf("streamed Responses call charged %d cents, want 525 — nested usage not counted?", got)
	}
}

// TestBudget_CapHoldsUnderConcurrency is the core race fix. With the old
// allow()/charge() split (no lock held across the upstream call) N callers
// at a near-cap budget all passed allow() and then all charged, overspending
// the cap by ~N×cost. With reserve()/settle(), once committed+reserved spend
// reaches the cap no further caller is admitted, so total committed spend can
// exceed the cap by at most one reserveCents worth of real cost.
//
// Run with -race.
func TestBudget_CapHoldsUnderConcurrency(t *testing.T) {
	// Cap sized as a large multiple of the worst-case reservation so many
	// calls can be in flight at once — that's the window the old
	// allow()/charge() split left unguarded.
	const cap = 50 * reserveCents
	bs := &budgetService{
		monthlyCap:  cap,
		ledger:      aiLedger{Month: utcMonth(time.Now())},
		persistFunc: func() error { return nil },
	}

	// Each admitted call's REAL cost equals the worst-case reservation, so a
	// correct gate admits ~cap/reserveCents = 50 calls. The buggy model
	// admitted far more: every caller that raced through allow() between the
	// gate and the charge committed its full cost, overspending the cap by
	// roughly (concurrent in-flight) × cost.
	const callers = 500
	const perCallCost = reserveCents

	var admitted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reserved, err := bs.reserve()
			if err != nil {
				return
			}
			atomic.AddInt64(&admitted, 1)
			// Hold the reservation briefly to force overlap — this is the
			// window the old code left unguarded.
			time.Sleep(2 * time.Millisecond)
			bs.settle(reserved, "model-x", perCallCost)
		}()
	}
	close(start)
	wg.Wait()

	if bs.reservedCents != 0 {
		t.Errorf("reservedCents = %d after all settle(), want 0 (reservation leak)", bs.reservedCents)
	}
	// The invariant: at every instant committed+reserved < cap + reserveCents,
	// so final committed spend can exceed the cap by at most one in-flight
	// reservation's worth. The old code had NO such bound.
	maxAllowed := cap + reserveCents
	if bs.ledger.SpentCents > maxAllowed {
		t.Errorf("committed spend %d exceeds cap+reserveCents (%d) — cap did not hold under concurrency; admitted=%d",
			bs.ledger.SpentCents, maxAllowed, admitted)
	}
	// We must have admitted a meaningful number (the gate isn't dead), but
	// far fewer than every caller — proof the reservation actually gated.
	if admitted < 40 {
		t.Errorf("admitted only %d calls — gate over-rejected", admitted)
	}
	if admitted >= callers {
		t.Errorf("admitted all %d callers — reservation gate did nothing", admitted)
	}
}

// TestBudget_ReserveBlocksWhenReservationsFillCap proves the gate counts
// in-flight reservations, not just committed spend. A single reserveCents
// reservation against a tight cap must block the next reserve() even though
// committed SpentCents is still 0 — that is exactly the near-cap window the
// old allow() (committed-only) let concurrent callers slip through.
func TestBudget_ReserveBlocksWhenReservationsFillCap(t *testing.T) {
	bs := &budgetService{
		monthlyCap:  reserveCents, // exactly one reservation worth of room
		ledger:      aiLedger{Month: utcMonth(time.Now())},
		persistFunc: func() error { return nil },
	}
	// First reservation admits and consumes the whole cap as in-flight hold.
	r1, err := bs.reserve()
	if err != nil {
		t.Fatalf("first reserve should admit: %v", err)
	}
	// Second reservation MUST be rejected: committed(0)+reserved(reserveCents)
	// already equals the cap. The buggy committed-only gate would admit it.
	if _, err := bs.reserve(); err == nil {
		t.Error("second reserve admitted while an equal-cap reservation was in flight — cap bypassable")
	}
	// After the first settles cheaply, room frees up again.
	bs.settle(r1, "m", 1)
	if _, err := bs.reserve(); err != nil {
		t.Errorf("reserve should admit again after the in-flight hold settled: %v", err)
	}
}
