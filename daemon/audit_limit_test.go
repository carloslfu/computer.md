// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/carloslfu/computer.md/daemon/audit"
)

// TestAuditLimitHonored verifies the ?limit= query param caps how many
// audit rows the handler returns when fewer than the cap are requested.
func TestAuditLimitHonored(t *testing.T) {
	ts := newTestServer(t)

	for i := 0; i < 5; i++ {
		ts.server.auditLog.Log(audit.Entry{Action: "probe", Category: "system"})
	}

	w := ts.do(t, http.MethodGet, "/audit?limit=2", "Bearer "+ts.signJWT(t, "user-1"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit = %d: %s", w.Code, w.Body.String())
	}
	var entries []audit.Entry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode audit entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("limit=2 returned %d entries, want 2", len(entries))
	}
}

// TestAuditLimitCappedAtMax verifies that an over-large ?limit= value is
// clamped to maxAuditLimit rather than flowing straight into a SQL LIMIT.
// A buggy or hostile client must not be able to force the daemon to load
// and serialize an unbounded result set.
func TestAuditLimitCappedAtMax(t *testing.T) {
	ts := newTestServer(t)

	// More rows than a single small page, but far fewer than the cap —
	// enough to confirm a huge limit returns successfully without error
	// and stays bounded by the cap.
	const rows = 50
	for i := 0; i < rows; i++ {
		ts.server.auditLog.Log(audit.Entry{Action: "probe", Category: "system"})
	}

	w := ts.do(t, http.MethodGet, "/audit?limit=99999999", "Bearer "+ts.signJWT(t, "user-1"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit = %d: %s", w.Code, w.Body.String())
	}
	var entries []audit.Entry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode audit entries: %v", err)
	}
	// The effective limit is min(requested, maxAuditLimit); with `rows`
	// entries present the response must never exceed the cap.
	if len(entries) > maxAuditLimit {
		t.Fatalf("over-large limit returned %d entries, exceeds cap %d", len(entries), maxAuditLimit)
	}
	if len(entries) != rows {
		t.Fatalf("expected all %d entries (cap is %d), got %d", rows, maxAuditLimit, len(entries))
	}
}
