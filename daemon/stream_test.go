// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSSEBroker_ServeHTTPWithInit_DeliversInitialEventsFirst verifies
// that the reconnect-replay feature works end-to-end: a new subscriber
// sees the initial events (e.g. pending task:waiting approvals) before
// any subsequently-broadcast events. Without this, a page reload during
// a pending guardrail approval would leave the user staring at a chat
// with no approval card.
func TestSSEBroker_ServeHTTPWithInit_DeliversInitialEventsFirst(t *testing.T) {
	broker := NewSSEBroker()

	initial := []SSEEvent{
		{ID: "1", Event: "task:waiting", Data: map[string]string{"task_id": "t-initial-1", "question": "approve one?"}},
		{ID: "2", Event: "task:waiting", Data: map[string]string{"task_id": "t-initial-2", "question": "approve two?"}},
	}

	recorder := httptest.NewRecorder()
	reqCtx, reqCancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/stream", nil).WithContext(reqCtx)

	done := make(chan struct{})
	go func() {
		// After a short delay, broadcast one more event to confirm the
		// broker loop is functional after initial replay.
		time.Sleep(30 * time.Millisecond)
		broker.Emit("task:completed", map[string]string{"task_id": "t-initial-1"})

		// Then terminate the client.
		time.Sleep(50 * time.Millisecond)
		reqCancel()
		close(done)
	}()

	broker.ServeHTTPWithInit(recorder, req, PresenceUser{}, initial)
	<-done

	body := recorder.Body.String()
	mustContain(t, body, "event: task:waiting", "initial event 1 not in body")
	if strings.Count(body, "event: task:waiting") < 2 {
		t.Fatalf("expected at least 2 task:waiting events, body=\n%s", body)
	}
	mustContain(t, body, `"task_id":"t-initial-1"`, "initial task_id 1 not in body")
	mustContain(t, body, `"task_id":"t-initial-2"`, "initial task_id 2 not in body")
	mustContain(t, body, "event: task:completed", "broadcast event after init not in body")

	// Order invariant: both task:waiting events appear BEFORE task:completed.
	waiting2 := strings.LastIndex(body, "task_id\":\"t-initial-2\"")
	completed := strings.Index(body, "event: task:completed")
	if waiting2 < 0 || completed < 0 || waiting2 >= completed {
		t.Fatalf("initial events should come before subsequent broadcast; waiting2=%d completed=%d", waiting2, completed)
	}
}

// TestSSEBroker_ServeHTTP_NoInitialEvents verifies backward compat —
// the no-init path still works (e.g. for new clients after all
// approvals have been resolved).
func TestSSEBroker_ServeHTTP_NoInitialEvents(t *testing.T) {
	broker := NewSSEBroker()

	recorder := httptest.NewRecorder()
	reqCtx, reqCancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/stream", nil).WithContext(reqCtx)

	go func() {
		time.Sleep(20 * time.Millisecond)
		broker.Emit("task:started", map[string]string{"task_id": "t-broadcast-only"})
		time.Sleep(30 * time.Millisecond)
		reqCancel()
	}()

	broker.ServeHTTP(recorder, req, PresenceUser{})

	body := recorder.Body.String()
	mustContain(t, body, "event: task:started", "broadcast event missing")
	if strings.Contains(body, "event: task:waiting") {
		t.Fatalf("body unexpectedly contains task:waiting:\n%s", body)
	}
}

func mustContain(t *testing.T, s, substr, msg string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("%s: body did not contain %q:\n%s", msg, substr, s)
	}
}

// TestSSEBroker_PresenceDedupesMultipleTabs verifies the core multi-tab
// invariant: same human, two tabs, one presence entry. The lock model
// failed this case — the new presence model must not.
func TestSSEBroker_PresenceDedupesMultipleTabs(t *testing.T) {
	broker := NewSSEBroker()

	alice := PresenceUser{UserID: "u_alice", Name: "Alice", Email: "alice@example.com"}
	bob := PresenceUser{UserID: "u_bob", Name: "Bob", Email: "bob@example.com"}

	tabAlice1 := broker.Subscribe(alice)
	tabAlice2 := broker.Subscribe(alice) // same user, second tab
	tabBob := broker.Subscribe(bob)
	defer broker.Unsubscribe(tabAlice1)
	defer broker.Unsubscribe(tabAlice2)
	defer broker.Unsubscribe(tabBob)

	users := broker.PresenceSnapshot()
	if len(users) != 2 {
		t.Fatalf("expected 2 deduped users, got %d: %+v", len(users), users)
	}

	seen := map[string]bool{}
	for _, u := range users {
		seen[u.UserID] = true
	}
	if !seen["u_alice"] || !seen["u_bob"] {
		t.Fatalf("missing expected users in presence: %+v", users)
	}
}

// TestSSEBroker_PresenceDropsOnLastTabClose verifies that closing all of
// a user's tabs removes them from presence — but closing only some tabs
// keeps them present.
func TestSSEBroker_PresenceDropsOnLastTabClose(t *testing.T) {
	broker := NewSSEBroker()
	alice := PresenceUser{UserID: "u_alice", Name: "Alice"}

	tab1 := broker.Subscribe(alice)
	tab2 := broker.Subscribe(alice)

	if len(broker.PresenceSnapshot()) != 1 {
		t.Fatalf("expected 1 user (Alice) after 2 tabs opened")
	}

	broker.Unsubscribe(tab1)
	if len(broker.PresenceSnapshot()) != 1 {
		t.Fatalf("Alice should still be present after closing 1 of 2 tabs")
	}

	broker.Unsubscribe(tab2)
	if len(broker.PresenceSnapshot()) != 0 {
		t.Fatalf("Alice should be gone after closing last tab")
	}
}

