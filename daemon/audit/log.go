// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Entry represents a single audit log entry.
type Entry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Category  string    `json:"category"`
	UserID    string    `json:"user_id,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	Details   string    `json:"details,omitempty"`
	RiskLevel string    `json:"risk_level"`
}

// Sink is an off-machine destination for audit entries. A compromised
// daemon can erase its own local audit_log table, so every entry is
// also streamed to a platform-side append-only sink the daemon can
// write but never read or delete (Phase 6). Implemented in package main
// (platformAuditSink) to keep this package dependency-free; injected via
// SetSink. Forward must be non-blocking — it never sits on the daemon's
// hot path.
type Sink interface {
	Forward(Entry)
}

// Logger writes structured audit log entries to the database and, if a
// sink is configured, mirrors them off-machine.
type Logger struct {
	db   *persistence.DB
	sink Sink
}

// NewLogger creates an audit logger backed by the given database.
func NewLogger(db *persistence.DB) *Logger {
	return &Logger{db: db}
}

// SetSink attaches the off-machine sink (Managed: on by default; BYOM:
// opt-in). Safe to call once at startup before serving.
func (l *Logger) SetSink(s Sink) { l.sink = s }

// Log records an audit entry.
func (l *Logger) Log(entry Entry) {
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	if entry.RiskLevel == "" {
		entry.RiskLevel = "low"
	}
	if entry.Category == "" {
		entry.Category = "general"
	}

	_, err := l.db.Conn().Exec(
		`INSERT INTO audit_log (id, timestamp, action, category, user_id, task_id, details, risk_level)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.Timestamp, entry.Action, entry.Category,
		entry.UserID, entry.TaskID, entry.Details, entry.RiskLevel,
	)
	if err != nil {
		log.Printf("audit: failed to log entry action=%s: %v", entry.Action, err)
	}

	// Mirror off-machine. Non-blocking by contract; the local row above
	// is authoritative for the daemon, the sink is the tamper-evident
	// record a compromised daemon cannot scrub.
	if l.sink != nil {
		l.sink.Forward(entry)
	}
}

// Query returns audit log entries with pagination.
func (l *Logger) Query(limit, offset int) ([]Entry, error) {
	rows, err := l.db.Conn().Query(
		`SELECT id, timestamp, action, category, user_id, task_id, details, risk_level
		 FROM audit_log ORDER BY timestamp DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var userID, taskID, details *string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Action, &e.Category,
			&userID, &taskID, &details, &e.RiskLevel); err != nil {
			return nil, err
		}
		if userID != nil {
			e.UserID = *userID
		}
		if taskID != nil {
			e.TaskID = *taskID
		}
		if details != nil {
			e.Details = *details
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// QueryByTask returns all audit entries for a specific task.
func (l *Logger) QueryByTask(taskID string) ([]Entry, error) {
	rows, err := l.db.Conn().Query(
		`SELECT id, timestamp, action, category, user_id, task_id, details, risk_level
		 FROM audit_log WHERE task_id = ? ORDER BY timestamp ASC`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var userID, tid, details *string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Action, &e.Category,
			&userID, &tid, &details, &e.RiskLevel); err != nil {
			return nil, err
		}
		if userID != nil {
			e.UserID = *userID
		}
		if tid != nil {
			e.TaskID = *tid
		}
		if details != nil {
			e.Details = *details
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// QueryByCategory returns entries filtered by category.
func (l *Logger) QueryByCategory(category string, limit int) ([]Entry, error) {
	rows, err := l.db.Conn().Query(
		`SELECT id, timestamp, action, category, user_id, task_id, details, risk_level
		 FROM audit_log WHERE category = ? ORDER BY timestamp DESC LIMIT ?`,
		category, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var userID, tid, details *string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Action, &e.Category,
			&userID, &tid, &details, &e.RiskLevel); err != nil {
			return nil, err
		}
		if userID != nil {
			e.UserID = *userID
		}
		if tid != nil {
			e.TaskID = *tid
		}
		if details != nil {
			e.Details = *details
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Count returns the total number of audit entries.
func (l *Logger) Count() (int, error) {
	var count int
	err := l.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&count)
	return count, err
}
