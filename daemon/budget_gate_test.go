// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/core"
	"github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/usage"
)

// Compile-time proof of the engine↔budget join: the BudgetTracker is
// what main.go wires into the engine / compactor / summarizer as their
// UsageRecorder. If the interface or the method signature ever drifts,
// this line fails the build instead of failing silently in prod.
var _ core.UsageRecorder = (*usage.BudgetTracker)(nil)

// fakePlatformBudget stands in for GET /api/machine/budget so the gate
// tests drive the daemon's budget through its REAL fetch path.
func fakePlatformBudget(t *testing.T, budgetUSD float64) *httptest.Server {
	t.Helper()
	periodStart, periodEnd := usage.CurrentMonthBounds()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ai_budget_usd": budgetUSD,
			"period_start":  periodStart,
			"period_end":    periodEnd,
			"period_source": "subscription",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newBudgetGateTestServer builds a daemon Server with the deps
// handleTask's budget gate needs: real DB-backed taskStore, usage
// store, budget tracker, audit logger, SSE broker. The tracker is
// pointed at a fake platform serving budgetUSD; budgetUSD < 0 means
// "no Refresh" (the fail-open case — tracker never learns a budget).
func newBudgetGateTestServer(t *testing.T, budgetUSD float64, doRefresh bool) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := persistence.Open(filepath.Join(dir, "test.db"), "test-key-32chars-XXXXXXXXXXXXXXXX")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store := usage.NewStore(db)
	platform := fakePlatformBudget(t, budgetUSD)
	bt := usage.NewBudgetTracker(store, "vc-test", "tok", platform.URL)
	if doRefresh {
		if err := bt.Refresh(context.Background()); err != nil {
			t.Fatalf("budget Refresh: %v", err)
		}
	}

	return &Server{
		db:            db,
		taskStore:     core.NewTaskStore(db),
		auditLog:      audit.NewLogger(db),
		broker:        NewSSEBroker(),
		usageStore:    store,
		budgetTracker: bt,
	}
}

func postTask(t *testing.T, srv *Server, convID, instruction string) (int, map[string]interface{}) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"instruction":     instruction,
		"conversation_id": convID,
	})
	req := httptest.NewRequest("POST", "/api/task", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyAccess, "control"))
	w := httptest.NewRecorder()
	srv.handleTask(w, req)
	var parsed map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	return w.Code, parsed
}

// TestBudgetGate_BlocksTaskWhenOverBudget — the daemon fetched a $1
// budget from the (fake) platform; $5 is already spent; a new task
// submission must be held: no task created, budget_blocked:true, and
// the user+manager exchange persisted to the conversation.
func TestBudgetGate_BlocksTaskWhenOverBudget(t *testing.T) {
	srv := newBudgetGateTestServer(t, 1.0, true)

	// Spend ~$5 against the $1 budget.
	if err := srv.usageStore.Record("gpt-5.4-mini", "seed", manager.Usage{
		OutputTokens: 333_333, // ~$1.50 at $4.50/MTok
	}); err != nil {
		t.Fatalf("seed spend: %v", err)
	}

	code, resp := postTask(t, srv, "convo-1", "build me a dashboard")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp["budget_blocked"] != true {
		t.Fatalf("expected budget_blocked:true, got %+v", resp)
	}
	if msg, _ := resp["message"].(string); msg == "" {
		t.Errorf("expected a non-empty manager message")
	}

	tasks, _ := srv.taskStore.ListRecent(10)
	if len(tasks) != 0 {
		t.Errorf("a budget-blocked submission must NOT create a task, got %d", len(tasks))
	}

	msgs, err := srv.taskStore.GetMessages("convo-1")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("expected [user, assistant] persisted messages, got %d: %+v", len(msgs), msgs)
	}
}

func TestManagerUnavailableGate_BlocksTaskWithoutCreatingWork(t *testing.T) {
	srv := newBudgetGateTestServer(t, 200.0, true)
	srv.cfg = &Config{
		ManagerKeyMode:           "operator",
		ManagerUnavailableReason: "This connected computer needs your OpenAI API key before the manager can run.",
	}

	code, resp := postTask(t, srv, "convo-missing-key", "check the server")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp["manager_unavailable"] != true {
		t.Fatalf("expected manager_unavailable:true, got %+v", resp)
	}
	if msg, _ := resp["message"].(string); strings.Contains(msg, "/etc/") || !strings.Contains(msg, "OpenAI API key") {
		t.Fatalf("expected friendly setup message, got %q", msg)
	}
	setup, ok := resp["setup_required"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected structured setup_required payload, got %+v", resp["setup_required"])
	}
	if setup["code"] != setupCodeOperatorOpenAIKey {
		t.Fatalf("setup code = %v", setup["code"])
	}

	tasks, _ := srv.taskStore.ListRecent(10)
	if len(tasks) != 0 {
		t.Fatalf("manager-unavailable submission must not create a task, got %d", len(tasks))
	}

	msgs, err := srv.taskStore.GetMessages("convo-missing-key")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[1].Type != "setup_request" {
		t.Fatalf("expected [user, assistant] persisted messages, got %d: %+v", len(msgs), msgs)
	}
	if strings.Contains(msgs[1].Content, "/etc/vibecraft/openai.key") {
		t.Fatalf("setup card must not expose daemon file paths: %s", msgs[1].Content)
	}
}

