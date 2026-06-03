// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"math"
	"testing"

	"github.com/carloslfu/computer.md/daemon/manager"
)

// approxEqual compares dollar amounts with tolerance for floating-point
// noise. Six decimal places is generous — we display $X.XX in the UI,
// so anything tighter than $0.0001 is invisible to the user.
func approxEqual(t *testing.T, got, want float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s: got %.10f want %.10f", msg, got, want)
	}
}

// TestPricing_GPT54MiniSample pins the gpt-5.4-mini math against a concrete
// hand-computed example. If OpenAI raises their rate (or we
// fat-finger the constant), this test breaks immediately and the user
// stops being billed the old (lower) rate while charging the new one.
func TestPricing_GPT54MiniSample(t *testing.T) {
	// 2M total input, of which 1M is cache-read and 1M is cache-create,
	// plus 1M output:
	//   0.0 * 0.75  = 0.00
	//   1.0 * 4.50  = 4.50
	//   1.0 * 0.075 = 0.075
	//   1.0 * 0.00  = 0.00
	//   total = 4.575
	u := manager.Usage{
		InputTokens:              2_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	approxEqual(t, Cost("gpt-5.4-mini", u), 4.575, "gpt-5.4-mini cached input subsets")
}

func TestPricing_GPT54Sample(t *testing.T) {
	// 2M total input, of which 1M is cache-read and 1M is cache-create,
	// plus 1M output:
	//   0.0 * 2.50 = 0.00
	//   1.0 * 15.0 = 15.00
	//   1.0 * 0.25 = 0.25
	//   1.0 * 0.00 = 0.00
	//   total = 15.25
	u := manager.Usage{
		InputTokens:              2_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	approxEqual(t, Cost("gpt-5.4", u), 15.25, "gpt-5.4 cached input subsets")
}

// TestPricing_RealisticTask is closer to what one agent loop actually
// looks like: a few thousand input + output tokens, most of the input
// served from cache. Pins the case that dominates real bills.
func TestPricing_RealisticTask(t *testing.T) {
	// 5k uncached input, 50k cached input, 2k output, 0 new cache writes:
	//   5_000  * 0.75  / 1e6 = 0.00375
	//   2_000  * 4.50  / 1e6 = 0.009
	//   50_000 * 0.075 / 1e6 = 0.00375
	//   total = 0.0165
	u := manager.Usage{
		InputTokens:          55_000,
		OutputTokens:         2_000,
		CacheReadInputTokens: 50_000,
	}
	approxEqual(t, Cost("gpt-5.4-mini", u), 0.0165, "realistic 1-turn cost")
}

func TestPricing_ClampsImpossibleCacheSubset(t *testing.T) {
	u := manager.Usage{
		InputTokens:          10_000,
		OutputTokens:         1_000,
		CacheReadInputTokens: 50_000,
	}
	want := (1_000 * Pricing["gpt-5.4-mini"].OutputPerMTok / 1_000_000.0) +
		(50_000 * Pricing["gpt-5.4-mini"].CacheReadPerMTok / 1_000_000.0)
	approxEqual(t, Cost("gpt-5.4-mini", u), want, "impossible cached subset should not make input cost negative")
}

// TestPricing_UnknownModelReturnsZero verifies the deliberate-zero
// fallback for a model we don't have pricing for. Caller is expected
// to surface unpriced-model warnings; this function returns 0 rather
// than crashing.
func TestPricing_UnknownModelReturnsZero(t *testing.T) {
	u := manager.Usage{InputTokens: 999_999, OutputTokens: 999_999}
	if got := Cost("gpt-future-expensive", u); got != 0 {
		t.Fatalf("unknown model should cost 0, got %.4f", got)
	}
	if IsKnownModel("gpt-future-expensive") {
		t.Fatalf("IsKnownModel should be false for unrecognized model")
	}
}

// TestPricing_ZeroUsageIsZero — defensive: an empty usage object must
// not produce phantom cost.
func TestPricing_ZeroUsageIsZero(t *testing.T) {
	for model := range Pricing {
		if got := Cost(model, manager.Usage{}); got != 0 {
			t.Fatalf("zero usage on %s should cost 0, got %.6f", model, got)
		}
	}
}

// TestPricing_KnownModelsCoverAllActuallyUsed locks in that every
// model the daemon code actually sends to OpenAI has a pricing
// entry. This list is hand-maintained alongside the pricing table —
// if engine.go starts using a new model, add it here + to Pricing.
func TestPricing_KnownModelsCoverAllActuallyUsed(t *testing.T) {
	mustPrice := []string{
		"gpt-5.4-mini", // engine.SendComputerUse, compactor, summarizer, classifier
	}
	for _, m := range mustPrice {
		if !IsKnownModel(m) {
			t.Fatalf("model %q is used by the daemon but has no Pricing entry — bill at cost is broken", m)
		}
	}
}
