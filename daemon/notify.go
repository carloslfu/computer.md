// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
)

// handleNotify accepts a push-to-user notification from the manager
// running on this machine. Localhost-only (see route registration in
// main.go): the manager calls it via loopback curl, the same trust
// model as /daemon/task and /routes. Anything that can reach loopback
// already runs on this machine as the vibecraft user; the real trust
// boundary is the platform side, which validates the forward against
// the machine's healthToken before storing or dispatching.
//
// We forward the notification to the platform's ingest endpoint using
// the machine's healthToken (the platform-known shared secret for this
// machine). Stored in the platform DB; dispatched to email/bell by
// priority.
//
// Body:
//
//	{
//	  "kind":            "system:done",   // freeform string
//	  "title":           "Daily report ready",
//	  "body":            "April invoices summary in /home/vibecraft/...",
//	  "conversation_id": "system:invoice-triage",  // optional
//	  "priority":        "normal"          // low | normal | high
//	}
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Kind           string `json:"kind"`
		Title          string `json:"title"`
		Body           string `json:"body"`
		ConversationID string `json:"conversation_id"`
		Priority       string `json:"priority"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		req.Kind = "manager"
	}
	switch req.Priority {
	case "", "low", "normal", "high":
		if req.Priority == "" {
			req.Priority = "normal"
		}
	default:
		jsonError(w, "priority must be low, normal, or high", http.StatusBadRequest)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "notification_emitted",
		Category: "notification",
		Details:  fmt.Sprintf("kind=%s priority=%s title=%s", req.Kind, req.Priority, req.Title),
	})

	// Forward to the platform in the background — we don't make the
	// manager wait on the round-trip.
	go func() {
		if err := forwardNotificationToPlatform(s, req.Kind, req.Title, req.Body, req.ConversationID, req.Priority); err != nil {
			log.Printf("notify: forward to platform failed: %v", err)
		}
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// forwardNotificationToPlatform POSTs the notification to the platform
// using the machine's healthToken as auth. The platform validates
// against the machines table (same secret it uses for the health
// check) and stores + dispatches.
func forwardNotificationToPlatform(s *Server, kind, title, body, convID, priority string) error {
	return postNotificationToPlatform(s.cfg, kind, title, body, convID, priority)
}

// notifyMaxAttempts bounds the forward retries. Some notifications are
// edge-triggered exactly once per billing period (the 80% / 100% usage
// warnings in usage/budget.go mark the period as warned *before* the
// notify call returns), so a single transient failure would otherwise
// drop the warning permanently for that period. We retry a handful of
// times with exponential backoff to survive a brief network blip or a
// platform 5xx/429, rather than losing the notification on the first
// hiccup.
const notifyMaxAttempts = 5

// postNotificationToPlatform is the cfg-only path so the engine's
// platformNotifier (which doesn't own a *Server) can call it directly.
//
// It retries on transport errors and retryable platform responses
// (429 + 5xx) with exponential backoff so a transient failure does not
// silently drop the notification. Deterministic client errors (other
// 4xx — e.g. a rejected token) are not retried, since a repeat would
// fail identically.
func postNotificationToPlatform(cfg *Config, kind, title, body, convID, priority string) error {
	if cfg.MachineID == "" || cfg.HealthToken == "" {
		return fmt.Errorf("missing machine_id or health_token in config")
	}

	payload := map[string]interface{}{
		"machine_id":      cfg.MachineID,
		"kind":            kind,
		"title":           title,
		"body":            body,
		"conversation_id": convID,
		"priority":        priority,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	platformBase := cfg.PlatformBaseURL
	if platformBase == "" {
		platformBase = "https://www.vibecraft.so"
	}
	url := platformBase + "/api/notifications/ingest"

	var lastErr error
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= notifyMaxAttempts; attempt++ {
		retryable, err := postNotificationOnce(cfg, url, data)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == notifyMaxAttempts {
			break
		}
		log.Printf("notify: forward attempt %d/%d failed: %v (retrying in %s)", attempt, notifyMaxAttempts, err, backoff)
		time.Sleep(backoff)
		backoff *= 2
	}
	return fmt.Errorf("forward failed after %d attempts: %w", notifyMaxAttempts, lastErr)
}

// postNotificationOnce performs a single forward attempt. It returns
// whether the failure (if any) is worth retrying: transport errors and
// retryable platform responses (429 + 5xx) are retryable; other non-2xx
// responses are deterministic and are not.
func postNotificationOnce(cfg *Config, url string, data []byte) (retryable bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		// Building the request is deterministic — retrying won't help.
		return false, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.HealthToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level failure (DNS, connection refused, timeout) — retry.
		return true, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retry, fmt.Errorf("platform returned %d", resp.StatusCode)
	}
	return false, nil
}

// platformNotifier implements core.Notifier by forwarding to the
// platform via the healthToken. Async; logs but does not propagate
// transient errors back to the caller.
type platformNotifier struct {
	cfg *Config
}

func (p *platformNotifier) Notify(kind, title, body, priority string) {
	go func() {
		if err := postNotificationToPlatform(p.cfg, kind, title, body, "", priority); err != nil {
			log.Printf("notify (engine): forward failed: %v", err)
		}
	}()
}
