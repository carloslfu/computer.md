// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Item represents a single memory entry.
type Item struct {
	ID        string    `json:"id"`
	Category  string    `json:"category"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Metadata  *string   `json:"metadata,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store handles CRUD operations for long-term agent memory.
type Store struct {
	db *persistence.DB
}

// NewStore creates a memory store backed by the given database.
func NewStore(db *persistence.DB) *Store {
	return &Store{db: db}
}

// Set creates or updates a memory entry. If a key already exists in the
// given category, its value is updated.
func (s *Store) Set(category, key, value string, metadata *string) (*Item, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	_, err := s.db.Conn().Exec(
		`INSERT INTO memory (id, category, key, value, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(category, key) DO UPDATE SET
		   value = excluded.value,
		   metadata = excluded.metadata,
		   updated_at = excluded.updated_at`,
		id, category, key, value, metadata, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("setting memory: %w", err)
	}

	// Return the actual stored item (may have a different ID if updated).
	return s.GetByKey(category, key)
}

// Get retrieves a memory item by ID.
func (s *Store) Get(id string) (*Item, error) {
	row := s.db.Conn().QueryRow(
		`SELECT id, category, key, value, metadata, created_at, updated_at
		 FROM memory WHERE id = ?`, id,
	)
	return scanItem(row)
}

// GetByKey retrieves a memory item by category and key.
func (s *Store) GetByKey(category, key string) (*Item, error) {
	row := s.db.Conn().QueryRow(
		`SELECT id, category, key, value, metadata, created_at, updated_at
		 FROM memory WHERE category = ? AND key = ?`, category, key,
	)
	return scanItem(row)
}

// Delete removes a memory item by ID.
func (s *Store) Delete(id string) error {
	result, err := s.db.Conn().Exec(`DELETE FROM memory WHERE id = ?`, id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("memory item not found: %s", id)
	}
	return nil
}

// List returns all memory items, optionally filtered by category.
func (s *Store) List(category string) ([]*Item, error) {
	var query string
	var args []interface{}

	if category != "" {
		query = `SELECT id, category, key, value, metadata, created_at, updated_at
		         FROM memory WHERE category = ? ORDER BY updated_at DESC`
		args = append(args, category)
	} else {
		query = `SELECT id, category, key, value, metadata, created_at, updated_at
		         FROM memory ORDER BY updated_at DESC`
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

// Categories returns all distinct memory categories.
func (s *Store) Categories() ([]string, error) {
	rows, err := s.db.Conn().Query(`SELECT DISTINCT category FROM memory ORDER BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []string
	for rows.Next() {
		var cat string
		if err := rows.Scan(&cat); err != nil {
			return nil, err
		}
		categories = append(categories, cat)
	}
	return categories, rows.Err()
}

// Count returns the total number of memory items.
func (s *Store) Count() (int, error) {
	var count int
	err := s.db.Conn().QueryRow(`SELECT COUNT(*) FROM memory`).Scan(&count)
	return count, err
}

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanItem(row scannable) (*Item, error) {
	var item Item
	err := row.Scan(&item.ID, &item.Category, &item.Key, &item.Value,
		&item.Metadata, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &item, nil
}
