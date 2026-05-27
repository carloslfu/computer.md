// SPDX-License-Identifier: Apache-2.0

package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Store is the persistent token accumulator. It upserts into
// usage_records on every manager call and aggregates over date ranges
// for the dashboard's Usage panel.
type Store struct {
	db *persistence.DB
}

func NewStore(db *persistence.DB) *Store {
	return &Store{db: db}
}

// Record accumulates a single manager response's usage into the
// (day, model, conversation_id) bucket for today. conversationID may
// be empty for calls outside a chat (compactor, summarizer when called
// at startup, etc.).
//
// Atomic via SQLite's INSERT ... ON CONFLICT ... DO UPDATE — no
// read-modify-write race even under concurrent agent loops.
func (s *Store) Record(model, conversationID string, u manager.Usage) error {
	if model == "" {
		return fmt.Errorf("usage.Record: model required")
	}
	day := time.Now().UTC().Format("2006-01-02")
	conn := s.db.Conn()
	_, err := conn.Exec(`
		INSERT INTO usage_records (
			day, model, conversation_id,
			input_tokens, output_tokens,
			cache_read_tokens, cache_create_tokens,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(day, model, conversation_id) DO UPDATE SET
			input_tokens        = input_tokens        + excluded.input_tokens,
			output_tokens       = output_tokens       + excluded.output_tokens,
			cache_read_tokens   = cache_read_tokens   + excluded.cache_read_tokens,
			cache_create_tokens = cache_create_tokens + excluded.cache_create_tokens,
			updated_at          = CURRENT_TIMESTAMP
	`, day, model, conversationID,
		u.InputTokens, u.OutputTokens,
		u.CacheReadInputTokens, u.CacheCreationInputTokens)
	return err
}

// Summary is the response shape for an aggregate query over a date
// range. All values are denominated in USD and tokens respectively.
type Summary struct {
	Period           Period           `json:"period"`
	TotalCostUSD     float64          `json:"total_cost_usd"`
	UnpricedModels   []string         `json:"unpriced_models,omitempty"`
	ByModel          []ModelBreakdown `json:"by_model"`
	ByDay            []DayBreakdown   `json:"by_day"`
	TopConversations []ConvoBreakdown `json:"top_conversations"`
	// BudgetState is the enforcement verdict — set by the /api/usage
	// handler from the BudgetTracker. nil when the daemon has no
	// budget tracker wired (tests). The SPA reads this to render the
	// "budget reached — tasks paused" banner authoritatively, rather
	// than recomputing paused-ness from plan + spend itself.
	BudgetState *BudgetState `json:"budget_state,omitempty"`
}

type Period struct {
	Start string `json:"start"` // YYYY-MM-DD, inclusive
	End   string `json:"end"`   // YYYY-MM-DD, inclusive
}

type ModelBreakdown struct {
	Model             string  `json:"model"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	CostUSD           float64 `json:"cost_usd"`
}

type DayBreakdown struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"cost_usd"`
}

