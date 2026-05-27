// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SSEEvent is a single server-sent event.
type SSEEvent struct {
	ID    string      `json:"id"`
	Event string      `json:"event"`
	Data  interface{} `json:"data"`
}

// PresenceUser identifies a connected user for the presence indicator
// in the chat header. Same human across multiple tabs/devices is deduped
// by UserID at broadcast time, so each unique person shows up once.
//
// Why presence and not a control lock: the product is "the first agentic
// computer for business" with team plans baked in. Two team members
// genuinely co-operating on a machine need awareness of each other, not
// an exclusive lock + hand-off ceremony — and one human with two tabs
// open (laptop + phone) must never trigger an "Alice is using this
// computer" warning against herself. Presence (Google Docs model) gives
// the awareness signal without locking anyone out.
type PresenceUser struct {
	UserID string `json:"user_id"`
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
}

// subscriber pairs an event channel with the identity of the connected
// user. Only stored internally so the broker can produce a deduped
// presence list without leaking channel identity to other clients.
type subscriber struct {
	ch   chan SSEEvent
	user PresenceUser
}

// SSEBroker manages SSE client connections, broadcasts events, and
// publishes a `presence` event to all subscribers whenever the set of
// connected users changes.
type SSEBroker struct {
	mu      sync.RWMutex
	clients map[chan SSEEvent]*subscriber
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan SSEEvent]*subscriber),
	}
}

// Subscribe adds a new client channel. Caller must call Unsubscribe when
// done. Passing a non-empty UserID in `user` includes this client in the
// broadcast presence list (deduped against any other tabs/devices the
// same user has open). An empty UserID is permitted — the client still
// receives events, but is invisible in the presence list (used by tests
// and any future unauthenticated stream).
func (b *SSEBroker) Subscribe(user PresenceUser) chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	b.mu.Lock()
	b.clients[ch] = &subscriber{ch: ch, user: user}
	b.mu.Unlock()
	if user.UserID != "" {
		b.broadcastPresence()
	}
	return ch
}

// Unsubscribe removes a client channel and closes it. If the departing
// client was the last connection for its user, the presence broadcast
// will drop them from the list.
func (b *SSEBroker) Unsubscribe(ch chan SSEEvent) {
	b.mu.Lock()
	sub, existed := b.clients[ch]
	delete(b.clients, ch)
	b.mu.Unlock()
	close(ch)
	if existed && sub.user.UserID != "" {
		b.broadcastPresence()
	}
}

// Publish sends an event to all connected clients. Non-blocking: if a
// client's buffer is full, the event is dropped for that client.
func (b *SSEBroker) Publish(event SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			// Client too slow, drop event.
		}
	}
}

// Emit is a convenience method that publishes a typed event with auto-generated ID.
func (b *SSEBroker) Emit(eventType string, data interface{}) {
	b.Publish(SSEEvent{
		ID:    fmt.Sprintf("%d", time.Now().UnixNano()),
		Event: eventType,
		Data:  data,
	})
}

// PresenceSnapshot returns the current deduped list of connected users.
// Used by ServeHTTPWithInit so a freshly-connecting client sees who else
// is here immediately instead of waiting for the next sub/unsub change.
func (b *SSEBroker) PresenceSnapshot() []PresenceUser {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := make(map[string]PresenceUser)
	for _, sub := range b.clients {
		if sub.user.UserID == "" {
			continue
		}
		if _, ok := seen[sub.user.UserID]; !ok {
			seen[sub.user.UserID] = sub.user
		}
	}
	users := make([]PresenceUser, 0, len(seen))
	for _, u := range seen {
		users = append(users, u)
	}
	return users
}

// broadcastPresence emits a `presence` event to every connected client
// with the current deduped user set. Called after any change to the
// subscriber set.
func (b *SSEBroker) broadcastPresence() {
	users := b.PresenceSnapshot()
	b.Emit("presence", map[string]interface{}{"users": users})
}

// ServeHTTP handles SSE connections. It sets the correct headers, streams
// events, and cleans up when the client disconnects. The `user`
// parameter identifies the connected user for presence; pass a zero
// value to subscribe silently.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request, user PresenceUser) {
	b.ServeHTTPWithInit(w, r, user, nil)
}

// ServeHTTPWithInit is like ServeHTTP but replays the provided initial
// events to THIS client before entering the broadcast loop. Used to
// self-heal reconnects (e.g. re-deliver pending task:waiting approvals
// that the client may have missed during a disconnect or page reload).
// The initial events are sent only to the new subscriber, not broadcast.
//
// A `presence` event is always sent as the FIRST replayed event so a
// reconnecting client immediately knows who else is on the machine —
// it doesn't have to wait for the next sub/unsub change.
func (b *SSEBroker) ServeHTTPWithInit(w http.ResponseWriter, r *http.Request, user PresenceUser, initial []SSEEvent) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := b.Subscribe(user)
	defer b.Unsubscribe(ch)

	ctx := r.Context()

	// Send initial keepalive.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Always send the current presence snapshot first so this client
	// renders other connected users immediately.
	writeSSEEvent(w, SSEEvent{
		ID:    fmt.Sprintf("%d-presence", time.Now().UnixNano()),
		Event: "presence",
		Data:  map[string]interface{}{"users": b.PresenceSnapshot()},
	})

	for _, evt := range initial {
		writeSSEEvent(w, evt)
	}
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-ch:
			writeSSEEvent(w, evt)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, evt SSEEvent) {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		data = []byte(`"error"`)
	}
	if evt.ID != "" {
		fmt.Fprintf(w, "id: %s\n", evt.ID)
	}
	if evt.Event != "" {
		fmt.Fprintf(w, "event: %s\n", evt.Event)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}
