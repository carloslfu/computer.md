// SPDX-License-Identifier: Apache-2.0

package persistence

import (
	"database/sql"
	"errors"
	"time"
)

// ErrSessionNotFound is returned when a session lookup misses or the
// session has expired. Wrapping sql.ErrNoRows so callers can use
// errors.Is for either.
var ErrSessionNotFound = errors.New("session not found")

// Session is a daemon-side authenticated browser session. The cookie
// holds the raw id; this row keys on SHA-256(raw id) so DB compromise
// can't exfiltrate live cookies.
type Session struct {
	IDHash    string
	Sub       string
	Name      string
	Email     string
	Access    string // "control" | "view"
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	IP        string
	UserAgent string
}

// CreateSession inserts a session row. Pass the SHA-256 hash of the
// raw cookie value as idHash — never the raw value itself.
func (db *DB) CreateSession(s Session) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.conn.Exec(
		`INSERT INTO sessions
		 (id_hash, sub, name, email, access, created_at, expires_at, last_seen, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.IDHash, s.Sub, s.Name, s.Email, s.Access,
		s.CreatedAt, s.ExpiresAt, s.LastSeen, s.IP, s.UserAgent,
	)
	return err
}

// GetSessionByIDHash returns a session by its hash, or ErrSessionNotFound
// if missing OR expired.
func (db *DB) GetSessionByIDHash(idHash string) (*Session, error) {
	var s Session
	err := db.conn.QueryRow(
		`SELECT id_hash, sub, name, email, access, created_at, expires_at, last_seen,
		        COALESCE(ip, ''), COALESCE(user_agent, '')
		 FROM sessions
		 WHERE id_hash = ? AND expires_at > ?`,
		idHash, time.Now().UTC(),
	).Scan(&s.IDHash, &s.Sub, &s.Name, &s.Email, &s.Access,
		&s.CreatedAt, &s.ExpiresAt, &s.LastSeen, &s.IP, &s.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// TouchSession bumps last_seen for a session. Best-effort — silent on
// missing rows because the middleware is on the request hot path and
// shouldn't introduce extra error surfaces.
func (db *DB) TouchSession(idHash string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.conn.Exec(
		`UPDATE sessions SET last_seen = ? WHERE id_hash = ?`,
		time.Now().UTC(), idHash,
	)
	return err
}

// DeleteSession removes a session row by hash (logout path).
func (db *DB) DeleteSession(idHash string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.conn.Exec(`DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteAllSessions wipes every browser session. Called from the
// management channel during incident-response rotation; operators
// re-handshake on the next request and get cookies signed with the new
// kid. Returns the number of rows deleted.
func (db *DB) DeleteAllSessions() (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.conn.Exec(`DELETE FROM sessions`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteSessionsBySub removes every browser session belonging to one user
// (WorkOS sub). Called from the management channel when the platform
// offboards a user (control-revoke, team removal, deactivation): the cookie
// bakes in the access tier at handshake with a multi-hour expiry and is never
// re-checked against platform access, so without this a removed teammate keeps
// full dashboard control until the cookie expires. Returns rows deleted.
func (db *DB) DeleteSessionsBySub(sub string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.conn.Exec(`DELETE FROM sessions WHERE sub = ?`, sub)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountActiveSessions returns the number of unexpired browser sessions
// at the given cutoff (typically time.Now().UTC()). Used by /metrics
// (Workstream K) to publish vibecraft_session_active as a gauge.
func (db *DB) CountActiveSessions(now time.Time) (int, error) {
	var n int
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE expires_at > ?`,
		now,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CreateSSOSession inserts an SSO session row (parallel to sessions
// but for the subdomain-wide vc_sso cookie used by internal apps).
func (db *DB) CreateSSOSession(s Session) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.conn.Exec(
		`INSERT INTO sso_sessions
		 (id_hash, sub, name, email, access, created_at, expires_at, last_seen, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.IDHash, s.Sub, s.Name, s.Email, s.Access,
		s.CreatedAt, s.ExpiresAt, s.LastSeen, s.IP, s.UserAgent,
	)
	return err
}

// GetSSOSessionByIDHash mirrors GetSessionByIDHash for the SSO table.
func (db *DB) GetSSOSessionByIDHash(idHash string) (*Session, error) {
	var s Session
	err := db.conn.QueryRow(
		`SELECT id_hash, sub, name, email, access, created_at, expires_at, last_seen,
		        COALESCE(ip, ''), COALESCE(user_agent, '')
		 FROM sso_sessions
		 WHERE id_hash = ? AND expires_at > ?`,
		idHash, time.Now().UTC(),
	).Scan(&s.IDHash, &s.Sub, &s.Name, &s.Email, &s.Access,
		&s.CreatedAt, &s.ExpiresAt, &s.LastSeen, &s.IP, &s.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteSSOSession removes an SSO session row.
func (db *DB) DeleteSSOSession(idHash string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.conn.Exec(`DELETE FROM sso_sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteSSOSessionsBySub removes every SSO session for one user (WorkOS sub) —
// companion to DeleteSessionsBySub. The vc_sso cookie grants subdomain-wide
// access to internal apps, so an offboarded user's SSO session must be cut
// alongside their main session. Returns rows deleted.
func (db *DB) DeleteSSOSessionsBySub(sub string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.conn.Exec(`DELETE FROM sso_sessions WHERE sub = ?`, sub)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAllSSOSessions wipes every SSO session — companion to
// DeleteAllSessions for incident rotation.
func (db *DB) DeleteAllSSOSessions() (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.conn.Exec(`DELETE FROM sso_sessions`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ConsumeGrantNonce records a grant code's nonce as used. Returns
// ErrNonceAlreadyConsumed if the nonce was already in the table — that
// signals a replayed grant code, which the caller must reject. Inserts
// the nonce atomically with its expiry; the hourly sweep cleans up.
var ErrNonceAlreadyConsumed = errors.New("grant nonce already consumed")

func (db *DB) ConsumeGrantNonce(nonce string, exp time.Time) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.conn.Exec(
		`INSERT INTO grant_nonces (nonce, consumed_at, expires_at) VALUES (?, ?, ?)`,
		nonce, time.Now().UTC(), exp,
	)
	if err != nil && isUniqueConstraintErr(err) {
		return ErrNonceAlreadyConsumed
	}
	return err
}

func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	// SQLCipher / SQLite error string. Cheap substring check — gives us
	// a stable signal across driver versions without importing the
	// driver-specific error type.
	msg := err.Error()
	for _, marker := range []string{"UNIQUE constraint", "constraint failed: nonce", "1555"} {
		if contains(msg, marker) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// SweepExpiredSessions deletes any session / sso_session / grant_nonce
// rows past their expires_at. Returns the total number of rows removed
// (for logging / audit).
func (db *DB) SweepExpiredSessions() (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	now := time.Now().UTC()

	var total int64
	for _, q := range []string{
		`DELETE FROM sessions     WHERE expires_at < ?`,
		`DELETE FROM sso_sessions WHERE expires_at < ?`,
		`DELETE FROM grant_nonces WHERE expires_at < ?`,
	} {
		res, err := db.conn.Exec(q, now)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
