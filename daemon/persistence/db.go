// SPDX-License-Identifier: Apache-2.0

package persistence

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

// DB wraps an encrypted SQLite database connection using SQLCipher.
type DB struct {
	conn *sql.DB
	mu   sync.Mutex
}

// Open creates or opens the SQLCipher database at path with the given
// encryption key. It applies the schema on first open.
func Open(path, encryptionKey string) (*DB, error) {
	dsn := fmt.Sprintf("%s?_pragma_key=%s&_pragma_cipher_page_size=4096", path, encryptionKey)
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// SQLite is single-writer. Serialize all access through one connection
	// to avoid SQLITE_BUSY contention between goroutines via CGO.
	conn.SetMaxOpenConns(1)

	// WAL mode: required for crash consistency. Multiple readers can run
	// alongside a writer without blocking each other.
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}

	// synchronous=NORMAL is the production-correct setting under WAL.
	// Standard recommendation; safe across crashes and the overwhelming
	// majority of power-loss scenarios. FULL would add bulletproof power-
	// loss safety at measurable write-cost, which is the wrong trade for
	// the data we store (chat history, vault, audit, sessions — important
	// but not financial-grade). Combined with rotating VACUUM-INTO backups
	// (see persistence/backup.go), worst-case recovery is "lose the last
	// few seconds of writes, restore from the most recent backup."
	if _, err := conn.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting synchronous=NORMAL: %w", err)
	}

	// Enable foreign keys.
	if _, err := conn.Exec("PRAGMA foreign_keys=ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}

	// Confirm the pragmas actually took effect — surfaces silent misconfig
	// (e.g. a SQLCipher build that didn't compile WAL support) at boot
	// rather than at corruption-recovery time.
	var journal, syncMode string
	_ = conn.QueryRow("PRAGMA journal_mode").Scan(&journal)
	_ = conn.QueryRow("PRAGMA synchronous").Scan(&syncMode)
	log.Printf("persistence: opened %s journal_mode=%s synchronous=%s", path, journal, syncMode)

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}

	return db, nil
}

// Close shuts down the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// Conn returns the underlying sql.DB for direct queries.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

// Lock acquires a mutex for write serialization when needed.
func (db *DB) Lock() {
	db.mu.Lock()
}

// Unlock releases the write mutex.
func (db *DB) Unlock() {
	db.mu.Unlock()
}

