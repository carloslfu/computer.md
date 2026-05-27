// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/carloslfu/computer.md/daemon/persistence"
)

// Workstream K — minimal fleet observability.
//
// /metrics exposes the four signal families the auth redesign
// introduces (JWKS health, cookie-auth, disk/engine, backups) in
// Prometheus exposition format. The platform's cron scraper hits this
// every 60s with the machine's healthToken (same auth as /health) and
// aggregates into the platform's machine_metrics table.
//
// Plain text emission, no client dependency. Counters live as global
// atomics; gauges read live state at scrape time.

// Counter and gauge state for /metrics. JWKS counters live in the
// jwks package (so the client can bump them locally); these track
// signals that originate elsewhere in the daemon.
var (
	mGrantConsumed     atomic.Int64
	mGrantNonceReplay  atomic.Int64
	mSessionRefresh    atomic.Int64
	mEnginePauseEvents atomic.Int64
	mBackupSuccessUnix atomic.Int64
	mBackupFailure     atomic.Int64

	// K6: per-call manager observability. Each successful API response
	// bumps these atomics by the four token kinds. Lets the platform watch
	// cache hit ratio + per-task token shape, and gives K7 a number to
	// measure pre/post K1+K3 tuning against.
	mManagerCacheReadTokens     atomic.Int64
	mManagerCacheCreationTokens atomic.Int64
	mManagerInputTokensFresh    atomic.Int64
	mManagerOutputTokens        atomic.Int64
	mManagerCallsTotal          atomic.Int64
)

// MetricsRecorder lets subsystems bump counters via a tiny indirection
// so tests can plug in a no-op. The default singleton writes the
// package-level atomics above.
type MetricsRecorder interface {
	GrantConsumed()
	GrantNonceReplay()
	SessionRefresh()
	EnginePauseEvent()
	BackupSuccess()
	BackupFailure()
	// K6 — bumped from the manager client after every successful API call. Fresh
	// is the portion of input_tokens that was NOT served from cache
	// (i.e., paid at full input rate); cache_read is the portion served
	// at cached-input rates; cache_creation is provider-specific.
	ManagerCall(cacheReadTokens, cacheCreationTokens, inputTokensFresh, outputTokens int)
}

type defaultRecorder struct{}

func (defaultRecorder) GrantConsumed()    { mGrantConsumed.Add(1) }
func (defaultRecorder) GrantNonceReplay() { mGrantNonceReplay.Add(1) }
func (defaultRecorder) SessionRefresh()   { mSessionRefresh.Add(1) }
func (defaultRecorder) EnginePauseEvent() { mEnginePauseEvents.Add(1) }
func (defaultRecorder) BackupSuccess()    { mBackupSuccessUnix.Store(time.Now().Unix()) }
func (defaultRecorder) BackupFailure()    { mBackupFailure.Add(1) }
func (defaultRecorder) ManagerCall(cacheRead, cacheCreation, inputFresh, output int) {
	mManagerCacheReadTokens.Add(int64(cacheRead))
	mManagerCacheCreationTokens.Add(int64(cacheCreation))
	mManagerInputTokensFresh.Add(int64(inputFresh))
	mManagerOutputTokens.Add(int64(output))
	mManagerCallsTotal.Add(1)
}

// defaultMetrics is the singleton; subsystems pick it up via
// the daemon's central Server struct (see s.metrics in main.go).
var defaultMetrics MetricsRecorder = defaultRecorder{}

