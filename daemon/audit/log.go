// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"log"
	"regexp"
	"time"

	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/google/uuid"
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
	db     *persistence.DB
	sink   Sink
	maskFn func(string) string
}

const maxAuditDetailsLen = 4096

var auditDetailRedactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s,;]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bvc_machine_[A-Za-z0-9_=-]+`), `vc_machine_[REDACTED]`},
	// OpenAI / Stripe restricted keys: sk-..., sk_live_..., sk_test_...,
	// rk_live_..., and project-scoped sk-proj-... all share the sk/rk prefix.
	{regexp.MustCompile(`\b(?:sk|rk)[_-](?:live|test|proj)[_-][A-Za-z0-9_-]{8,}`), `[REDACTED]`},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}`), `sk-[REDACTED]`},
	// AWS access key IDs (AKIA/ASIA + 16 base32 chars).
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), `[REDACTED]`},
	// Slack tokens (xoxb-, xoxp-, xoxa-, xoxr-, xoxs-).
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), `[REDACTED]`},
	// GitHub tokens (ghp_, gho_, ghu_, ghs_, ghr_, and github_pat_).
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), `[REDACTED]`},
	// PEM private-key blocks (RSA/EC/OPENSSH/generic), header through footer.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), `[REDACTED]`},
	// key=value form: password=..., token=..., api_key=..., etc.
	{regexp.MustCompile(`(?i)\b(password|passwd|token|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|key)=("[^"]*"|'[^']*'|[^\s,;]+)`), `${1}=[REDACTED]`},
	// key: value and "key": "value" forms (colon / JSON), which the
	// equals-only rule above misses entirely.
	{regexp.MustCompile(`(?i)("?\b(?:password|passwd|token|secret|api[_-]?key|access[_-]?key|secret[_-]?key|private[_-]?key|client[_-]?secret|key)"?\s*:\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`), `${1}[REDACTED]`},
}

// NewLogger creates an audit logger backed by the given database.
func NewLogger(db *persistence.DB) *Logger {
	return &Logger{db: db}
}

// SetSink attaches the off-machine sink (Managed: on by default; BYOM:
// opt-in). Safe to call once at startup before serving.
func (l *Logger) SetSink(s Sink) { l.sink = s }

// SetMasker attaches the vault masker that scrubs known secret VALUES from
// Details before the entry is written to the authoritative local audit_log
// row (the table the dashboard reads). The regex sanitizer only catches
// recognizable secret FORMATS; the vault masker catches the operator's
// actual stored secret values regardless of format. Kept as a function
// field so this package stays dependency-free (the masker lives in package
// vault and is injected from main, the same shape as the sink). Safe to
// call once at startup before serving; nil-safe in Log.
func (l *Logger) SetMasker(fn func(string) string) { l.maskFn = fn }

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
	entry.Details = l.sanitizeDetails(entry.Details)

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

func (l *Logger) sanitizeDetails(details string) string {
	// Mask the operator's actual stored secret VALUES first (vault masker,
	// when wired). This scrubs secrets the regex set below cannot recognize
	// by format. Applied to the authoritative local row, not just the
	// off-machine sink. Nil-safe so callers (and tests) without a masker
	// still work.
	if l.maskFn != nil {
		details = l.maskFn(details)
	}
	for _, r := range auditDetailRedactions {
		details = r.re.ReplaceAllString(details, r.repl)
	}
	if len(details) > maxAuditDetailsLen {
		return details[:maxAuditDetailsLen] + "...[truncated]"
	}
	return details
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