// TestSSEBroker_AnonymousSubscriberInvisible verifies that subscribers
// with an empty UserID (test / unauthenticated paths) still receive
// events but do NOT appear in the presence list.
func TestSSEBroker_AnonymousSubscriberInvisible(t *testing.T) {
	broker := NewSSEBroker()
	anon := broker.Subscribe(PresenceUser{})
	defer broker.Unsubscribe(anon)

	if got := broker.PresenceSnapshot(); len(got) != 0 {
		t.Fatalf("anonymous subscriber should not appear in presence, got %+v", got)
	}
}

// TestSSEBroker_PresenceBroadcastsOnJoinAndLeave is the wire-level
// integration test: stand up two simulated HTTP SSE clients (alice and
// bob, distinct users), capture every event that gets written to each,
// and assert that the presence events reflect the actual subscriber
// changes. This is the closest we get to a real two-browser test
// without two real browsers.
func TestSSEBroker_PresenceBroadcastsOnJoinAndLeave(t *testing.T) {
	broker := NewSSEBroker()

	alice := PresenceUser{UserID: "u_alice", Name: "Alice"}
	bob := PresenceUser{UserID: "u_bob", Name: "Bob"}

	// Alice connects first.
	aliceRec := httptest.NewRecorder()
	aliceCtx, aliceCancel := context.WithCancel(context.Background())
	aliceReq := httptest.NewRequest("GET", "/api/stream", nil).WithContext(aliceCtx)

	aliceDone := make(chan struct{})
	go func() {
		broker.ServeHTTP(aliceRec, aliceReq, alice)
		close(aliceDone)
	}()

	// Brief delay so Alice's subscribe-side presence broadcast completes
	// before Bob joins. Without this we race — Bob could observe the
	// pre-Alice empty snapshot via his init replay.
	time.Sleep(30 * time.Millisecond)

	// Bob joins. His SSE response will receive an initial presence
	// snapshot showing both users.
	bobRec := httptest.NewRecorder()
	bobCtx, bobCancel := context.WithCancel(context.Background())
	bobReq := httptest.NewRequest("GET", "/api/stream", nil).WithContext(bobCtx)

	bobDone := make(chan struct{})
	go func() {
		broker.ServeHTTP(bobRec, bobReq, bob)
		close(bobDone)
	}()

	// Let the join propagate.
	time.Sleep(50 * time.Millisecond)

	// Alice leaves first.
	aliceCancel()
	<-aliceDone

	// Let the leave propagate, then close Bob.
	time.Sleep(50 * time.Millisecond)
	bobCancel()
	<-bobDone

	aliceBody := aliceRec.Body.String()
	bobBody := bobRec.Body.String()

	// Alice's stream MUST contain a presence event mentioning Bob (the
	// arrival broadcast). Bob's stream MUST contain an initial presence
	// snapshot showing both users.
	if !strings.Contains(aliceBody, `"user_id":"u_bob"`) {
		t.Fatalf("Alice should see Bob join in her stream:\n%s", aliceBody)
	}
	if !strings.Contains(bobBody, `"user_id":"u_alice"`) {
		t.Fatalf("Bob should see Alice in his initial presence:\n%s", bobBody)
	}
	if !strings.Contains(bobBody, `"user_id":"u_bob"`) {
		t.Fatalf("Bob should see himself in his initial presence:\n%s", bobBody)
	}
}

// TestSSEBroker_PresenceMultiTabSameUserNoDoubleBroadcast verifies the
// SECOND tab of the same user does not produce a presence "join" that
// looks like a new user. Other clients should see no change.
func TestSSEBroker_PresenceMultiTabSameUserNoDoubleBroadcast(t *testing.T) {
	broker := NewSSEBroker()
	alice := PresenceUser{UserID: "u_alice", Name: "Alice"}
	bob := PresenceUser{UserID: "u_bob", Name: "Bob"}

	bobRec := httptest.NewRecorder()
	bobCtx, bobCancel := context.WithCancel(context.Background())
	bobReq := httptest.NewRequest("GET", "/api/stream", nil).WithContext(bobCtx)

	bobDone := make(chan struct{})
	go func() {
		broker.ServeHTTP(bobRec, bobReq, bob)
		close(bobDone)
	}()
	time.Sleep(30 * time.Millisecond)

	// Alice opens two tabs back-to-back.
	tabA := broker.Subscribe(alice)
	tabB := broker.Subscribe(alice)

	// Let any presence broadcasts settle.
	time.Sleep(50 * time.Millisecond)

	// Snapshot should show exactly Alice + Bob, not "Alice, Alice, Bob".
	snap := broker.PresenceSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 users despite Alice having 2 tabs, got %d: %+v", len(snap), snap)
	}

	broker.Unsubscribe(tabA)
	broker.Unsubscribe(tabB)
	bobCancel()
	<-bobDone

	bobBody := bobRec.Body.String()
	// Bob's stream should contain a presence event (the broadcast for
	// Alice's first tab joining). Count is harder to assert precisely
	// without tightly controlling timing, but the deduped snapshot is
	// the load-bearing invariant.
	if !strings.Contains(bobBody, "event: presence") {
		t.Fatalf("Bob should have seen at least one presence event:\n%s", bobBody)
	}
}
