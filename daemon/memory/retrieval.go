// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"strings"
)

// Search finds memory items relevant to the given query string.
// It uses a simple keyword-matching approach: it tokenizes the query,
// then scores each memory item by how many query tokens appear in
// its key, value, or category. Results are returned sorted by relevance.
func (s *Store) Search(query string, limit int) ([]*Item, error) {
	if query == "" || limit <= 0 {
		return nil, nil
	}

	// Get all memory items.
	all, err := s.List("")
	if err != nil {
		return nil, err
	}

	if len(all) == 0 {
		return nil, nil
	}

	// Tokenize the query into lowercase words.
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return all, nil
	}

	// Score each item.
	type scored struct {
		item  *Item
		score float64
	}

	var results []scored
	for _, item := range all {
		score := scoreItem(item, tokens)
		if score > 0 {
			results = append(results, scored{item: item, score: score})
		}
	}

	// Sort by score descending (insertion sort is fine for small lists).
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].score > results[j-1].score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	// Apply limit.
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	var items []*Item
	for _, r := range results {
		items = append(items, r.item)
	}

	return items, nil
}

// SearchByCategory searches within a specific category.
func (s *Store) SearchByCategory(category, query string, limit int) ([]*Item, error) {
	if category == "" {
		return s.Search(query, limit)
	}

	all, err := s.List(category)
	if err != nil {
		return nil, err
	}

	if query == "" {
		if limit > 0 && len(all) > limit {
			return all[:limit], nil
		}
		return all, nil
	}

	tokens := tokenize(query)

	type scored struct {
		item  *Item
		score float64
	}

	var results []scored
	for _, item := range all {
		score := scoreItem(item, tokens)
		if score > 0 {
			results = append(results, scored{item: item, score: score})
		}
	}

	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].score > results[j-1].score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	var items []*Item
	for _, r := range results {
		items = append(items, r.item)
	}

	return items, nil
}

// GetRecentByCategory returns the most recently updated items in a category.
func (s *Store) GetRecentByCategory(category string, limit int) ([]*Item, error) {
	var query string
	var args []interface{}

	if category != "" {
		query = `SELECT id, category, key, value, metadata, created_at, updated_at
		         FROM memory WHERE category = ? ORDER BY updated_at DESC LIMIT ?`
		args = append(args, category, limit)
	} else {
		query = `SELECT id, category, key, value, metadata, created_at, updated_at
		         FROM memory ORDER BY updated_at DESC LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Conn().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*Item
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.Category, &item.Key, &item.Value,
			&item.Metadata, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

func tokenize(text string) []string {
	words := strings.Fields(strings.ToLower(text))
	// Remove very short words and common stop words.
	stopWords := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "it": true,
		"in": true, "on": true, "at": true, "to": true, "for": true,
		"of": true, "and": true, "or": true, "but": true, "with": true,
		"by": true, "from": true, "this": true, "that": true, "be": true,
		"as": true, "are": true, "was": true, "were": true, "been": true,
		"has": true, "have": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"can": true, "may": true, "might": true, "shall": true,
		"i": true, "me": true, "my": true, "we": true, "our": true,
		"you": true, "your": true, "he": true, "she": true, "they": true,
	}

	var tokens []string
	for _, w := range words {
		if len(w) < 2 {
			continue
		}
		if stopWords[w] {
			continue
		}
		tokens = append(tokens, w)
	}
	return tokens
}

func scoreItem(item *Item, tokens []string) float64 {
	score := 0.0

	keyLower := strings.ToLower(item.Key)
	valueLower := strings.ToLower(item.Value)
	categoryLower := strings.ToLower(item.Category)

	for _, token := range tokens {
		// Key matches are worth the most.
		if strings.Contains(keyLower, token) {
			score += 3.0
		}
		// Category matches.
		if strings.Contains(categoryLower, token) {
			score += 2.0
		}
		// Value matches.
		if strings.Contains(valueLower, token) {
			score += 1.0
		}
	}

	// Exact key match bonus.
	for _, token := range tokens {
		if keyLower == token {
			score += 5.0
		}
	}

	return score
}
