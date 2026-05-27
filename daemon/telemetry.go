// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

// startTelemetry kicks off the optional weekly version-ping loop.
//
// Trust is the differentiator for VibeCraft. Telemetry is off by
// default. When opted in via `VIBECRAFT_TELEMETRY=on`, the daemon
// sends exactly one payload, once a week, to
// `${VIBECRAFT_TELEMETRY_URL:-${VIBECRAFT_PLATFORM_URL}/api/telemetry/v1/ping}`:
//
//	{
//	  "version": "<binary version>",
//	  "os":      "<runtime.GOOS>",
//	  "arch":    "<runtime.GOARCH>"
//	}
//
// No PII. No customer data. No identifiers other than version/os/arch.
// Open source so the payload is auditable line by line.
//
// `VIBECRAFT_CRASH_REPORTING=on` is recognized for forward compatibility
// but is not implemented in this reference build — production daemons
// that ship a crash reporter must scrub all paths and arguments before
// sending.
func startTelemetry(ctx context.Context, platformBaseURL, version string) {
	if os.Getenv("VIBECRAFT_TELEMETRY") != "on" {
		return
	}
	endpoint := os.Getenv("VIBECRAFT_TELEMETRY_URL")
	if endpoint == "" {
		endpoint = platformBaseURL + "/api/telemetry/v1/ping"
	}
	go telemetryLoop(ctx, endpoint, version)
}

func telemetryLoop(ctx context.Context, endpoint, version string) {
	// Stagger first ping by 1-2 minutes after start to avoid bunching.
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			pingTelemetry(ctx, endpoint, version)
			timer.Reset(7 * 24 * time.Hour)
		}
	}
}

func pingTelemetry(ctx context.Context, endpoint, version string) {
	payload := map[string]string{
		"version": version,
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
	}
	body, _ := json.Marshal(payload)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("vibecraft-daemon/%s", version))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Silent failure — telemetry is best-effort.
		return
	}
	_ = resp.Body.Close()
}
