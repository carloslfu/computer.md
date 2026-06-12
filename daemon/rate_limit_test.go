// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRateLimiterExemptsLocalControlPlane is the regression for daemon-services-1:
// the usage-credit proxy, Caddy's on-demand-TLS ask, and the manager all reach the
// daemon over loopback with no X-Forwarded-For and shared one 127.0.0.1 bucket,
// so a busy hosted tool got 429'd and starved TLS issuance. Loopback-no-XFF must
// be exempt; external (X-Forwarded-For) traffic must still be limited.
func TestRateLimiterExemptsLocalControlPlane(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	defer rl.stop()
	h := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Loopback, no X-Forwarded-For: never limited, even well past the limit.
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("GET", "/api/ai/credits/openai/v1/chat", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("loopback control-plane request %d was rate-limited (code %d); must be exempt", i, rec.Code)
		}
	}

	// External traffic (Caddy sets X-Forwarded-For) is still limited.
	limited := false
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api/something", nil)
		req.RemoteAddr = "127.0.0.1:54321"            // Caddy's hop is loopback…
		req.Header.Set("X-Forwarded-For", "203.0.113.9") // …but XFF marks it external
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("external (X-Forwarded-For) traffic was never rate-limited")
	}
}
