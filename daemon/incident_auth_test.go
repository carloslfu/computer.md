// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"
)

// TestIncidentEndpoints_HealthTokenGated locks in the Workstream C
// re-gate: /api/management/refresh-jwks and /api/management/revoke-sessions
// must accept the HEALTH token (the platform-held per-machine
// credential, so a fleet fan-out is possible) and must REJECT the
// daemon token (machine-local, the SQLCipher DB-key root, never
// escrowed to the platform). Before the re-gate these were
// withManagementAuth (daemon token) and the platform could never call
// them — the incident-rotation fan-out was dead.
func TestIncidentEndpoints_HealthTokenGated(t *testing.T) {
	ts := newTestServer(t)

	for _, path := range []string{
		"/api/management/refresh-jwks",
		"/api/management/revoke-sessions",
	} {
		// Daemon token must NO LONGER work — proves the re-gate.
		if w := ts.raw("POST", path, "Bearer "+ts.daemonTok); w.Code != http.StatusUnauthorized {
			t.Errorf("%s with daemon token: want 401 (re-gated off daemon token), got %d", path, w.Code)
		}
		// No auth must be rejected.
		if w := ts.raw("POST", path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s with no auth: want 401, got %d", path, w.Code)
		}
		// Health token must pass the auth gate. refresh-jwks may then
		// 500 (no platform JWKS server in the test) — that's past the
		// gate, which is all this asserts. The point: NOT 401.
		if w := ts.raw("POST", path, "Bearer "+ts.healthTok); w.Code == http.StatusUnauthorized {
			t.Errorf("%s with health token: want auth to pass (not 401), got 401", path)
		}
	}

	// revoke-sessions with the health token should fully succeed
	// (empty test DB → DeleteAllSessions returns cleanly).
	if w := ts.raw("POST", "/api/management/revoke-sessions", "Bearer "+ts.healthTok); w.Code != http.StatusOK {
		t.Errorf("revoke-sessions with health token: want 200, got %d", w.Code)
	}
}