func TestManagerUnavailableGate_AnswersLocalAgentFacts(t *testing.T) {
	srv := newBudgetGateTestServer(t, 200.0, true)
	srv.cfg = &Config{
		ManagerKeyMode:           "operator",
		ManagerUnavailableReason: "This connected computer needs your OpenAI API key before the manager can run.",
	}

	code, resp := postTask(t, srv, "convo-local-fact", "Is Codex installed here?")
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if resp["manager_unavailable"] != true {
		t.Fatalf("expected manager_unavailable:true, got %+v", resp)
	}
	if answer, _ := resp["local_answer"].(string); !strings.Contains(strings.ToLower(answer), "codex") {
		t.Fatalf("expected local Codex answer, got %q", answer)
	}

	msgs, err := srv.taskStore.GetMessages("convo-local-fact")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 3 || msgs[1].Type != "text" || msgs[2].Type != "setup_request" {
		t.Fatalf("expected user, local answer, setup card; got %+v", msgs)
	}
}

// TestBudgetGate_AllowsTaskUnderBudget — fetched a generous budget,
// spend is well under it: the task is created normally.
func TestBudgetGate_AllowsTaskUnderBudget(t *testing.T) {
	srv := newBudgetGateTestServer(t, 200.0, true)
	srv.usageStore.Record("gpt-5.4-mini", "seed", manager.Usage{OutputTokens: 1000})

	code, resp := postTask(t, srv, "convo-1", "build me a dashboard")
	if code != 201 {
		t.Fatalf("expected 201 Created, got %d (resp=%+v)", code, resp)
	}
	if resp["budget_blocked"] == true {
		t.Errorf("under-budget task must not be budget_blocked")
	}
}

// TestBudgetGate_FailOpenWhenBudgetUnknown — the daemon never managed
// to fetch a budget (no Refresh). Tasks must flow — a platform outage
// cannot wedge the machine.
func TestBudgetGate_FailOpenWhenBudgetUnknown(t *testing.T) {
	srv := newBudgetGateTestServer(t, 1.0, false) // server exists, but no Refresh
	srv.usageStore.Record("gpt-5.4-mini", "seed", manager.Usage{
		OutputTokens: 99_999_999,
	})

	code, resp := postTask(t, srv, "convo-1", "do the thing")
	if code != 201 {
		t.Fatalf("fail-open: expected 201, got %d (resp=%+v)", code, resp)
	}
}

// TestBudgetGate_EnterpriseNeverBlocked — a fetched budget of -1
// (Enterprise / unmetered) never blocks, regardless of spend.
func TestBudgetGate_EnterpriseNeverBlocked(t *testing.T) {
	srv := newBudgetGateTestServer(t, -1.0, true)
	srv.usageStore.Record("gpt-5.4-mini", "seed", manager.Usage{
		OutputTokens: 99_999_999,
	})

	code, _ := postTask(t, srv, "convo-1", "do the thing")
	if code != 201 {
		t.Fatalf("Enterprise unmetered must never be blocked, got %d", code)
	}
}

// TestBudgetGate_FullJoin walks the entire enforcement chain in one
// test: fake platform → daemon Refresh → over-budget usage → real
// handleTask gate → /api/usage budget_state. If any join is wrong
// (fetch, parse, cache, State, gate, the budget_state serialization)
// one of these assertions fails.
func TestBudgetGate_FullJoin(t *testing.T) {
	srv := newBudgetGateTestServer(t, 5.0, true) // platform says $5

	// Burn past $5.
	if err := srv.usageStore.Record("gpt-5.4-mini", "seed", manager.Usage{
		OutputTokens: 1_200_000, // ~$5.40 at $4.50/MTok
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The gate must hold a new task.
	code, resp := postTask(t, srv, "convo-1", "deploy the app")
	if code != 200 || resp["budget_blocked"] != true {
		t.Fatalf("over-budget task should be held; got code=%d resp=%+v", code, resp)
	}

	// /api/usage must report the same verdict the gate acted on.
	ureq := httptest.NewRequest("GET", "/api/usage", nil)
	uw := httptest.NewRecorder()
	srv.handleAIUsage(uw, ureq)
	if uw.Code != 200 {
		t.Fatalf("/api/usage: expected 200, got %d", uw.Code)
	}
	var summary struct {
		BudgetState *usage.BudgetState `json:"budget_state"`
	}
	if err := json.Unmarshal(uw.Body.Bytes(), &summary); err != nil {
		t.Fatalf("/api/usage body not parseable: %v", err)
	}
	if summary.BudgetState == nil {
		t.Fatalf("/api/usage must carry budget_state")
	}
	if !summary.BudgetState.Enforced {
		t.Errorf("budget_state.enforced should be true (a $5 budget was fetched)")
	}
	if !summary.BudgetState.Paused {
		t.Errorf("budget_state.paused should be true (~$6 spent over $5)")
	}
	if summary.BudgetState.BudgetUSD != 5 {
		t.Errorf("budget_state.budget_usd should be the fetched $5, got %.2f", summary.BudgetState.BudgetUSD)
	}
}
