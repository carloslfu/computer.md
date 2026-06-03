// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/vault"
)

// platformAuditSink streams audit entries to the platform's append-only
// ingest endpoint (Phase 6 — write-only remote audit). The daemon's
// credential is the per-machine health token; the endpoint only
// appends, never returns or deletes, so even a fully compromised daemon
// cannot read or scrub the off-machine history.
//
// Non-blocking by contract: Forward drops into a buffered channel and
// returns immediately. On sustained backpressure the OLDEST queued
// entry is dropped (the daemon's local audit_log row is still the
// authoritative local copy; the sink is best-effort durability, not a
// transaction). It must never slow or wedge the audit hot path.
type platformAuditSink struct {
	cfg  *Config
	mask *vault.Masker
	ch   chan audit.Entry
}

func newPlatformAuditSink(cfg *Config, mask *vault.Masker) *platformAuditSink {
	s := &platformAuditSink{cfg: cfg, mask: mask, ch: make(chan audit.Entry, 256)}
	go s.run()
	return s
}

func (s *platformAuditSink) Forward(e audit.Entry) {
	select {
	case s.ch <- e:
	default:
		// Buffer full (platform unreachable / slow). Drop the oldest to
		// make room for the newest, then enqueue. Best-effort: never
		// block the caller.
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- e:
		default:
		}
	}
}

func (s *platformAuditSink) run() {
	for e := range s.ch {
		if err := s.post(e); err != nil {
			// A dropped sink entry is not fatal — the local row stands.
			log.Printf("audit sink: forward failed (action=%s): %v", e.Action, err)
		}
	}
}

func (s *platformAuditSink) post(e audit.Entry) error {
	// Last-hop secret scrub before the entry leaves the machine. Engine
	// call sites already mask tool input, but the sink is the single choke
	// point every audit entry passes through on its way off-box, so we
	// mask Details here too: a future (or third-party) audit caller that
	// records a raw secret in Details still cannot exfiltrate it through
	// the write-only platform sink. Defense in depth, not the primary
	// control. Nil-safe so tests that build the sink without a masker
	// (and any pre-wiring caller) still work.
	details := e.Details
	if s.mask != nil {
		details = s.mask.Mask(details)
	}
	payload := map[string]any{
		"machine_id": s.cfg.MachineID,
		"id":         e.ID,
		"timestamp":  e.Timestamp,
		"action":     e.Action,
		"category":   e.Category,
		"user_id":    e.UserID,
		"task_id":    e.TaskID,
		"details":    details,
		"risk_level": e.RiskLevel,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := s.cfg.PlatformBaseURL + "/api/audit/ingest"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.HealthToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &sinkHTTPError{status: resp.StatusCode}
	}
	return nil
}

type sinkHTTPError struct{ status int }

func (e *sinkHTTPError) Error() string {
	return "platform audit ingest returned " + http.StatusText(e.status)
}
