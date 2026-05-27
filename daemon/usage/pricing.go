// SPDX-License-Identifier: Apache-2.0

// Package usage tracks the daemon's manager API consumption and exposes
// it as a per-machine spend report.
//
// Why this is per-machine: the daemon runs the manager (the AI agent on
// the customer's computer). On Managed machines, every manager turn consumes
// OpenAI tokens through VibeCraft's platform key. VibeCraft pays OpenAI and
// bills it back to the customer at cost. For that promise to be operational,
// the customer needs to see actual consumption, in dollars, continuously.
// This package is the source of that truth.
//
// Workers (Claude Code, Codex CLI) don't bill through here — they run
// on the user's own Claude Pro/Max or ChatGPT subscription. Only the
// manager turns flow through manager.Client and therefore through this
// package's recorder.
package usage

import (
	"github.com/carloslfu/computer.md/daemon/manager"
)

// ModelPricing is the per-million-token rate for one manager model.
// OpenAI cached input is priced separately. Cache creation remains in this
// shape because the usage store has that column and future providers may
// report it.
//
// Source: https://developers.openai.com/api/docs/pricing (rates frozen at
// the time we ship; revisit whenever OpenAI publishes new prices). The
// "bills at cost" promise is broken if these numbers drift from the
// real upstream rates, so changing them is a deliberate operational
// action, not a casual edit.
type ModelPricing struct {
	InputPerMTok       float64
	OutputPerMTok      float64
	CacheReadPerMTok   float64
	CacheCreatePerMTok float64
}

// Pricing is the canonical table. Add new models here when the engine
// or any other manager caller starts using them. An unknown model is
// treated as zero-cost (with a logged warning at the caller) rather
// than silently overcharging the customer — but every model we
// actually use must be present here.
var Pricing = map[string]ModelPricing{
	"gpt-5.4-mini": {
		InputPerMTok:       0.75,
		OutputPerMTok:      4.50,
		CacheReadPerMTok:   0.075,
		CacheCreatePerMTok: 0,
	},
	"gpt-5.4-mini-2026-03-17": {
		InputPerMTok:       0.75,
		OutputPerMTok:      4.50,
		CacheReadPerMTok:   0.075,
		CacheCreatePerMTok: 0,
	},
	"gpt-5.4": {
		InputPerMTok:       2.50,
		OutputPerMTok:      15.00,
		CacheReadPerMTok:   0.25,
		CacheCreatePerMTok: 0,
	},
}

// Cost computes the USD cost of a single manager response given the
// model that produced it. Returns 0 if the model isn't in the Pricing
// table — the caller is expected to also surface "unpriced model" so
// the gap is visible rather than silently zeroed.
func Cost(model string, u manager.Usage) float64 {
	p, ok := Pricing[model]
	if !ok {
		return 0
	}
	return CostFromCounts(p, int64(u.InputTokens), int64(u.OutputTokens), int64(u.CacheReadInputTokens), int64(u.CacheCreationInputTokens))
}

// CostFromCounts is the same as Cost but takes raw token counts. Used
// by the store to compute totals over aggregated rows where the
// manager.Usage object isn't carried along.
func CostFromCounts(p ModelPricing, input, output, cacheRead, cacheCreate int64) float64 {
	const perMillion = 1_000_000.0
	return float64(input)*p.InputPerMTok/perMillion +
		float64(output)*p.OutputPerMTok/perMillion +
		float64(cacheRead)*p.CacheReadPerMTok/perMillion +
		float64(cacheCreate)*p.CacheCreatePerMTok/perMillion
}

// CostByModel returns 0 for unknown models. Use IsKnownModel if you
// need to distinguish "priced at zero" from "unrecognized model".
func CostByModel(model string, input, output, cacheRead, cacheCreate int64) float64 {
	p, ok := Pricing[model]
	if !ok {
		return 0
	}
	return CostFromCounts(p, input, output, cacheRead, cacheCreate)
}

// IsKnownModel reports whether a model has a pricing entry. Callers
// surfacing usage to users should flag any totals computed from
// unknown-model rows so the user sees "missing pricing for model X"
// rather than a wrongly-low total.
func IsKnownModel(model string) bool {
	_, ok := Pricing[model]
	return ok
}