type ConvoBreakdown struct {
	ConversationID string  `json:"conversation_id"`
	Title          string  `json:"title"` // empty when the conversations row is gone
	CostUSD        float64 `json:"cost_usd"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
}

// Aggregate sums usage between start and end (inclusive). Dates are
// YYYY-MM-DD in UTC; the caller is responsible for picking the right
// period boundaries (calendar month, Stripe subscription period, etc).
//
// topConvos limits the TopConversations list. Pass 0 to skip the
// conversation breakdown entirely (cheaper query).
func (s *Store) Aggregate(start, end string, topConvos int) (Summary, error) {
	summary := Summary{
		Period:           Period{Start: start, End: end},
		ByModel:          []ModelBreakdown{},
		ByDay:            []DayBreakdown{},
		TopConversations: []ConvoBreakdown{},
	}

	if start == "" || end == "" {
		return summary, fmt.Errorf("usage.Aggregate: start and end required (YYYY-MM-DD)")
	}

	conn := s.db.Conn()

	byModel, unpriced, err := s.aggregateByModel(conn, start, end)
	if err != nil {
		return summary, fmt.Errorf("aggregate by model: %w", err)
	}
	summary.ByModel = byModel
	if len(unpriced) > 0 {
		summary.UnpricedModels = unpriced
	}
	for _, m := range byModel {
		summary.TotalCostUSD += m.CostUSD
	}

	byDay, err := s.aggregateByDay(conn, start, end)
	if err != nil {
		return summary, fmt.Errorf("aggregate by day: %w", err)
	}
	summary.ByDay = byDay

	if topConvos > 0 {
		convos, err := s.topConversations(conn, start, end, topConvos)
		if err != nil {
			return summary, fmt.Errorf("top conversations: %w", err)
		}
		summary.TopConversations = convos
	}

	return summary, nil
}

func (s *Store) aggregateByModel(conn *sql.DB, start, end string) ([]ModelBreakdown, []string, error) {
	rows, err := conn.Query(`
		SELECT
			model,
			SUM(input_tokens),
			SUM(output_tokens),
			SUM(cache_read_tokens),
			SUM(cache_create_tokens)
		FROM usage_records
		WHERE day >= ? AND day <= ?
		GROUP BY model
		ORDER BY model
	`, start, end)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	results := []ModelBreakdown{}
	var unpriced []string
	for rows.Next() {
		var b ModelBreakdown
		if err := rows.Scan(&b.Model, &b.InputTokens, &b.OutputTokens,
			&b.CacheReadTokens, &b.CacheCreateTokens); err != nil {
			return nil, nil, err
		}
		b.CostUSD = CostByModel(b.Model, b.InputTokens, b.OutputTokens, b.CacheReadTokens, b.CacheCreateTokens)
		if !IsKnownModel(b.Model) {
			unpriced = append(unpriced, b.Model)
		}
		results = append(results, b)
	}
	return results, unpriced, rows.Err()
}

func (s *Store) aggregateByDay(conn *sql.DB, start, end string) ([]DayBreakdown, error) {
	rows, err := conn.Query(`
		SELECT
			day,
			model,
			SUM(input_tokens),
			SUM(output_tokens),
			SUM(cache_read_tokens),
			SUM(cache_create_tokens)
		FROM usage_records
		WHERE day >= ? AND day <= ?
		GROUP BY day, model
		ORDER BY day ASC
	`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Pivot to "one row per day, cost summed across models" — the chart
	// only needs the day total. Per-model-per-day is one query layer
	// down if anyone ever needs it.
	byDay := map[string]float64{}
	order := []string{}
	for rows.Next() {
		var day, model string
		var input, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&day, &model, &input, &output, &cacheRead, &cacheCreate); err != nil {
			return nil, err
		}
		if _, ok := byDay[day]; !ok {
			order = append(order, day)
		}
		byDay[day] += CostByModel(model, input, output, cacheRead, cacheCreate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	results := make([]DayBreakdown, 0, len(order))
	for _, day := range order {
		results = append(results, DayBreakdown{Day: day, CostUSD: byDay[day]})
	}
	return results, nil
}

func (s *Store) topConversations(conn *sql.DB, start, end string, limit int) ([]ConvoBreakdown, error) {
	// Join with conversations to get titles. LEFT JOIN so usage from a
	// since-deleted conversation still shows up (with empty title) — we
	// never silently drop attributed spend.
	rows, err := conn.Query(`
		SELECT
			u.conversation_id,
			COALESCE(c.title, '') AS title,
			SUM(u.input_tokens),
			SUM(u.output_tokens),
			SUM(u.cache_read_tokens),
			SUM(u.cache_create_tokens),
			-- Per-row cost can't be computed in SQL because pricing is
			-- per-model. We aggregate by (convo, model), compute cost
			-- in Go, then re-aggregate by convo. So this query just
			-- pulls the raw token counts grouped by (convo, model).
			u.model
		FROM usage_records u
		LEFT JOIN conversations c ON c.id = u.conversation_id
		WHERE u.day >= ? AND u.day <= ?
		  AND u.conversation_id != ''
		GROUP BY u.conversation_id, u.model
	`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type accum struct {
		title       string
		input       int64
		output      int64
		cacheRead   int64
		cacheCreate int64
		costUSD     float64
	}
	byConvo := map[string]*accum{}
	for rows.Next() {
		var convo, title, model string
		var input, output, cacheRead, cacheCreate int64
		if err := rows.Scan(&convo, &title, &input, &output, &cacheRead, &cacheCreate, &model); err != nil {
			return nil, err
		}
		a, ok := byConvo[convo]
		if !ok {
			a = &accum{title: title}
			byConvo[convo] = a
		}
		a.input += input
		a.output += output
		a.cacheRead += cacheRead
		a.cacheCreate += cacheCreate
		a.costUSD += CostByModel(model, input, output, cacheRead, cacheCreate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	results := make([]ConvoBreakdown, 0, len(byConvo))
	for convo, a := range byConvo {
		results = append(results, ConvoBreakdown{
			ConversationID: convo,
			Title:          a.title,
			CostUSD:        a.costUSD,
			InputTokens:    a.input,
			OutputTokens:   a.output,
		})
	}

	// Sort by cost descending, then by convo ID for stable ordering
	// when costs tie.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].CostUSD > results[i].CostUSD ||
				(results[j].CostUSD == results[i].CostUSD && strings.Compare(results[j].ConversationID, results[i].ConversationID) < 0) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// CurrentMonthBounds returns the YYYY-MM-DD strings for the first and
// last day of the current UTC calendar month. Used as the default
// period when the SPA doesn't pass explicit start/end (e.g. when the
// platform-side Stripe subscription anchor isn't available).
func CurrentMonthBounds() (string, string) {
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}