// handleMetrics emits Prometheus exposition format. Authenticated via
// withHealthAuth so anyone with the machine's health token can scrape
// (the platform cron uses the same auth as /api/cron/health).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Counters — monotonically increasing since process start. Platform
	// computes rate() across scrapes for time-series.
	js := s.jwks.Stats()
	fmt.Fprintln(w, "# HELP vibecraft_jwks_refresh_total JWKS refresh attempts split by result.")
	fmt.Fprintln(w, "# TYPE vibecraft_jwks_refresh_total counter")
	fmt.Fprintf(w, "vibecraft_jwks_refresh_total{result=\"ok\"} %d\n", js.RefreshOK)
	fmt.Fprintf(w, "vibecraft_jwks_refresh_total{result=\"fail\"} %d\n", js.RefreshFail)

	fmt.Fprintln(w, "# HELP vibecraft_jwks_unknown_kid_total Tokens presented with a kid not in the cache.")
	fmt.Fprintln(w, "# TYPE vibecraft_jwks_unknown_kid_total counter")
	fmt.Fprintf(w, "vibecraft_jwks_unknown_kid_total %d\n", js.UnknownKid)

	// Gauge — age in seconds of the last successful JWKS refresh.
	fmt.Fprintln(w, "# HELP vibecraft_jwks_cache_age_seconds Age of the JWKS cache since the last successful refresh.")
	fmt.Fprintln(w, "# TYPE vibecraft_jwks_cache_age_seconds gauge")
	if !js.FetchedAt.IsZero() {
		fmt.Fprintf(w, "vibecraft_jwks_cache_age_seconds %d\n", int64(time.Since(js.FetchedAt).Seconds()))
	} else {
		// -1 = never fetched (boot bootstrap path only).
		fmt.Fprintln(w, "vibecraft_jwks_cache_age_seconds -1")
	}

	// Cookie auth.
	fmt.Fprintln(w, "# HELP vibecraft_grant_codes_consumed_total Successfully exchanged grant codes.")
	fmt.Fprintln(w, "# TYPE vibecraft_grant_codes_consumed_total counter")
	fmt.Fprintf(w, "vibecraft_grant_codes_consumed_total %d\n", mGrantConsumed.Load())

	fmt.Fprintln(w, "# HELP vibecraft_grant_nonce_replay_total Replayed grant nonces — security signal.")
	fmt.Fprintln(w, "# TYPE vibecraft_grant_nonce_replay_total counter")
	fmt.Fprintf(w, "vibecraft_grant_nonce_replay_total %d\n", mGrantNonceReplay.Load())

	fmt.Fprintln(w, "# HELP vibecraft_session_refresh_total Silent X-Vc-Refresh hints emitted to the SPA.")
	fmt.Fprintln(w, "# TYPE vibecraft_session_refresh_total counter")
	fmt.Fprintf(w, "vibecraft_session_refresh_total %d\n", mSessionRefresh.Load())

	// Live gauges — read at scrape time.
	fmt.Fprintln(w, "# HELP vibecraft_session_active Active (unexpired) browser sessions.")
	fmt.Fprintln(w, "# TYPE vibecraft_session_active gauge")
	fmt.Fprintf(w, "vibecraft_session_active %d\n", s.activeSessionCount())

	// Disk free ratio of the data dir, computed once per scrape.
	fmt.Fprintln(w, "# HELP vibecraft_disk_free_ratio Free space ratio (0..1) on the vibecraft data dir.")
	fmt.Fprintln(w, "# TYPE vibecraft_disk_free_ratio gauge")
	if ratio, ok := diskFreeRatio(s.cfg.DataDir); ok {
		fmt.Fprintf(w, "vibecraft_disk_free_ratio %.4f\n", ratio)
	}

	// Engine paused gauge.
	fmt.Fprintln(w, "# HELP vibecraft_engine_paused 1 if the engine loop is currently paused (e.g. low disk).")
	fmt.Fprintln(w, "# TYPE vibecraft_engine_paused gauge")
	paused := 0
	if s.engine != nil && s.engine.IsPaused() {
		paused = 1
	}
	fmt.Fprintf(w, "vibecraft_engine_paused %d\n", paused)

	fmt.Fprintln(w, "# HELP vibecraft_engine_pause_events_total Times the engine has paused since last boot.")
	fmt.Fprintln(w, "# TYPE vibecraft_engine_pause_events_total counter")
	fmt.Fprintf(w, "vibecraft_engine_pause_events_total %d\n", mEnginePauseEvents.Load())

	// Backups. The backup runner is the source of truth — it counts its
	// own attempts and timestamps the latest success. Fall back to zero
	// values if the runner failed to initialize.
	var lastSuccess int64
	var failureCount int64
	if s.backup != nil {
		lastSuccess = s.backup.LastSuccessUnix()
		failureCount = s.backup.FailureCount()
	}
	fmt.Fprintln(w, "# HELP vibecraft_backup_last_success_timestamp Unix seconds of the last successful local snapshot.")
	fmt.Fprintln(w, "# TYPE vibecraft_backup_last_success_timestamp gauge")
	fmt.Fprintf(w, "vibecraft_backup_last_success_timestamp %d\n", lastSuccess)

	fmt.Fprintln(w, "# HELP vibecraft_backup_failure_total Failed snapshot attempts.")
	fmt.Fprintln(w, "# TYPE vibecraft_backup_failure_total counter")
	fmt.Fprintf(w, "vibecraft_backup_failure_total %d\n", failureCount)

	// K6 — per-call manager observability. Cumulative since boot; the
	// platform's metrics cron derives rate() across scrapes to get
	// per-minute averages, cache hit ratio (cache_read / (cache_read +
	// input_fresh + cache_creation)), and effective per-call cost.
	fmt.Fprintln(w, "# HELP vibecraft_manager_calls_total Manager API calls completed since boot.")
	fmt.Fprintln(w, "# TYPE vibecraft_manager_calls_total counter")
	fmt.Fprintf(w, "vibecraft_manager_calls_total %d\n", mManagerCallsTotal.Load())
	fmt.Fprintln(w, "# HELP vibecraft_claude_calls_total Deprecated compatibility alias for manager API calls.")
	fmt.Fprintln(w, "# TYPE vibecraft_claude_calls_total counter")
	fmt.Fprintf(w, "vibecraft_claude_calls_total %d\n", mManagerCallsTotal.Load())

	fmt.Fprintln(w, "# HELP vibecraft_manager_cache_read_tokens_total Input tokens served from the provider prompt cache.")
	fmt.Fprintln(w, "# TYPE vibecraft_manager_cache_read_tokens_total counter")
	fmt.Fprintf(w, "vibecraft_manager_cache_read_tokens_total %d\n", mManagerCacheReadTokens.Load())

	fmt.Fprintln(w, "# HELP vibecraft_manager_cache_creation_tokens_total Input tokens written into the provider prompt cache when reported.")
	fmt.Fprintln(w, "# TYPE vibecraft_manager_cache_creation_tokens_total counter")
	fmt.Fprintf(w, "vibecraft_manager_cache_creation_tokens_total %d\n", mManagerCacheCreationTokens.Load())

	fmt.Fprintln(w, "# HELP vibecraft_manager_input_tokens_fresh_total Input tokens NOT served from cache.")
	fmt.Fprintln(w, "# TYPE vibecraft_manager_input_tokens_fresh_total counter")
	fmt.Fprintf(w, "vibecraft_manager_input_tokens_fresh_total %d\n", mManagerInputTokensFresh.Load())

	fmt.Fprintln(w, "# HELP vibecraft_manager_output_tokens_total Output tokens generated by the manager model.")
	fmt.Fprintln(w, "# TYPE vibecraft_manager_output_tokens_total counter")
	fmt.Fprintf(w, "vibecraft_manager_output_tokens_total %d\n", mManagerOutputTokens.Load())
}

// activeSessionCount queries unexpired sessions. SweepExpiredSessions
// keeps the table reasonably tight so this is cheap.
func (s *Server) activeSessionCount() int {
	n, err := s.db.CountActiveSessions(time.Now().UTC())
	if err != nil {
		return -1
	}
	return n
}

func diskFreeRatio(path string) (float64, bool) {
	if path == "" {
		return 0, false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, false
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize)
	if total == 0 {
		return 0, false
	}
	return float64(free) / float64(total), true
}

// Silence unused-import warning when only the type alias is referenced.
var _ persistence.Session