func (db *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		instruction TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'queued',
		result TEXT,
		error_message TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		started_at DATETIME,
		completed_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS conversations (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'text',
		image_data TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (conversation_id) REFERENCES conversations(id)
	);

	CREATE TABLE IF NOT EXISTS memory (
		id TEXT PRIMARY KEY,
		category TEXT NOT NULL DEFAULT 'general',
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		metadata TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(category, key)
	);

	CREATE TABLE IF NOT EXISTS audit_log (
		id TEXT PRIMARY KEY,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		action TEXT NOT NULL,
		category TEXT NOT NULL DEFAULT 'general',
		user_id TEXT,
		task_id TEXT,
		details TEXT,
		risk_level TEXT DEFAULT 'low'
	);

	CREATE TABLE IF NOT EXISTS guardrail_rules (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		pattern TEXT NOT NULL,
		action TEXT NOT NULL DEFAULT 'block',
		description TEXT,
		enabled INTEGER NOT NULL DEFAULT 1,
		priority INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		key_hash TEXT NOT NULL,
		key_hint TEXT NOT NULL,
		created_by TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_used_at TEXT,
		revoked_at TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_conversation ON tasks(conversation_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
	CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id);
	CREATE INDEX IF NOT EXISTS idx_memory_category ON memory(category);
	CREATE INDEX IF NOT EXISTS idx_memory_key ON memory(key);
	CREATE INDEX IF NOT EXISTS idx_audit_log_timestamp ON audit_log(timestamp);
	CREATE INDEX IF NOT EXISTS idx_audit_log_action ON audit_log(action);
	CREATE INDEX IF NOT EXISTS idx_audit_log_task ON audit_log(task_id);
	CREATE INDEX IF NOT EXISTS idx_audit_log_user ON audit_log(user_id);
	CREATE TABLE IF NOT EXISTS routes (
		name TEXT PRIMARY KEY,
		port INTEGER NOT NULL,
		sso_enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

	-- Browser sessions: cookie-based auth for the per-machine chat UI.
	-- Cookie carries the raw 32-byte session id; the daemon stores only
	-- SHA-256(raw) so a DB compromise can't exfiltrate live cookies.
	-- Same hygiene as api_keys.keyHash.
	CREATE TABLE IF NOT EXISTS sessions (
		id_hash     TEXT PRIMARY KEY,
		sub         TEXT NOT NULL,
		name        TEXT NOT NULL DEFAULT '',
		email       TEXT NOT NULL DEFAULT '',
		access      TEXT NOT NULL,
		created_at  TIMESTAMP NOT NULL,
		expires_at  TIMESTAMP NOT NULL,
		last_seen   TIMESTAMP NOT NULL,
		ip          TEXT,
		user_agent  TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_sub ON sessions(sub);
	CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

	-- Single-use enforcement for grant codes. Persistent so an attacker
	-- can't replay a captured grant code after a daemon restart.
	CREATE TABLE IF NOT EXISTS grant_nonces (
		nonce       TEXT PRIMARY KEY,
		consumed_at TIMESTAMP NOT NULL,
		expires_at  TIMESTAMP NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_grant_nonces_expires_at ON grant_nonces(expires_at);

	-- Subdomain-scoped SSO sessions for internal hosted apps. Same shape
	-- as sessions, separate table so SSO can be revoked independently of
	-- the chat session.
	CREATE TABLE IF NOT EXISTS sso_sessions (
		id_hash     TEXT PRIMARY KEY,
		sub         TEXT NOT NULL,
		name        TEXT NOT NULL DEFAULT '',
		email       TEXT NOT NULL DEFAULT '',
		access      TEXT NOT NULL,
		created_at  TIMESTAMP NOT NULL,
		expires_at  TIMESTAMP NOT NULL,
		last_seen   TIMESTAMP NOT NULL,
		ip          TEXT,
		user_agent  TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_sso_sessions_sub ON sso_sessions(sub);
	CREATE INDEX IF NOT EXISTS idx_sso_sessions_expires_at ON sso_sessions(expires_at);

	-- ── AI usage records ──
	-- Persistent, append-with-upsert token accumulator for every manager
	-- API call the daemon makes. Keyed by (day, model, conversation_id)
	-- so the table grows O(distinct_days × distinct_models × distinct_chats)
	-- rather than O(API_calls) — a busy machine still produces a manageable
	-- few-rows-per-day table after a year of use.
	--
	-- conversation_id defaults to empty string for calls that aren't
	-- bound to a chat (compactor, summarizer when called outside a task
	-- context). Storing empty rather than NULL keeps the PRIMARY KEY
	-- definition straightforward (NULLs don't participate in uniqueness
	-- the way you'd expect in SQLite).
	--
	-- Why this lives on the daemon, not the platform: usage is a
	-- per-machine fact. The platform owns plan budgets (which need cross-
	-- machine coordination); the daemon owns consumption. The UI joins
	-- them at render time.
	CREATE TABLE IF NOT EXISTS usage_records (
		day                  TEXT    NOT NULL,
		model                TEXT    NOT NULL,
		conversation_id      TEXT    NOT NULL DEFAULT '',
		input_tokens         INTEGER NOT NULL DEFAULT 0,
		output_tokens        INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens    INTEGER NOT NULL DEFAULT 0,
		cache_create_tokens  INTEGER NOT NULL DEFAULT 0,
		updated_at           DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (day, model, conversation_id)
	);
	CREATE INDEX IF NOT EXISTS idx_usage_records_day ON usage_records(day);
	CREATE INDEX IF NOT EXISTS idx_usage_records_convo ON usage_records(conversation_id);
	`
	_, err := db.conn.Exec(schema)
	if err != nil {
		return err
	}

	// Add user_id column to existing audit_log tables (no-op if already present).
	db.conn.Exec(`ALTER TABLE audit_log ADD COLUMN user_id TEXT`)

	// ── Audit log: append-only at the SQL layer ──
	// The audit log is the historical record of every guardrail decision,
	// approval, denial, and override that happened on this machine. It
	// must be immutable; an attacker (or a bug) that could rewrite
	// history would defeat the entire point of auditing.
	//
	// SQLite triggers reject UPDATE and DELETE on the audit_log table.
	// Daemon code only ever appends (audit.Log calls INSERT), so this
	// constraint doesn't affect normal operation. If we ever need to
	// rotate/archive old entries, we'll add an explicit migration with
	// a temporary DROP TRIGGER scope — not a sneaky UPDATE.
	//
	// CREATE TRIGGER IF NOT EXISTS is idempotent across daemon restarts.
	_, _ = db.conn.Exec(`
		CREATE TRIGGER IF NOT EXISTS audit_log_no_update
		BEFORE UPDATE ON audit_log
		BEGIN
			SELECT RAISE(ABORT, 'audit_log is append-only — UPDATE is not permitted');
		END;
		CREATE TRIGGER IF NOT EXISTS audit_log_no_delete
		BEFORE DELETE ON audit_log
		BEGIN
			SELECT RAISE(ABORT, 'audit_log is append-only — DELETE is not permitted');
		END;
	`)

	// Add type and image_data columns to existing messages tables.
	db.conn.Exec(`ALTER TABLE messages ADD COLUMN type TEXT NOT NULL DEFAULT 'text'`)
	db.conn.Exec(`ALTER TABLE messages ADD COLUMN image_data TEXT`)

	// Attachments: JSON array of {name, path, mime, size} for files the user
	// uploaded alongside a message. Stored as JSON text on the message row
	// rather than a separate table — the N is tiny per message, and reads
	// always fetch them together with the message.
	db.conn.Exec(`ALTER TABLE messages ADD COLUMN attachments TEXT`)

	// Workstream H: hosted apps registered via /api/routes carry a
	// per-app sso_enabled flag (defaults to ON). Existing tables get the
	// column added in-place; SQLite ignores duplicate-add errors so
	// this is idempotent across restarts.
	db.conn.Exec(`ALTER TABLE routes ADD COLUMN sso_enabled INTEGER NOT NULL DEFAULT 1`)

	return nil
}

// APIKey represents a machine-scoped API key stored in the local database.
type APIKey struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	KeyHash    string  `json:"-"`
	KeyHint    string  `json:"hint"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
}

// CreateAPIKey inserts a new API key record.
func (db *DB) CreateAPIKey(id, name, keyHash, keyHint, createdBy string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.conn.Exec(
		`INSERT INTO api_keys (id, name, key_hash, key_hint, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, keyHash, keyHint, createdBy, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// ListAPIKeys returns all API keys (never includes the hash).
func (db *DB) ListAPIKeys() ([]APIKey, error) {
	rows, err := db.conn.Query(
		`SELECT id, name, key_hint, created_by, created_at, last_used_at FROM api_keys WHERE revoked_at IS NULL ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHint, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetAPIKeyByHash looks up a non-revoked API key by its SHA-256 hash.
func (db *DB) GetAPIKeyByHash(keyHash string) (*APIKey, error) {
	var k APIKey
	err := db.conn.QueryRow(
		`SELECT id, name, key_hash, key_hint, created_by, created_at, last_used_at FROM api_keys WHERE key_hash = ? AND revoked_at IS NULL`,
		keyHash,
	).Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyHint, &k.CreatedBy, &k.CreatedAt, &k.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// UpdateAPIKeyLastUsed sets the last_used_at timestamp for a key.
func (db *DB) UpdateAPIKeyLastUsed(id string) error {
	_, err := db.conn.Exec(
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id,
	)
	return err
}

// Route represents a registered app route mapping a subdomain name to a local port.
type Route struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	SSOEnabled bool   `json:"sso_enabled"`
	CreatedAt  string `json:"created_at"`
}

// CreateRoute inserts a new route or updates an existing one. New rows
// default to sso_enabled=1 per the schema; updates leave that column
// untouched so the operator's toggle in the hosted-apps panel survives
// agent-driven re-registration.
func (db *DB) CreateRoute(name string, port int) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.conn.Exec(
		`INSERT INTO routes (name, port, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET port = excluded.port`,
		name, port, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// ListRoutes returns all registered routes.
func (db *DB) ListRoutes() ([]Route, error) {
	rows, err := db.conn.Query(`SELECT name, port, COALESCE(sso_enabled, 1), created_at FROM routes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []Route
	for rows.Next() {
		var r Route
		var sso int
		if err := rows.Scan(&r.Name, &r.Port, &sso, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.SSOEnabled = sso != 0
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

// GetRoute returns a single route by name.
func (db *DB) GetRoute(name string) (*Route, error) {
	var r Route
	var sso int
	err := db.conn.QueryRow(
		`SELECT name, port, COALESCE(sso_enabled, 1), created_at FROM routes WHERE name = ?`, name,
	).Scan(&r.Name, &r.Port, &sso, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	r.SSOEnabled = sso != 0
	return &r, nil
}

// SetRouteSSOEnabled toggles the per-app SSO flag. Operator-driven; only
// the hosted-apps panel calls this.
func (db *DB) SetRouteSSOEnabled(name string, enabled bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	v := 0
	if enabled {
		v = 1
	}
	res, err := db.conn.Exec(
		`UPDATE routes SET sso_enabled = ? WHERE name = ?`,
		v, name,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("route not found")
	}
	return nil
}

// DeleteRoute removes a route by name.
func (db *DB) DeleteRoute(name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.conn.Exec(`DELETE FROM routes WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("route not found")
	}
	return nil
}

// RevokeAPIKey sets the revoked_at timestamp, effectively disabling the key.
func (db *DB) RevokeAPIKey(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.conn.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key not found or already revoked")
	}
	return nil
}
