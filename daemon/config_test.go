// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

func fakeManagerFiles(files map[string]string) func(string) (string, error) {
	return func(path string) (string, error) {
		if v, ok := files[path]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}
}

func fakeManagerEnv(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func TestLoadManagerSettings_LegacyBYOMMissingOpenAIKeyBootsDegraded(t *testing.T) {
	key, mode, model, unavailable, err := loadManagerSettings(
		fakeManagerFiles(nil),
		fakeManagerEnv(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("key = %q, want empty", key)
	}
	if mode != "operator" {
		t.Fatalf("mode = %q, want operator", mode)
	}
	if model != "gpt-5.4-mini" {
		t.Fatalf("model = %q, want gpt-5.4-mini", model)
	}
	if strings.Contains(unavailable, "/etc/") || !strings.Contains(unavailable, "OpenAI API key") {
		t.Fatalf("unavailable reason should be friendly setup copy, got %q", unavailable)
	}
}

func TestLoadManagerSettings_KeyWithoutModeDefaultsPlatformCompatibility(t *testing.T) {
	key, mode, _, unavailable, err := loadManagerSettings(
		fakeManagerFiles(map[string]string{
			"/etc/vibecraft/openai.key": "sk-test",
		}),
		fakeManagerEnv(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-test" {
		t.Fatalf("key = %q", key)
	}
	if mode != "platform" {
		t.Fatalf("mode = %q, want platform", mode)
	}
	if unavailable != "" {
		t.Fatalf("unavailable = %q, want empty", unavailable)
	}
}

func TestLoadManagerSettings_OperatorModeWithKeyIsReady(t *testing.T) {
	key, mode, model, unavailable, err := loadManagerSettings(
		fakeManagerFiles(map[string]string{
			"/etc/vibecraft/openai.key":       "sk-operator",
			"/etc/vibecraft/manager_key_mode": "operator",
			"/etc/vibecraft/manager_model":    "gpt-5.4",
		}),
		fakeManagerEnv(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-operator" || mode != "operator" || model != "gpt-5.4" {
		t.Fatalf("got key=%q mode=%q model=%q", key, mode, model)
	}
	if unavailable != "" {
		t.Fatalf("unavailable = %q, want empty", unavailable)
	}
}

func TestLoadManagerSettings_PlatformModeMissingKeyUsesManagedCopy(t *testing.T) {
	_, mode, _, unavailable, err := loadManagerSettings(
		fakeManagerFiles(map[string]string{
			"/etc/vibecraft/manager_key_mode": "platform",
		}),
		fakeManagerEnv(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "platform" {
		t.Fatalf("mode = %q, want platform", mode)
	}
	if strings.Contains(unavailable, "/etc/") ||
		strings.Contains(unavailable, "your OpenAI API key") ||
		!strings.Contains(unavailable, "managed computer") {
		t.Fatalf("unavailable reason should be managed friendly copy, got %q", unavailable)
	}
}

func TestLoadManagerRuntimeSettings_DefaultsToMediumReasoningAnd64KOutput(t *testing.T) {
	effort, maxOutput, deepEffort, deepMaxOutput, err := loadManagerRuntimeSettings(
		fakeManagerFiles(nil),
		fakeManagerEnv(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if effort != "medium" {
		t.Fatalf("effort = %q, want medium", effort)
	}
	if maxOutput != 65536 {
		t.Fatalf("maxOutput = %d, want 65536", maxOutput)
	}
	if deepEffort != "high" {
		t.Fatalf("deepEffort = %q, want high", deepEffort)
	}
	if deepMaxOutput != 128000 {
		t.Fatalf("deepMaxOutput = %d, want 128000", deepMaxOutput)
	}
}

func TestLoadManagerRuntimeSettings_EnvOverridesFiles(t *testing.T) {
	effort, maxOutput, deepEffort, deepMaxOutput, err := loadManagerRuntimeSettings(
		fakeManagerFiles(map[string]string{
			"/etc/vibecraft/manager_reasoning_effort":       "low",
			"/etc/vibecraft/manager_max_output_tokens":      "8192",
			"/etc/vibecraft/manager_deep_reasoning_effort":  "medium",
			"/etc/vibecraft/manager_deep_max_output_tokens": "32768",
		}),
		fakeManagerEnv(map[string]string{
			"VIBECRAFT_MANAGER_REASONING_EFFORT":       "medium",
			"VIBECRAFT_MANAGER_MAX_OUTPUT_TOKENS":      "65536",
			"VIBECRAFT_MANAGER_DEEP_REASONING_EFFORT":  "high",
			"VIBECRAFT_MANAGER_DEEP_MAX_OUTPUT_TOKENS": "128000",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if effort != "medium" || maxOutput != 65536 || deepEffort != "high" || deepMaxOutput != 128000 {
		t.Fatalf("got effort=%q maxOutput=%d deepEffort=%q deepMaxOutput=%d", effort, maxOutput, deepEffort, deepMaxOutput)
	}
}

func TestLoadManagerRuntimeSettings_RejectsInvalidReasoningEffort(t *testing.T) {
	_, _, _, _, err := loadManagerRuntimeSettings(
		fakeManagerFiles(nil),
		fakeManagerEnv(map[string]string{
			"VIBECRAFT_MANAGER_REASONING_EFFORT": "minimal",
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "invalid manager reasoning effort") {
		t.Fatalf("expected invalid reasoning effort error, got %v", err)
	}
}

func TestLoadManagerRuntimeSettings_RejectsOversizedOutputCap(t *testing.T) {
	_, _, _, _, err := loadManagerRuntimeSettings(
		fakeManagerFiles(nil),
		fakeManagerEnv(map[string]string{
			"VIBECRAFT_MANAGER_MAX_OUTPUT_TOKENS": "200000",
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "invalid manager max output tokens") {
		t.Fatalf("expected invalid max output tokens error, got %v", err)
	}
}
