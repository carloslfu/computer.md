// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"testing"
)

func TestSuccessEnvelope_RoundTrip(t *testing.T) {
	env := Success(StatusData{
		MachineID:     "vc-abc",
		Status:        "healthy",
		UptimeSeconds: 3600,
	})

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `{"v":1,"ok":true,"data":{"machine_id":"vc-abc","status":"healthy","uptime_seconds":3600}}`
	if got := string(b); got != want {
		t.Errorf("wire format drift\n got: %s\nwant: %s", got, want)
	}
}

func TestSuccessEnvelope_OmitsErrorField(t *testing.T) {
	b, _ := json.Marshal(Success(map[string]string{"k": "v"}))
	// Stable assertion that successful envelopes never carry a stray
	// "error":null on the wire. Agents that match on the existence of
	// `error` to detect failure would be misled.
	if got := string(b); contains(got, "error") {
		t.Errorf("success envelope leaked error field: %s", got)
	}
}

func TestFailureEnvelope_OmitsDataField(t *testing.T) {
	b, _ := json.Marshal(Failure(Newf(CodeAuthRequired, "no creds")))
	if got := string(b); contains(got, "data") {
		t.Errorf("failure envelope leaked data field: %s", got)
	}
}

func TestFailureEnvelope_WithHint(t *testing.T) {
	env := Failure(Newf(CodeAuthRequired, "no creds").WithHint("run vibecraft auth login"))
	b, _ := json.Marshal(env)
	want := `{"v":1,"ok":false,"error":{"code":"auth_required","message":"no creds","hint":"run vibecraft auth login"}}`
	if got := string(b); got != want {
		t.Errorf("wire format drift\n got: %s\nwant: %s", got, want)
	}
}

func TestErrorImplementsError(t *testing.T) {
	var err error = Newf(CodeTaskNotFound, "task abc not found")
	if err.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

func TestVersionIsOne(t *testing.T) {
	// If you're bumping this, you're making a breaking wire-format change.
	// That means: existing agent skills will fail to parse. Update LLMS.md,
	// update every test golden file, and announce it in the release notes
	// before changing this constant.
	if Version != 1 {
		t.Errorf("Version drifted: want 1, got %d", Version)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
