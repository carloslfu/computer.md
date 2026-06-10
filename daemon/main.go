// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "image/png"

	"github.com/carloslfu/computer.md/daemon/audit"
	"github.com/carloslfu/computer.md/daemon/computer"
	"github.com/carloslfu/computer.md/daemon/core"
	"github.com/carloslfu/computer.md/daemon/guardrails"
	"github.com/carloslfu/computer.md/daemon/jwks"
	managerclient "github.com/carloslfu/computer.md/daemon/manager"
	"github.com/carloslfu/computer.md/daemon/memory"
	"github.com/carloslfu/computer.md/daemon/persistence"
	"github.com/carloslfu/computer.md/daemon/routes"
	"github.com/carloslfu/computer.md/daemon/sandbox"
	"github.com/carloslfu/computer.md/daemon/usage"
	"github.com/carloslfu/computer.md/daemon/vault"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// embeddedPrompt is the system prompt compiled into the binary.
// Single source of truth: daemon/prompt.md. Version-locked to the binary.
//
//go:embed prompt.md
var embeddedPrompt []byte

var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version)
		os.Exit(0)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("vibecraft daemon %s starting", version)

	// Self-heal missing baseline tools (xterm). Has to run before the
	// HTTP server opens for traffic — otherwise a first agent call can
	// race in while apt is still pulling the package down and the
	// "command -v xterm" check in the prompt returns false. Blocking
	// adds <1s if everything is already in place (the common path) and
	// up to 30s when an actual install is needed.
	bootstrapTools()

	// Drop the embedded brand.md to /etc/vibecraft/brand.md so the
	// manager (and any worker) can read the latest design baseline
	// without a cloud-init refresh. Idempotent overwrite — older
	// versions stamped on the box never outlive their daemon update.
	writeBrandFile()

	startTime := time.Now()

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Off by default, opt-in via VIBECRAFT_TELEMETRY=on. Sends nothing
	// more than version/os/arch once a week. See daemon/telemetry.go.
	startTelemetry(context.Background(), cfg.PlatformBaseURL, version)

	// Open encrypted database.
	db, err := persistence.Open(cfg.DBPath, cfg.DBEncryptionKey)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Initialize subsystems.
	taskStore := core.NewTaskStore(db)
	memStore := memory.NewStore(db)

	// Vault + masker are created before the audit sink so the sink can
	// scrub secret values out of audit Details on the last hop off-machine.
	vaultStore, err := vault.NewStore(cfg.VaultPath, cfg.VaultEncryptionKey)
	if err != nil {
		log.Fatalf("failed to open vault: %v", err)
	}
	vaultMasker := vault.NewMasker(vaultStore)

	auditLog := audit.NewLogger(db)
	if cfg.AuditSink {
		auditLog.SetSink(newPlatformAuditSink(cfg, vaultMasker))
		log.Printf("Phase 6: remote audit sink enabled (write-only stream to platform)")
	}
	grEngine := guardrails.NewEngine(db)

	managerClient := managerclient.NewClientWithOptions(cfg.OpenAIKey, managerclient.ClientOptions{
		Model:               cfg.ManagerModel,
		ReasoningEffort:     cfg.ManagerReasoningEffort,
		MaxOutputTokens:     cfg.ManagerMaxOutputTokens,
		DeepReasoningEffort: cfg.ManagerDeepReasoningEffort,
		DeepMaxOutputTokens: cfg.ManagerDeepMaxOutputTokens,
	})
	// K6: wire the per-call observability hook so every successful manager
	// API response bumps the token counters exposed at /metrics.
	managerclient.MetricsHook = func(cacheRead, cacheCreation, inputFresh, output int) {
		defaultMetrics.ManagerCall(cacheRead, cacheCreation, inputFresh, output)
	}

	ctrl := computer.NewController()
	ssService := computer.NewScreenshotService(cfg.ScreenshotDir)
	shell := computer.NewShell("/home/vibecraft", "vibecraft")

	// System prompt is embedded in the binary (see embeddedPrompt below).
	// This ensures the prompt is always present, version-locked to the
	// binary, and can't get out of sync with a separate file on disk.
	promptData := embeddedPrompt

	// SSE broker.
	broker := NewSSEBroker()

	// Agent engine.
	engine := core.NewEngine(
		taskStore, managerClient, ctrl, ssService, shell,
		grEngine, memStore, vaultStore, vaultMasker, auditLog,
		string(promptData), broker,
	)

	// Wire the post-task activity summarizer. The summarizer receives the
	// manager client directly (not via the engine's narrow ManagerAPI interface)
	// so adding SendText doesn't force test doubles to widen.
	summarizer := core.NewSummarizer(managerClient, taskStore, auditLog, memStore, vaultMasker, broker)
	engine.SetSummarizer(summarizer)

	// Wire the mid-task compactor. When a single task's input crosses the
	// token threshold (140k), the compactor collapses old turns into a
	// structured summary so the next iteration stays under the context
	// threshold. Uses the same manager model as the agent loop because the
	// summary quality replaces the agent's working memory.
	compactor := core.NewCompactor(managerClient, "", broker)
	engine.SetCompactor(compactor)

	// Wire the AI usage accumulator + budget enforcement. Every manager
	// call (engine, compactor, summarizer) records through the
	// BudgetTracker: it forwards the token counts to the per-machine
	// usage Store AND checks the budget thresholds, firing the 80% /
	// 100% notifications. The BudgetTracker also fetches the customer's
	// monthly AI credit budget from the platform (hourly) so the
	// task-submission gate can hold new tasks once the budget is spent.
	usageStore := usage.NewStore(db)
	budgetTracker := usage.NewBudgetTracker(
		usageStore, cfg.MachineID, cfg.HealthToken, cfg.PlatformBaseURL,
	)
	budgetTracker.SetManagerKeyMode(cfg.ManagerKeyMode)
	budgetTracker.SetNotifier(func(kind, title, body, priority string) {
		if err := postNotificationToPlatform(cfg, kind, title, body, "", priority); err != nil {
			log.Printf("budget: notification forward failed: %v", err)
		}
	})
	engine.SetUsageRecorder(budgetTracker)
	// Enforce pause-at-zero at the engine consumer too (defense in depth):
	// stop claiming new tasks when the AI budget is exhausted.
	engine.SetBudgetGate(func() bool {
		st, err := budgetTracker.State()
		return err == nil && st.Paused
	})
	compactor.SetUsageRecorder(budgetTracker)
	summarizer.SetUsageRecorder(budgetTracker)

	// Wire the auto-review classifier. Runs against the manager model between
	// the regex-based guardrail engine's Confirm decision and the human
	// approval card — downgrades obvious-safe variants of risky-looking
	// patterns (e.g. `curl URL | python3 -c '<bs4>'` is fine; `curl URL
	// | sh` is not) to silent auto-approval. Failures degrade to
	// escalation, so this strictly improves UX without softening
	// real-risk gates.
	classifier := guardrails.NewRiskClassifier(managerClient, "")
	engine.SetRiskClassifier(classifier)

	// Route manager for customer app hosting.
	routeMgr := routes.NewManager(db, cfg.MachineHost)
	go func() {
		if err := routeMgr.SyncCaddyfile(); err != nil {
			log.Printf("warning: Caddyfile sync failed: %v", err)
		}
	}()

	// Wire the cross-machine notifier into the engine so disk-full /
	// other system-level events surface to the operator via the
	// platform's notification ingest. Same path the manager uses via
	// the /notify HTTP endpoint, just called in-process.
	engine.SetNotifier(&platformNotifier{cfg: cfg})

	// Periodic VACUUM INTO backups of the encrypted core DB. Rotates to
	// keep ~28 days of history (7 recent + 4 weekly) and runs every 4
	// hours. Local-only by default; an Uploader (Workstream B.1 Managed
	// S3 push) can be set later once the platform bucket is wired up.
	backupRunner, err := persistence.NewBackup(db, filepath.Join(cfg.DataDir, "backups"), 4*time.Hour)
	if err != nil {
		log.Printf("warning: backup runner init failed: %v", err)
	}

	// Off-machine push for Managed machines (Workstream B.1 / Track H).
	// Both AWS and Hetzner machines push to the same S3 bucket
	// (vibecraft-backups-prod) under their own machineId prefix. Cloud-init
	// bakes the AWS_* creds into the environment on managed machines;
	// BYOM machines leave them unset and stay local-only.
	if backupRunner != nil && os.Getenv("VIBECRAFT_BACKUPS_BUCKET") != "" {
		uploader, err := persistence.NewS3Uploader(
			context.Background(),
			os.Getenv("VIBECRAFT_BACKUPS_BUCKET"),
			cfg.MachineID,
			os.Getenv("AWS_REGION"),
			os.Getenv("AWS_ACCESS_KEY_ID"),
			os.Getenv("AWS_SECRET_ACCESS_KEY"),
		)
		if err != nil {
			log.Printf("warning: s3 backup uploader init failed: %v — backups stay local-only", err)
		} else {
			backupRunner.SetUploader(uploader)
			log.Printf("backups: S3 push enabled to bucket=%s prefix=%s/", os.Getenv("VIBECRAFT_BACKUPS_BUCKET"), cfg.MachineID)
		}
	}

	// Start the engine processing loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)
	if backupRunner != nil {
		backupRunner.Start(ctx)
	}

	auditLog.Log(audit.Entry{
		Action:   "daemon_started",
		Category: "system",
		Details:  fmt.Sprintf("machine=%s port=%d", cfg.MachineID, cfg.Port),
	})

	// JWKS verification client. Pre-populates the cache with the on-disk
	// PEM under kid="vibecraft-1" so verification works offline from boot.
	// Subsequent platform refreshes add new kids (rotation without redeploy).
	jwksClient := jwks.NewClient(
		cfg.PlatformBaseURL+"/.well-known/jwks.json",
		"vibecraft-1",
		cfg.JWTPublicKey,
	)

	// AI credits proxy: per-machine bearer token + monthly budget
	// ledger. Initialized BEFORE the Server struct so its handlers
	// can reference them at registration time. Failure to load the
	// token (e.g. /etc/vibecraft not writable on a dev box) is
	// non-fatal — the proxy endpoint will return 401 for every
	// request and the rest of the daemon keeps working.
	aiTokVal, aiTokErr := loadOrCreateAICreditsToken()
	if aiTokErr != nil {
		log.Printf("ai_proxy: could not initialize credits token: %v (proxy will reject all requests)", aiTokErr)
	}
	aiTok := &AICreditsToken{value: aiTokVal}
	bsvc := newBudgetService()

	// Link the AI-credits proxy to the plan-aware pooled budget + spend
	// reporting (daemon-manager-ai-1 / -2):
	//   • The proxy gates on the BudgetTracker's pooled remaining budget — the
	//     authoritative, plan-sized, top-up-aware ceiling — instead of the
	//     hardcoded $50 static cap (which nothing ever wrote, so every plan was
	//     capped at $50). Before the first budget fetch the static cap stands
	//     as a startup safety net.
	//   • The tracker reports the proxy ledger's spend to the platform, so
	//     hosted-tool AI bills back at cost like manager turns instead of being
	//     silently absorbed.
	bsvc.SetPooledBudget(func() (int, bool, bool) {
		snap := budgetTracker.Snapshot()
		if snap == nil {
			return 0, false, false // no fetch yet → fall back to the static cap
		}
		if snap.BudgetUSD < 0 {
			return 0, true, true // -1 = unmetered (Enterprise)
		}
		return int(snap.BudgetUSD*100 + 0.5), false, true
	})
	budgetTracker.SetProxySpend(bsvc.currentSpentCents)

	// Build HTTP server with all routes.
	srv := &Server{
		cfg:           cfg,
		db:            db,
		taskStore:     taskStore,
		memStore:      memStore,
		auditLog:      auditLog,
		grEngine:      grEngine,
		vaultStore:    vaultStore,
		vaultMask:     vaultMasker,
		manager:       managerClient,
		ctrl:          ctrl,
		screenshot:    ssService,
		shell:         shell,
		engine:        engine,
		broker:        broker,
		routeMgr:      routeMgr,
		jwks:          jwksClient,
		backup:        backupRunner,
		usageStore:    usageStore,
		budgetTracker: budgetTracker,
		startTime:     startTime,

		aiCreditsToken: aiTok,
		budget:         bsvc,
	}

	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	// ── Phase 2: the agent's whole world runs inside ONE long-lived
	// agent-shell sandbox. $HOME is backed by the real /home/vibecraft
	// (systems, ~/.claude, ~/inbox, tools persist); the shared Xvfb
	// socket is bound (xterm/Chrome still draw); egress is audit-mode
	// (open web, logged) but the netns severs host 127.0.0.1:8420 — so
	// the agent reaches the daemon via the per-sandbox /run/vibecraft.sock
	// (handler = this mux; socket-implicit auth). No host .bashrc key.
	// Platform-keyed Managed machines fail closed if this sandbox is missing.
	// Operator-owned BYOM/self-host and macOS dev can fall back to host bash.
	if sandbox.Supported() {
		// $HOME stays /home/vibecraft (the home is bound at the SAME
		// path inside the sandbox) so every daemon-supplied absolute
		// path (inbox attachments, ~/systems) resolves identically.
		ashEnv := map[string]string{
			"HOME":            "/home/vibecraft",
			"USER":            "vibecraft",
			"LOGNAME":         "vibecraft",
			"SHELL":           "/bin/bash",
			"DISPLAY":         ":1",
			"XAUTHORITY":      "/home/vibecraft/.Xauthority",
			"LANG":            "en_US.UTF-8",
			"TERM":            "xterm-256color",
			"PATH":            "/home/vibecraft/.npm-global/bin:/home/vibecraft/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"XDG_RUNTIME_DIR": "/run/user/1000",
		}
		// Codex (the peer worker) authenticates through its own
		// `codex login` (ChatGPT subscription) flow; the token lands
		// in ~/.codex/ and the daemon does not inject an OpenAI key
		// into the agent shell. Claude Code and Codex authenticate through
		// subscription/session state or visible login prompts, not through
		// VibeCraft-hosted provider keys.
		ash, ashErr := sandbox.NewAgentShell("/home/vibecraft", "/tmp/.X11-unix/X1", nil, ashEnv, mux)
		if ashErr != nil {
			if cfg.ManagerKeyMode == "platform" {
				log.Fatalf("agent-shell sandbox unavailable in platform key mode: %v", ashErr)
			}
			log.Printf("agent-shell sandbox unavailable in operator key mode, falling back to host bash: %v", ashErr)
		} else {
			engine.SetAgentShell(ash)
			defer ash.Close()
			log.Printf("Phase 2: agent bash routed through the agent-shell sandbox (socket %s)", ash.Path())
		}
		// Phase 3: when a discovery-mode (unmanifested) system's
		// observation window closes, propose a locked egress manifest.
		// On-brand realization (CLAUDE.md "the customer talks to the
		// manager", "build less"): write a reviewable proposed manifest
		// next to the system, push ONE customer notification, audit it.
		// The customer approves by telling the manager, which writes
		// manifest.json + replies — no bespoke dashboard card.
		sandbox.SystemProposalHook = func(system string, observed []string) {
			systemsRoot := "/home/vibecraft/systems"
			proposed := map[string]any{
				"allow_fqdns":    observed,
				"enforce_egress": false, // customer reviews, then flips to enforce
			}
			pj, _ := json.MarshalIndent(proposed, "", "  ")
			_ = os.WriteFile(filepath.Join(systemsRoot, system, "manifest.proposed.json"), pj, 0644)
			body := fmt.Sprintf("System %q has reached %d host(s) over its discovery window: %s. "+
				"Reply \"lock %s egress\" to apply this allowlist (audit mode), or review "+
				"~/systems/%s/manifest.proposed.json.", system, len(observed), strings.Join(observed, ", "), system, system)
			if nerr := postNotificationToPlatform(cfg, "system:egress-proposal",
				"Egress proposal: "+system, body, "system:"+system, "normal"); nerr != nil {
				log.Printf("egress-proposal notify failed for %s: %v", system, nerr)
			}
			auditLog.Log(audit.Entry{
				Action: "egress_manifest_proposed", Category: "security",
				Details: system + " -> " + strings.Join(observed, ","), RiskLevel: "low",
			})
		}

		// Phase 3 one-shot: move any pre-sandboxing host crontab into the
		// agent-shell sandbox, then wipe the host crontab. Idempotent.
		migrateHostCrontab("/home/vibecraft", auditLog)

		// Defensive re-chown of the agent-shell crontab on every daemon
		// start. Pre-v0.40.3 daemons created the file via os.WriteFile
		// with no follow-up chown, leaving it root-owned — inside the
		// sandbox's user namespace that maps to nobody:nogroup and the
		// manager can't edit it (observed live: the manager-agent could
		// not strip a scheduled system's entry, leaving an orphan that
		// fired "file not found" every 10 minutes forever). One daemon
		// restart after this lands repairs ownership in place across
		// the entire fleet. Idempotent; no-op when the daemon isn't
		// root (dev) or when the file is already owned correctly.
		sandbox.EnsureCrontabOwnership("/home/vibecraft")

		// Phase 3 scheduler reconciliation: the manager authors systems
		// by writing ~/systems/<name>/crontab. Start the corresponding
		// per-system sandbox when that file appears so scheduled systems
		// begin running without a separate daemon API call.
		startSystemSchedulerReconciler(ctx, systemsRoot, auditLog, newSystemSandboxStarter(vaultStore, mux), 15*time.Second)
	} else if cfg.ManagerKeyMode == "platform" {
		log.Fatalf("agent-shell sandbox unsupported in platform key mode")
	} else {
		log.Printf("agent-shell sandbox unsupported in operator key mode, using host bash")
	}

	// Hourly cleanup of expired session / sso_session / grant_nonce rows.
	// Cheap, idempotent, in-process — no cron / external scheduler.
	srv.startSessionSweeper(ctx)

	// Budget refresh loop: fetch the customer's AI credit budget from
	// the platform on startup, then hourly. Until the first fetch
	// succeeds, enforcement stays off (fail-open) — a platform outage
	// must never wedge the machine.
	go budgetTracker.StartRefreshLoop(ctx)

	rl := newRateLimiter(60, time.Minute)
	defer rl.stop()

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: rl.middleware(corsMiddleware(mux)),
		// ReadHeaderTimeout protects against Slowloris on headers; the
		// request body is allowed to take longer so a 25MB upload over
		// a mobile connection (several minutes at low bandwidth) isn't
		// cut off mid-stream. The per-handler size caps (MaxBytesReader
		// in /upload) bound memory usage regardless.
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      0, // SSE needs no write timeout
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)

		auditLog.Log(audit.Entry{
			Action:   "daemon_stopping",
			Category: "system",
		})

		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	// Phase 6: drop to the residual capability set now that the
	// privileged subsystems (sandbox netns/nft, agent-shell) are wired
	// and only per-request sandbox spawns remain (those need the
	// retained three). Best-effort by design — see caps_linux.go.
	if err := dropResidualCaps(); err != nil {
		log.Printf("warning: residual capability drop incomplete (continuing, no worse than status quo): %v", err)
		auditLog.Log(audit.Entry{
			Action:    "cap_drop_incomplete",
			Category:  "security",
			Details:   err.Error(),
			RiskLevel: "medium",
		})
	} else if caps := capRetained(); len(caps) > 0 {
		log.Printf("Phase 6: dropped residual capabilities; retained %v", caps)
		auditLog.Log(audit.Entry{
			Action:    "cap_drop",
			Category:  "security",
			Details:   fmt.Sprintf("retained %v", caps),
			RiskLevel: "low",
		})
	}

	log.Printf("listening on :%d", cfg.Port)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}

	log.Println("daemon stopped")
}

// Server holds all dependencies for HTTP handlers.
type Server struct {
	cfg           *Config
	db            *persistence.DB
	taskStore     *core.TaskStore
	memStore      *memory.Store
	auditLog      *audit.Logger
	grEngine      *guardrails.Engine
	vaultStore    *vault.Store
	vaultMask     *vault.Masker
	manager       *managerclient.Client
	ctrl          *computer.Controller
	screenshot    *computer.ScreenshotService
	shell         *computer.Shell
	engine        *core.Engine
	broker        *SSEBroker
	routeMgr      *routes.Manager
	jwks          *jwks.Client
	backup        *persistence.Backup  // optional; nil in tests
	usageStore    *usage.Store         // nil in tests; production wires usage.NewStore(db)
	budgetTracker *usage.BudgetTracker // nil in tests; production wires usage.NewBudgetTracker(...)
	startTime     time.Time

	// AI credits proxy. Per-machine bearer token + monthly budget
	// ledger. Initialized once at daemon startup (see initAIProxy).
	// Apps deployed via install-app-service receive the bearer in
	// their unit env and call /api/ai/credits/{anthropic,openai}/...
	// — the daemon strips their bearer, injects the platform key,
	// forwards, streams back, and charges the ledger.
	aiCreditsToken *AICreditsToken
	budget         *budgetService
}

// stripAPIPrefix rewrites /api/foo → /foo before calling next, so
// handlers that parse r.URL.Path with strings.TrimPrefix("/foo/", …)
// work uniformly for both the legacy un-namespaced path and the new
// /api/* path. The dual registration in registerRoutes is what routes
// both URLs to the same wrapped handler.
func stripAPIPrefix(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		}
		next(w, r)
	}
}

// readTrimmed reads a small credential file, trimmed; "" on error.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// dual registers a handler under /api/<path>. The name is historical:
	// it used to also register the bare un-namespaced <path> for the
	// legacy in-platform ChatLayout (Bearer-JWT, direct daemon calls).
	// D-7 retired that path — the SPA + CLI relay both speak /api/* —
	// so the un-namespaced registration is gone. stripAPIPrefix still
	// rewrites /api/foo → /foo so handlers that parse r.URL.Path with
	// TrimPrefix("/foo/", …) keep working unchanged.
	dual := func(path string, h http.HandlerFunc) {
		mux.HandleFunc("/api"+path, stripAPIPrefix(h))
	}

	// Health (health token auth). Two endpoints, same auth, different
	// shape — keeping /health zero-knowledge for the platform cron and
	// extending /api/health with `version` so the dashboard can branch
	// on daemon capability per Workstream G2 (version-gated rollout).
	mux.HandleFunc("/health", s.withHealthAuth(s.handleHealth))
	mux.HandleFunc("/api/health", s.withHealthAuth(s.handleHealthAPI))

	// Prometheus-format /metrics — fleet observability (Workstream K).
	// Same auth as /health so the platform cron can scrape with the
	// per-machine healthToken it already holds.
	mux.HandleFunc("/api/metrics", s.withHealthAuth(s.handleMetrics))
	mux.HandleFunc("/metrics", s.withHealthAuth(s.handleMetrics))

	// Data-plane routes accept the new vc_session cookie (SPA) OR a
	// legacy Bearer token (Vercel dashboard + CLI/API key). The chain
	// tries the cookie first and falls through to withAuth on miss.
	// Once D-7 retires the platform ChatLayout we can drop the Bearer
	// branch from any routes only the SPA consumes.
	dual("/status", s.withCookieOrBearer(s.handleStatus))
	dual("/machine", s.withCookieOrBearer(s.handleMachineIdentity))
	dual("/system", s.withCookieOrBearer(s.handleSystem))

	dual("/task", s.withCookieOrBearer(s.handleTask))
	dual("/task/", s.withCookieOrBearer(s.handleTaskByID))
	dual("/tasks", s.withCookieOrBearer(s.handleTasks))

	dual("/conversations", s.withCookieOrBearer(s.handleConversations))
	dual("/conversations/", s.withCookieOrBearer(s.handleConversationByID))

	dual("/screenshot", s.withCookieOrBearer(s.handleScreenshot))

	dual("/upload", s.withCookieOrBearer(s.handleUpload))
	dual("/inbox/", s.withCookieOrBearer(s.handleInbox))

	// Agent-native CLI surface (plans/agent-native-cli.md):
	// - /api/version       — version + schema-versions probe
	// - /api/files/<path>  — read a file under the /home/vibecraft jail
	// - /api/files-ls      — list a directory under the jail
	// Documented in cli/cmd/llms_embed.txt. All path-safe.
	dual("/version", s.handleVersion) // public-ish: no auth required so the
	// version probe can be done over the same TLS the data plane uses
	// (the CLI calls it first to decide whether to keep going). The probe
	// returns only the daemon's version string and schema versions —
	// nothing customer-specific.
	// Control-tier gate even for reads: these endpoints serve raw bytes
	// from anywhere under /home/vibecraft, which includes worker
	// subscription credentials (~/.codex/auth.json, ~/.claude/
	// .credentials.json) and the entire db.md company store. A view-tier
	// (read-only) principal must NOT be able to exfiltrate those, so we
	// mirror /keys' requireControlTier discipline here. CLI/API-key
	// callers always carry control tier, so this does not break them.
	dual("/files/", s.withCookieOrBearer(s.withControlTier(s.handleFiles)))
	dual("/files-ls", s.withCookieOrBearer(s.withControlTier(s.handleFilesLs)))

	dual("/stream", s.withCookieOrBearer(s.handleStream))

	dual("/memory", s.withCookieOrBearer(s.handleMemory))
	dual("/memory/", s.withCookieOrBearer(s.handleMemoryByID))

	dual("/rules", s.withCookieOrBearer(s.handleRules))
	dual("/rules/", s.withCookieOrBearer(s.handleRuleByID))

	dual("/audit", s.withCookieOrBearer(s.handleAudit))

	dual("/usage", s.withCookieOrBearer(s.handleAIUsage))

	dual("/vault", s.withCookieOrBearer(s.handleVault))
	dual("/vault/", s.withCookieOrBearer(s.handleVaultByName))

	dual("/manager-key", s.withCookieOrBearer(s.handleManagerKey))

	dual("/config", s.withCookieOrBearer(s.handleConfig))

	dual("/computer-md", s.withCookieOrBearer(s.handleComputerMD))

	// API key management (JWT auth only, not API key).
	dual("/keys", s.withJWTAuth(s.handleKeys))
	dual("/keys/", s.withJWTAuth(s.handleKeyByID))

	// Route management (localhost only — agent calls via curl from bash).
	// The operator-driven listing + SSO toggle is exposed separately via
	// /api/hosted-apps* below so the hosted-apps SPA panel doesn't
	// require localhost (the SPA fetches from the operator's browser,
	// which never reaches the loopback interface).
	dual("/routes", s.withLocalhostAuth(s.handleRoutes))
	dual("/routes/", s.withLocalhostAuth(s.handleRouteByName))

	// Operator-facing view of the same routes table. Listing + SSO
	// toggle + app restart. No name/port mutation here — the agent owns
	// the table via /routes (localhost-only) and the operator observes
	// + tunes.
	mux.HandleFunc("/api/hosted-apps", s.withCookieOrBearer(s.handleHostedApps))
	mux.HandleFunc("/api/hosted-apps/", s.withCookieOrBearer(s.handleHostedAppByName))
	// /routes/verify stays at its original path only — Caddy calls this
	// verbatim under the un-namespaced URL. Adding /api/routes/verify
	// would force Caddyfile coordination for no consumer benefit.
	// withLoopbackOnly (Phase 0b): closes the external-probe info
	// disclosure (the ${machineHost} reverse_proxy carries
	// X-Forwarded-For → 403) while leaving Caddy's tokenless on-demand
	// `ask` working (loopback, no X-F-F). NOT withLocalhostAuth — Caddy's
	// `ask` cannot present a bearer token.
	mux.HandleFunc("/routes/verify", s.withLoopbackOnly(s.handleRouteVerify))

	// Manager inbox (localhost only — systems authored by the manager
	// enqueue follow-up tasks here without needing a JWT or API key).
	// Same threat model as /routes/verify: only processes on this
	// machine can reach loopback. See handleDaemonTask comment.
	dual("/daemon/task", s.withLocalhostAuth(s.handleDaemonTask))
	// Phase 1 worker spawn (D3 explicit helper: /usr/local/bin/
	// vc-spawn-worker). Sandboxes when VIBECRAFT_SANDBOXED_WORKERS is
	// on, legacy otherwise. Same localhost-token gate as /daemon/task.
	dual("/daemon/spawn-worker", s.withLocalhostAuth(s.handleSpawnWorker))

	// Hosted-app persistence primitive. Writes a systemd-user unit
	// (host netns, vibecraft-owned) and starts it. The bash sandbox
	// can't install user-services itself — its dbus is outside the
	// netns — so this endpoint is the only honest path from a worker
	// build to a Caddy-reachable backend. See daemon/app_service.go
	// for the "why" comment block; the short version is that every
	// 502-after-deploy we shipped through v0.46 stemmed from servers
	// bound inside the sandbox loopback that Caddy on the host could
	// never reach.
	dual("/daemon/install-app-service", s.withLocalhostAuth(s.handleInstallAppService))
	// Manager-facing app lifecycle over the per-sandbox unix socket.
	// This is the supported way to cycle a hosted app from inside the
	// sandbox without exposing host PIDs, host loopback, or the user's
	// systemd bus to the sandbox itself.
	dual("/apps/", s.withLocalhostAuth(s.handleAppServiceAction))

	// AI credits proxy (see daemon/ai_proxy.go). Per-machine bearer
	// token in `x-api-key` / `Authorization: Bearer ...` validates;
	// daemon strips the app's auth header, injects the platform's
	// real provider key, forwards to api.anthropic.com /
	// api.openai.com, streams the response back, and charges the
	// monthly budget ledger. Apps deployed via install-app-service
	// receive the token in their unit env automatically — they just
	// point the AI SDK's baseURL here.
	//
	// Prefix routing (trailing slash) so the whole upstream API
	// surface is reachable — /v1/messages, /v1/messages/batches,
	// /v1/chat/completions, /v1/embeddings, anything the provider
	// adds. The proxy is upstream-shape-preserving by design.
	//
	// Loopback-only is enforced via withLoopbackOnly. There is no
	// /api/ai/credits surface for the dashboard browser — the proxy
	// exists for in-process apps on the box, not for cross-origin
	// browser calls.
	mux.HandleFunc("/api/ai/credits/anthropic/", s.withLoopbackOnly(s.aiProxyHandler(anthropicSpec)))
	mux.HandleFunc("/api/ai/credits/openai/", s.withLoopbackOnly(s.aiProxyHandler(openaiSpec)))

	// Management routes (management token auth).
	dual("/management/rotate-key", s.withManagementAuth(s.handleRotateKey))
	dual("/management/usage", s.withManagementAuth(s.handleUsage))
	dual("/management/checkpoint", s.withManagementAuth(s.handleCheckpoint))
	dual("/management/machine-name", s.withHealthAuth(s.handleManagementMachineName))
	// Incident-response handlers (Workstream C). Gated by the HEALTH
	// token, not the daemon token: the platform must be able to call
	// these to fan-out a JWKS cache flush + session revocation across
	// the fleet in minutes during a signing-key compromise. The daemon
	// token is machine-local and is the SQLCipher DB-key root — it is
	// never sent to the platform, so a withManagementAuth gate left
	// these endpoints unreachable by their only intended caller. The
	// health token is the established platform↔per-machine credential
	// (already gates /health and /metrics, stored in the machines
	// table). Incremental risk of a stolen single-machine health token:
	// a harmless forced JWKS refetch + a recoverable session logout on
	// that one machine — proportionate, and trivial next to the
	// platform-compromise threat these endpoints exist to answer.
	dual("/management/refresh-jwks", s.withHealthAuth(s.handleManagementRefreshJWKS))
	dual("/management/revoke-sessions", s.withHealthAuth(s.handleManagementRevokeSessions))
	// Revoke every brokered vc_machine_* daemon key owned by a userId.
	// Same HEALTH-token auth as refresh-jwks/revoke-sessions: the platform
	// calls this on control-access revoke / team-member removal so the
	// long-lived, never-expiring CLI key the daemon mints locally can't
	// outlive the control grant that authorized it. The daemon token is
	// machine-local and never reaches the platform, so it can't gate a
	// platform→daemon call here (same reasoning as the JWKS/session
	// incident-response handlers above).
	dual("/management/revoke-keys", s.withHealthAuth(s.handleManagementRevokeKeys))
	dual("/management/revoke-sessions-for-user", s.withHealthAuth(s.handleManagementRevokeSessionsForUser))
	// Push-to-user bridge (localhost only — the manager calls this via
	// curl from bash, exactly like /daemon/task and /routes: only
	// processes on this machine reach loopback; Caddy sets
	// X-Forwarded-For for external traffic, which is rejected). The
	// daemon forwards to the platform's ingest endpoint authenticated
	// with the machine's healthToken, which stores + dispatches bell +
	// email. NOT withManagementAuth: the manager is correctly forbidden
	// from reading the daemon.token (prompt.md security rules), so a
	// management-token gate left notify permanently unreachable. See
	// prompt.md "Notifying the customer".
	dual("/notify", s.withLocalhostAuth(s.handleNotify))

	// Systems management (localhost only — manager owns its authored
	// systems and uninstall is an idempotent atomic teardown). GET
	// /api/systems lists everything under ~/systems/<name>/ with a
	// last-run timestamp; POST /api/systems/{name}/uninstall scrubs
	// the catch-all ~/crontab entry then removes the directory. The
	// flat-file install pattern (prompt.md "Authoring systems") still
	// works as before — these endpoints close the previously-missing
	// "and now remove it" half. Same threat model as /daemon/task and
	// /routes: only processes on this machine reach loopback.
	dual("/systems", s.withLocalhostAuth(s.handleSystems))
	dual("/systems/", s.withLocalhostAuth(s.handleSystemByName))

	// Dashboard sidebar "what's running" view. Browser-authed (cookie
	// or bearer), strictly read-only, parallel to the localhost-only
	// /api/systems above. Different handler because the sidebar needs
	// different fields (humanized name, hosted-app URL cross-reference,
	// fast live probe) and a different latency budget (~1s) than the
	// manager's introspection view. See systems_dashboard.go for the
	// design rationale.
	dual("/dashboard/systems", s.withCookieOrBearer(s.handleDashboardSystems))

	// ── Cookie-based browser auth (Workstream G) ──────────────────────
	// /auth/callback consumes a one-shot grant code from the platform
	// and sets both vc_session (exact host) and vc_sso (subdomain-wide)
	// cookies. No auth on entry — the grant code IS the auth, verified
	// via JWKS (Workstream A).
	mux.HandleFunc("/auth/callback", s.handleAuthCallback)
	mux.HandleFunc("/api/auth/callback", s.handleAuthCallback)

	// /api/auth/whoami reads either cookie; used by the SPA on bootstrap
	// and by internal hosted apps server-side per Workstream H.
	mux.HandleFunc("/api/auth/whoami", s.handleAuthWhoami)
	mux.HandleFunc("/auth/whoami", s.handleAuthWhoami) // legacy alias for parity

	// /api/auth/logout deletes session + sso_session rows and clears
	// both cookies. SPA navigates to /dashboard on the platform after.
	mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("/auth/logout", s.handleAuthLogout)

	// SPA: every unknown non-API path is served from the embedded Vite
	// bundle (Workstream D). Must be registered LAST because the "/"
	// pattern in net/http.ServeMux matches anything not handled by a
	// more specific pattern; we want all API routes to win before this
	// fallback runs.
	s.registerWebRoutes(mux)
}

// --- Auth middleware ---

// withHealthAuth accepts only the health token for health check endpoints.
func (s *Server) withHealthAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			jsonError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			jsonError(w, "invalid Authorization format", http.StatusUnauthorized)
			return
		}

		if parts[1] != s.cfg.HealthToken {
			jsonError(w, "invalid health token", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// withAuth accepts JWT or API key (prefix vc_machine_). Does NOT accept daemon token.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			jsonError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			jsonError(w, "invalid Authorization format", http.StatusUnauthorized)
			return
		}

		tokenStr := parts[1]

		// Check if this is a machine API key.
		if strings.HasPrefix(tokenStr, "vc_machine_") {
			hash := sha256.Sum256([]byte(tokenStr))
			keyHash := hex.EncodeToString(hash[:])

			apiKey, err := s.db.GetAPIKeyByHash(keyHash)
			if err != nil {
				jsonError(w, "invalid API key", http.StatusUnauthorized)
				return
			}

			// Update last_used_at in the background.
			go s.db.UpdateAPIKeyLastUsed(apiKey.ID)

			// Log API key access.
			go s.auditLog.Log(audit.Entry{
				Action:   "api_key_access",
				Category: "security",
				UserID:   apiKey.CreatedBy,
				Details:  fmt.Sprintf("key=%s endpoint=%s method=%s ip=%s", apiKey.KeyHint, r.URL.Path, r.Method, remoteIP(r)),
			})

			// Machine API keys are a control-tier credential. They can only
			// be minted by a control user (handleKeys is JWT-gated; the v1
			// relay signs `access:"control"` for the same reason — see
			// lib/relay.ts), and they exist for CLI/programmatic writes
			// (task submission, vault, fanout, systems). Set the tier
			// explicitly so requireControl lets these through; without it
			// the key would carry no tier and every mutating route would
			// 403, silently breaking the entire CLI write surface.
			ctx := r.Context()
			if apiKey.CreatedBy != "" {
				ctx = context.WithValue(ctx, ctxKeyUserID, apiKey.CreatedBy)
			}
			ctx = context.WithValue(ctx, ctxKeyAccess, "control")
			next(w, r.WithContext(ctx))
			return
		}

		// Validate as JWT (RS256).
		s.validateJWTAndServe(w, r, tokenStr, next)
	}
}

// withJWTAuth accepts JWT only. Used for key management routes where API key auth is not allowed.
func (s *Server) withJWTAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			jsonError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			jsonError(w, "invalid Authorization format", http.StatusUnauthorized)
			return
		}

		tokenStr := parts[1]

		// Reject API keys for key management routes.
		if strings.HasPrefix(tokenStr, "vc_machine_") {
			jsonError(w, "API key management requires JWT authentication", http.StatusForbidden)
			return
		}

		s.validateJWTAndServe(w, r, tokenStr, next)
	}
}

// validateJWTAndServe validates an RS256 JWT and calls next if valid.
func (s *Server) validateJWTAndServe(w http.ResponseWriter, r *http.Request, tokenStr string, next http.HandlerFunc) {
	if s.jwks == nil {
		jsonError(w, "JWT authentication not configured", http.StatusUnauthorized)
		return
	}

	// Harden validation against algorithm-confusion and unbounded tokens:
	//   - WithValidMethods pins RS256 so an attacker can't downgrade to
	//     "none" or coerce an HMAC verify against the public key.
	//   - WithExpirationRequired rejects any token with no exp — a forged
	//     or legacy never-expiring token must not authenticate.
	//   - WithIssuer binds to the platform's iss ("vibecraft.so"), which
	//     both signers (legacy PEM + KMS) stamp on every token.
	// The platform never sets an `aud` claim; the `machine` claim below is
	// the per-machine audience binding and is checked explicitly.
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			// Legacy JWTs predate explicit kid; the bootstrap PEM
			// is pre-populated in the cache under vibecraft-1.
			kid = "vibecraft-1"
		}
		return s.jwks.KeyFor(kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(jwtExpectedIssuer),
	)

	if err != nil || !token.Valid {
		jsonError(w, "invalid token", http.StatusUnauthorized)
		return
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		jsonError(w, "invalid token claims", http.StatusUnauthorized)
		return
	}

	machineID, ok := claims["machine"].(string)
	if !ok || machineID == "" {
		jsonError(w, "missing required machine claim", http.StatusUnauthorized)
		return
	}

	if machineID != s.cfg.MachineID {
		jsonError(w, "token not valid for this machine", http.StatusForbidden)
		return
	}

	purpose, _ := claims["purpose"].(string)
	if purpose == "grant" || stringClaim(claims, "nonce") != "" {
		jsonError(w, "grant code cannot authenticate data-plane requests", http.StatusUnauthorized)
		return
	}

	// Store the sub/access claims in the request context for downstream handlers.
	sub, _ := claims["sub"].(string)
	access, _ := claims["access"].(string)
	ctx := r.Context()
	if sub != "" {
		ctx = context.WithValue(ctx, ctxKeyUserID, sub)
	}
	if access != "" {
		ctx = context.WithValue(ctx, ctxKeyAccess, access)
	}
	r = r.WithContext(ctx)

	// Log JWT access.
	go s.auditLog.Log(audit.Entry{
		Action:   "jwt_access",
		Category: "security",
		UserID:   sub,
		Details:  fmt.Sprintf("endpoint=%s method=%s ip=%s", r.URL.Path, r.Method, remoteIP(r)),
	})

	next(w, r)
}

// ctxKey is an unexported type for context keys to avoid collisions.
type ctxKey string

const (
	ctxKeyUserID ctxKey = "userID"
	ctxKeyAccess ctxKey = "access"
)

// jwtExpectedIssuer is the `iss` claim both platform signers stamp on
// every data-plane token (lib/jwt.ts signLegacy + lib/kms-signer.ts).
// validateJWTAndServe pins it so a token minted for a different issuer
// can't authenticate here.
const jwtExpectedIssuer = "vibecraft.so"

func (s *Server) withManagementAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			jsonError(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			jsonError(w, "invalid Authorization format", http.StatusUnauthorized)
			return
		}

		if parts[1] != s.cfg.DaemonToken {
			jsonError(w, "invalid management token", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// withLoopbackOnly restricts access to direct loopback connections only.
// Rejects anything proxied through Caddy (Caddy sets X-Forwarded-For on
// every reverse_proxy hop) and anything whose RemoteAddr is not 127.0.0.1
// / ::1. It does NOT require a bearer token.
//
// This is the right gate for /routes/verify: Caddy's on-demand-TLS `ask`
// reaches the daemon over loopback with NO Authorization header and NO
// X-Forwarded-For (proven via the Caddy v2.11.3 PoC in the sandboxing
// plan), while an external probe arriving through the ${machineHost}
// reverse_proxy is also loopback at the socket but carries
// X-Forwarded-For — so the X-F-F reject is the load-bearing
// discriminator and a token would only break Caddy's ask.
func (s *Server) withLoopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Caddy always sets X-Forwarded-For when proxying external traffic.
		// Direct localhost connections (agent's curl) don't have this header.
		if r.Header.Get("X-Forwarded-For") != "" {
			jsonError(w, "route management is only available from localhost", http.StatusForbidden)
			return
		}

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}

		if host != "127.0.0.1" && host != "::1" {
			jsonError(w, "route management is only available from localhost", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// withLocalhostAuth is withLoopbackOnly plus a per-machine local-token
// requirement (Phase 0b). The localhost-only data-plane endpoints
// (/api/routes, /api/daemon/task, /api/notify) take this: a process that
// merely shares the vibecraft uid (a cron job, a systemd unit, an
// npm-postinstall running outside the agent's shell) reaches loopback but
// does NOT carry $VIBECRAFT_LOCAL_TOKEN, which the daemon injects only
// into the shells it spawns.
//
// The local token is a boot invariant (LoadConfig generate-if-absent),
// so there is no legacy no-token mode: every non-sandbox loopback caller
// MUST present it. /routes/verify deliberately uses withLoopbackOnly,
// NOT this — Caddy's `ask` cannot supply a token.
func (s *Server) withLocalhostAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only the daemon-created manager shell keeps socket-implicit
		// localhost auth. System/tool sandboxes have a socket identity for
		// scoped endpoints, but that identity must not become a host-level
		// local token bypass.
		if id := sandbox.SandboxID(r); id != "" {
			if id != sandbox.AgentShellID {
				jsonError(w, "local daemon access is only available to the agent shell", http.StatusForbidden)
				return
			}
			next(w, r)
			return
		}
		s.withLoopbackOnly(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			authz := r.Header.Get("Authorization")
			presented := strings.TrimPrefix(authz, prefix)
			if !strings.HasPrefix(authz, prefix) ||
				subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.LocalToken)) != 1 {
				jsonError(w, "local token required", http.StatusUnauthorized)
				return
			}
			next(w, r)
		})(w, r)
	}
}

// --- Route handlers ---

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routeList, err := s.routeMgr.List()
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to list routes: %v", err), http.StatusInternalServerError)
			return
		}

		// Add URLs to each route for convenience.
		type routeWithURL struct {
			Name       string `json:"name"`
			Port       int    `json:"port"`
			URL        string `json:"url"`
			SSOEnabled bool   `json:"sso_enabled"`
			CreatedAt  string `json:"created_at"`
		}
		result := make([]routeWithURL, len(routeList))
		for i, r := range routeList {
			result[i] = routeWithURL{
				Name:       r.Name,
				Port:       r.Port,
				URL:        s.routeMgr.URL(r.Name),
				SSOEnabled: r.SSOEnabled,
				CreatedAt:  r.CreatedAt,
			}
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"routes": result})

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Port == 0 {
			jsonError(w, "name and port are required", http.StatusBadRequest)
			return
		}

		if err := s.routeMgr.Register(req.Name, req.Port); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}

		s.auditLog.Log(audit.Entry{
			Action:   "route_registered",
			Category: "routes",
			Details:  fmt.Sprintf("name=%s port=%d url=%s", req.Name, req.Port, s.routeMgr.URL(req.Name)),
		})

		jsonResponse(w, http.StatusCreated, map[string]string{
			"name": req.Name,
			"url":  s.routeMgr.URL(req.Name),
		})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRouteByName(w http.ResponseWriter, r *http.Request) {
	// Handle /routes/verify separately (it has its own handler registered).
	name := strings.TrimPrefix(r.URL.Path, "/routes/")
	if name == "" || name == "verify" {
		jsonError(w, "route name is required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := s.routeMgr.Unregister(name); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.auditLog.Log(audit.Entry{
			Action:   "route_unregistered",
			Category: "routes",
			Details:  fmt.Sprintf("name=%s", name),
		})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})

	case http.MethodPatch:
		// Operator-driven toggle from the hosted-apps panel. Only the
		// sso_enabled flag is mutable today; the route name + port are
		// owned by the agent via Register.
		var req struct {
			SSOEnabled *bool `json:"sso_enabled"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.SSOEnabled == nil {
			jsonError(w, "sso_enabled is required", http.StatusBadRequest)
			return
		}
		if err := s.routeMgr.SetSSOEnabled(name, *req.SSOEnabled); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.auditLog.Log(audit.Entry{
			Action:   "route_sso_toggled",
			Category: "routes",
			Details:  fmt.Sprintf("name=%s enabled=%v", name, *req.SSOEnabled),
		})
		jsonResponse(w, http.StatusOK, map[string]any{
			"name":        name,
			"sso_enabled": *req.SSOEnabled,
		})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleHostedApps is the operator-facing view of the same routes
// table /routes manages. Read-only listing for the hosted-apps SPA
// panel (Workstream J). Mutating endpoints live on /api/hosted-apps/
// per-name.
func (s *Server) handleHostedApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	routeList, err := s.routeMgr.List()
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to list routes: %v", err), http.StatusInternalServerError)
		return
	}
	type routeView struct {
		Name       string `json:"name"`
		Port       int    `json:"port"`
		URL        string `json:"url"`
		SSOEnabled bool   `json:"sso_enabled"`
		CreatedAt  string `json:"created_at"`
	}
	out := make([]routeView, len(routeList))
	for i, r := range routeList {
		out[i] = routeView{
			Name:       r.Name,
			Port:       r.Port,
			URL:        s.routeMgr.URL(r.Name),
			SSOEnabled: r.SSOEnabled,
			CreatedAt:  r.CreatedAt,
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"routes": out})
}

// handleHostedAppByName supports DELETE + PATCH on a registered route
// from the operator UI, plus POST /restart for app lifecycle. The
// agent's route table mutation side lives at /api/routes
// (localhost-only); the manager-facing restart alias lives at
// /api/apps/<name>/restart.
func (s *Server) handleHostedAppByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/hosted-apps/")
	if name == "" {
		jsonError(w, "route name is required", http.StatusBadRequest)
		return
	}
	if actionName, action, ok := parseAppActionPath(r.URL.Path, "/api/hosted-apps/"); ok {
		switch action {
		case "restart":
			if r.Method != http.MethodPost {
				jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !s.requireControl(w, r) {
				return
			}
			result, code := s.restartHostedAppService(r.Context(), actionName)
			s.auditLog.Log(audit.Entry{
				Action:   "hosted_app_restart",
				Category: "systems",
				Details: fmt.Sprintf("name=%s status=%s old_pid=%d new_pid=%d port=%d listening=%v actor=operator",
					result.Name, result.Status, result.OldPID, result.NewPID, result.Port, result.ListeningAfter),
				RiskLevel: "low",
			})
			jsonResponse(w, code, result)
		case "deploy":
			if r.Method != http.MethodPost {
				jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !s.requireControl(w, r) {
				return
			}
			var req struct {
				Environment []string `json:"environment"`
				Port        *int     `json:"port"`
			}
			// Body is optional: a bare deploy redeploys the current code.
			if r.ContentLength != 0 {
				if err := readJSON(r, &req); err != nil {
					jsonError(w, "invalid request body", http.StatusBadRequest)
					return
				}
			}
			result, code := s.deployHostedAppService(r.Context(), actionName, req.Environment, req.Port)
			s.auditLog.Log(audit.Entry{
				Action:   "hosted_app_deploy",
				Category: "systems",
				Details: fmt.Sprintf("name=%s status=%s old_pid=%d new_pid=%d port=%d listening=%v env_overrides=%d actor=operator",
					result.Name, result.Status, result.OldPID, result.NewPID, result.Port, result.ListeningAfter, len(req.Environment)),
				RiskLevel: "medium",
			})
			jsonResponse(w, code, result)
		case "status":
			if r.Method != http.MethodGet {
				jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			result, code := s.hostedAppStatus(r.Context(), actionName)
			jsonResponse(w, code, result)
		case "logs":
			if r.Method != http.MethodGet {
				jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
			result, code := s.hostedAppLogs(r.Context(), actionName, n)
			jsonResponse(w, code, result)
		case "env":
			if r.Method != http.MethodGet {
				jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			result, code := s.hostedAppEnv(actionName)
			jsonResponse(w, code, result)
		default:
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if strings.Contains(name, "/") {
		jsonError(w, "route name is invalid", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if !s.requireControl(w, r) {
			return
		}
		if err := s.routeMgr.Unregister(name); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.auditLog.Log(audit.Entry{
			Action:   "route_unregistered",
			Category: "routes",
			Details:  fmt.Sprintf("name=%s actor=operator", name),
		})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
	case http.MethodPatch:
		if !s.requireControl(w, r) {
			return
		}
		var req struct {
			SSOEnabled *bool `json:"sso_enabled"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.SSOEnabled == nil {
			jsonError(w, "sso_enabled is required", http.StatusBadRequest)
			return
		}
		if err := s.routeMgr.SetSSOEnabled(name, *req.SSOEnabled); err != nil {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		s.auditLog.Log(audit.Entry{
			Action:   "route_sso_toggled",
			Category: "routes",
			Details:  fmt.Sprintf("name=%s enabled=%v actor=operator", name, *req.SSOEnabled),
		})
		jsonResponse(w, http.StatusOK, map[string]any{
			"name":        name,
			"sso_enabled": *req.SSOEnabled,
		})
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRouteVerify is called by Caddy's on_demand_tls to approve cert issuance.
// No auth required — only reachable from localhost (Caddy's internal ask).
func (s *Server) handleRouteVerify(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" || len(domain) > 253 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if s.routeMgr.Verify(domain) {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusForbidden)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Seconds()

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"uptime":    int64(uptime),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleHealthAPI is /api/health — same auth as the cron-facing /health
// but returns daemon version too. The platform's dashboard caches this
// for 60s per machine and branches the chat path on it: pre-0.21 daemons
// fall through to the legacy in-platform ChatLayout, ≥0.21 daemons get
// the cookie-handshake redirect (Workstream G2).
func (s *Server) handleHealthAPI(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Seconds()

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"uptime":    int64(uptime),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"version":   version,
	})
}

// handleStream serves the SSE connection. It replays any currently
// outstanding task:waiting approvals and credential requests to the new
// client before entering the broadcast loop — a reconnect (page reload,
// network blip) should self-heal instead of leaving the user staring at
// a chat with no visible card while the daemon blocks.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	var initial []SSEEvent
	waiting, err := s.taskStore.ListWaitingForInput()
	if err != nil {
		log.Printf("handleStream: list waiting tasks: %v", err)
	}
	for _, t := range waiting {
		stored := ""
		if t.Result != nil {
			stored = *t.Result
		}
		if stored == "" {
			continue
		}

		// Credential requests get their own event — the card UI is
		// different from an approval card, and conflating the two
		// under task:waiting would force the client to sniff the
		// payload to pick a renderer. MessageID travels inside the
		// payload (embedded by handleCredentialRequest), so a
		// reconnecting client can dedupe against its rehydrated
		// message list.
		if cp, ok := core.DecodeCredentialRequestJSON(stored); ok {
			initial = append(initial, SSEEvent{
				ID:    fmt.Sprintf("%d-replay", time.Now().UnixNano()),
				Event: "task:credentials_requested",
				Data: map[string]interface{}{
					"task_id":    t.ID,
					"message_id": cp.MessageID,
					"payload":    cp,
				},
			})
			continue
		}

		// Task.result may hold either the structured JSON payload (new
		// format) or a legacy plain-text prompt from an older daemon
		// version. Emit both fields so clients can render either way.
		payload := map[string]interface{}{
			"task_id":  t.ID,
			"question": stored,
		}
		if ap, ok := core.DecodeApprovalJSON(stored); ok {
			payload["question"] = ap.PlainText()
			payload["approval"] = ap
		}
		initial = append(initial, SSEEvent{
			ID:    fmt.Sprintf("%d-replay", time.Now().UnixNano()),
			Event: "task:waiting",
			Data:  payload,
		})
	}
	s.broker.ServeHTTPWithInit(w, r, presenceUserFromCtx(r), initial)
}

// handleStatus returns machine status for authenticated users (richer than /health).
// handleAIUsage returns a Usage summary (per-model breakdown, daily
// trend, top conversations, total cost in USD) over a requested period.
//
// Query parameters:
//
//	start, end    YYYY-MM-DD (UTC), inclusive. Defaults to the current
//	              calendar month when both are omitted. The SPA passes
//	              the Stripe subscription period from the platform when
//	              it has one — calendar month is the fallback so this
//	              endpoint is useful before the platform-side join lands.
//	top           how many top-conversations to include (default 5,
//	              capped at 20 to keep the response bounded).
//
// Returns the JSON shape defined by usage.Summary.
func (s *Server) handleAIUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.usageStore == nil {
		// Defensive: production always wires the store. If this path
		// fires we'd rather return a clear 503 than silently report
		// "$0.00" (which would look like "no usage" to the customer).
		jsonError(w, "usage store not configured", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	start := q.Get("start")
	end := q.Get("end")
	if start == "" && end == "" {
		start, end = usage.CurrentMonthBounds()
	}
	// Partial / missing bounds → reject explicitly. The SPA is expected
	// to send both or neither.
	if start == "" || end == "" {
		jsonError(w, "start and end must both be set (YYYY-MM-DD) or both be omitted", http.StatusBadRequest)
		return
	}

	top := 5
	if v := q.Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			top = n
		}
	}
	if top > 20 {
		top = 20
	}

	summary, err := s.usageStore.Aggregate(start, end, top)
	if err != nil {
		jsonError(w, fmt.Sprintf("aggregate usage: %v", err), http.StatusInternalServerError)
		return
	}

	// Attach the budget enforcement verdict so the SPA can render the
	// "paused" banner authoritatively (the daemon is the enforcer; the
	// UI should display the enforcer's verdict, not recompute it).
	if s.budgetTracker != nil {
		if state, berr := s.budgetTracker.State(); berr == nil {
			summary.BudgetState = &state
		}
	}

	jsonResponse(w, http.StatusOK, summary)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.startTime).Seconds()

	// Get current task info.
	currentTask := ""
	tasks, err := s.taskStore.ListRecent(1)
	if err == nil && len(tasks) > 0 && tasks[0].Status == "running" {
		currentTask = tasks[0].ID
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"machine_id":   s.cfg.MachineID,
		"version":      version,
		"uptime":       int64(uptime),
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"current_task": currentTask,
	})
}

// handleTasks returns a list of recent tasks.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tasks, err := s.taskStore.ListRecent(50)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to list tasks: %v", err), http.StatusInternalServerError)
		return
	}
	if tasks == nil {
		tasks = []*core.Task{}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"tasks": tasks})
}

// idempotencyKeyLocks serializes concurrent submissions that carry the
// same idempotency key. The lookup→create→store sequence in handleTask
// is otherwise a TOCTOU window: idempotencyLookup and idempotencyStoreSet
// each take idempotencyMu independently and release it between, so two
// near-simultaneous (or retried) POSTs with the same key both miss the
// lookup and both call CreateTask, producing duplicate tasks — double
// side-effects and double manager billing. Holding a per-key lock across
// the whole sequence makes the claim atomic: the second caller blocks
// until the first stores its task id, then its lookup hits and it returns
// the SAME task. Keyed locks (not the global idempotencyMu) so unrelated
// keys never serialize against each other.
var (
	idempotencyKeyLocksMu sync.Mutex
	idempotencyKeyLocks   = map[string]*idempotencyKeyLock{}
)

type idempotencyKeyLock struct {
	mu  sync.Mutex
	ref int
}

// acquireIdempotencyKeyLock returns a locked per-key mutex and a release
// function. The release unlocks and drops the lock from the registry once
// no waiters remain, so the map stays bounded by in-flight keys only.
func acquireIdempotencyKeyLock(key string) func() {
	idempotencyKeyLocksMu.Lock()
	l, ok := idempotencyKeyLocks[key]
	if !ok {
		l = &idempotencyKeyLock{}
		idempotencyKeyLocks[key] = l
	}
	l.ref++
	idempotencyKeyLocksMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		idempotencyKeyLocksMu.Lock()
		l.ref--
		if l.ref == 0 {
			delete(idempotencyKeyLocks, key)
		}
		idempotencyKeyLocksMu.Unlock()
	}
}

// handleTask dispatches POST /task (create) and GET /task (unsupported).
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	var req struct {
		Instruction    string            `json:"instruction"`
		Message        string            `json:"message"`
		ConversationID string            `json:"conversation_id"`
		IdempotencyKey string            `json:"idempotency_key,omitempty"`
		Attachments    []core.Attachment `json:"attachments,omitempty"`
	}

	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Idempotency: A retried POST with the same key returns the existing
	// task instead of creating a duplicate. We don't check that the body
	// matches (a "conflict" semantic) — the wire contract is "same key →
	// same task", letting agents safely retry without bookkeeping. If
	// agents need conflict-on-different-body they can hash the body
	// themselves.
	//
	// Hold a per-key lock across the entire lookup→create→store sequence
	// so two concurrent (or retried) submissions with the same key can't
	// both miss the lookup and both create a task. The second caller
	// blocks here until the first has stored its task id, then its lookup
	// below hits and returns the same task. Released via defer at the end
	// of the handler.
	if req.IdempotencyKey != "" {
		release := acquireIdempotencyKeyLock(req.IdempotencyKey)
		defer release()

		if existing, ok := idempotencyLookup(req.IdempotencyKey); ok {
			task, err := s.taskStore.GetTask(existing)
			if err == nil {
				jsonResponse(w, http.StatusOK, task)
				return
			}
		}
	}

	// Accept both "instruction" and "message" field names.
	if req.Instruction == "" {
		req.Instruction = req.Message
	}

	// A message with only attachments (no typed text) is valid — non-technical
	// users will often just drop a file and hit Send. Derive a friendly label
	// from the first file's name so the sidebar title and user bubble read
	// naturally instead of showing a placeholder string.
	if req.Instruction == "" && len(req.Attachments) > 0 {
		name := req.Attachments[0].Original
		if name == "" {
			name = req.Attachments[0].Name
		}
		if len(req.Attachments) > 1 {
			req.Instruction = fmt.Sprintf("%s and %d more", name, len(req.Attachments)-1)
		} else {
			req.Instruction = name
		}
	}

	if req.Instruction == "" {
		jsonError(w, "instruction is required", http.StatusBadRequest)
		return
	}

	if req.ConversationID == "" {
		req.ConversationID = uuid.New().String()
	}

	// Validate attachments belong to this conversation's inbox. Rejects a
	// client that tried to hand us a path from a different conversation or,
	// worse, somewhere outside the inbox entirely.
	if err := s.validateAttachmentPaths(req.ConversationID, req.Attachments); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// ── Manager auth gate ───────────────────────────────────────────
	// Legacy BYOM machines can auto-update from the Anthropic-era binary
	// to the OpenAI manager binary before the operator has installed an
	// OpenAI key. Keep the daemon/dashboard online, but hold new manager
	// work with a clear durable reply instead of letting Caddy show 502.
	if s.cfg != nil && s.cfg.ManagerUnavailableReason != "" {
		if aerr := s.taskStore.EnsureConversation(req.ConversationID, req.Instruction); aerr != nil {
			log.Printf("manager gate: ensure conversation: %v", aerr)
		}
		if aerr := s.taskStore.AddMessage(req.ConversationID, "user", req.Instruction); aerr != nil {
			log.Printf("manager gate: persist user message: %v", aerr)
		}
		localAnswer := answerLocalMachineFact(req.Instruction)
		if localAnswer != "" {
			if aerr := s.taskStore.AddMessage(req.ConversationID, "assistant", localAnswer); aerr != nil {
				log.Printf("manager gate: persist local answer: %v", aerr)
			}
		}
		var setupRequired *SetupRequestPayload
		if s.cfg.ManagerKeyMode == "operator" {
			payload := operatorOpenAIKeySetupPayload("")
			msgID, aerr := s.taskStore.AddTypedMessageReturningID(req.ConversationID, "assistant", setupPayloadJSON(payload), "setup_request", nil)
			if aerr != nil {
				log.Printf("manager gate: persist setup request: %v", aerr)
			} else {
				payload.MessageID = msgID
				if uerr := s.taskStore.UpdateMessageContent(msgID, setupPayloadJSON(payload)); uerr != nil {
					log.Printf("manager gate: update setup request: %v", uerr)
				}
			}
			setupRequired = &payload
		} else {
			if aerr := s.taskStore.AddMessage(req.ConversationID, "assistant", s.cfg.ManagerUnavailableReason); aerr != nil {
				log.Printf("manager gate: persist manager unavailable message: %v", aerr)
			}
		}
		s.auditLog.Log(audit.Entry{
			Action:    "task_manager_unavailable",
			Category:  "task",
			UserID:    userFromCtx(r),
			Details:   req.Instruction,
			RiskLevel: "low",
		})
		resp := map[string]interface{}{
			"manager_unavailable": true,
			"message":             s.cfg.ManagerUnavailableReason,
			"local_answer":        localAnswer,
			"conversation_id":     req.ConversationID,
		}
		if setupRequired != nil {
			resp["setup_required"] = setupRequired
		}
		jsonResponse(w, http.StatusOK, resp)
		return
	}

	// ── AI budget gate ──────────────────────────────────────────────
	// If the customer has spent their full monthly AI credit budget,
	// hold new tasks. This is the enforcement half of "no surprise
	// bills" (PRODUCT.md). Deliberately *smooth*:
	//   - Submission-time only — a task already running always
	//     finishes; we never kill work in flight.
	//   - The user's message + a clear manager reply are persisted to
	//     the conversation, so the exchange survives a page reload and
	//     reads like a normal turn, not a silent drop.
	//   - The response carries budget_blocked:true; the SPA renders the
	//     manager reply inline and keeps the user's message visible.
	//   - Fail-open: State() only reports Paused when there's a real,
	//     positive budget AND spend has reached it. A platform outage
	//     (no budget known) or Enterprise (-1) never pauses.
	if s.budgetTracker != nil {
		if state, err := s.budgetTracker.State(); err == nil && state.Paused {
			msg := fmt.Sprintf(
				"You've used your full $%.0f monthly AI budget. New tasks are paused until your billing cycle renews on %s — your usage resets then. Anything already running will finish normally.",
				state.BudgetUSD, state.ResetsOn,
			)
			// Persist the exchange so it survives reload and shows in
			// the conversation like any other turn. EnsureConversation
			// first — the messages→conversations FK needs the row, and
			// the budget gate runs before CreateTask would create it.
			if aerr := s.taskStore.EnsureConversation(req.ConversationID, req.Instruction); aerr != nil {
				log.Printf("budget gate: ensure conversation: %v", aerr)
			}
			if aerr := s.taskStore.AddMessage(req.ConversationID, "user", req.Instruction); aerr != nil {
				log.Printf("budget gate: persist user message: %v", aerr)
			}
			if aerr := s.taskStore.AddMessage(req.ConversationID, "assistant", msg); aerr != nil {
				log.Printf("budget gate: persist manager message: %v", aerr)
			}
			s.auditLog.Log(audit.Entry{
				Action:    "task_budget_blocked",
				Category:  "task",
				UserID:    userFromCtx(r),
				Details:   req.Instruction,
				RiskLevel: "low",
			})
			jsonResponse(w, http.StatusOK, map[string]interface{}{
				"budget_blocked":  true,
				"message":         msg,
				"resets_on":       state.ResetsOn,
				"conversation_id": req.ConversationID,
			})
			return
		}
	}

	task, err := s.taskStore.CreateTask(req.ConversationID, req.Instruction, req.Attachments...)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to create task: %v", err), http.StatusInternalServerError)
		return
	}

	if req.IdempotencyKey != "" {
		idempotencyStoreSet(req.IdempotencyKey, task.ID)
	}

	s.auditLog.Log(audit.Entry{
		Action:   "task_created",
		Category: "task",
		UserID:   userFromCtx(r),
		TaskID:   task.ID,
		Details:  req.Instruction,
	})

	s.broker.Emit("task:created", task)

	// If the engine is currently parked on an approval or credential card
	// in a DIFFERENT conversation, the new task is the user's signal that
	// they've redirected attention. The engine auto-expires the open card
	// and frees the loop so this new task can run. Same-conversation
	// creates and creates with no held card are no-ops. See
	// Engine.NotifyNewTaskInConversation for the full semantics. Nil
	// check is for the budget-gate HTTP-level unit tests that exercise
	// handleTask without wiring up a full engine.
	if s.engine != nil {
		s.engine.NotifyNewTaskInConversation(req.ConversationID)
	}

	jsonResponse(w, http.StatusCreated, task)
}

// handleTaskByID dispatches /task/{id}/status, /task/{id}/respond, /task/{id}/cancel.
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/task/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, "task ID is required", http.StatusBadRequest)
		return
	}

	taskID := parts[0]

	// /task/{id} with no sub-path -> treat as status.
	if len(parts) == 1 {
		s.handleTaskStatus(w, r, taskID)
		return
	}

	switch parts[1] {
	case "status":
		s.handleTaskStatus(w, r, taskID)
	case "respond":
		s.handleTaskRespond(w, r, taskID)
	case "cancel":
		s.handleTaskCancel(w, r, taskID)
	case "credentials":
		s.handleTaskCredentials(w, r, taskID)
	default:
		jsonError(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request, taskID string) {
	task, err := s.taskStore.GetTask(taskID)
	if err != nil {
		jsonError(w, "task not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, http.StatusOK, task)
}

func (s *Server) handleTaskRespond(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	var req struct {
		Input string `json:"input"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.engine.SubmitInput(taskID, req.Input); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "user_responded",
		Category: "task",
		UserID:   userFromCtx(r),
		TaskID:   taskID,
		Details:  req.Input,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "input received"})
}

// handleTaskCredentials receives the customer's submission from a
// request_credentials card. The body is either a list of name/value
// pairs to store, or {"cancelled": true} when the customer dismissed
// the card without filling it in.
//
// Validation + vault writes happen inside engine.SubmitCredentials so
// the agent loop can't be unblocked with state the engine hasn't
// verified against the original ask.
func (s *Server) handleTaskCredentials(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	var req struct {
		Cancelled   bool                   `json:"cancelled"`
		Credentials []core.CredentialValue `json:"credentials"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !req.Cancelled && len(req.Credentials) == 0 {
		jsonError(w, "credentials array is required (or set cancelled=true)", http.StatusBadRequest)
		return
	}

	resp := core.CredentialResponse{
		Cancelled: req.Cancelled,
		Values:    req.Credentials,
	}
	if err := s.engine.SubmitCredentials(taskID, resp); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Engine-side audit entries (credentials_request_cancelled,
	// vault_secret_set_from_credential_request) cover the detail;
	// adding a duplicate HTTP-side log would double-count the same
	// event. The HTTP layer only adds the user id, which is already
	// captured by jwt_access. Keep the handler quiet.

	if req.Cancelled {
		jsonResponse(w, http.StatusOK, map[string]string{"status": "dismissed"})
		return
	}

	stored := make([]string, len(req.Credentials))
	for i, c := range req.Credentials {
		stored[i] = c.Name
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": "stored",
		"names":  stored,
	})
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	if err := s.engine.CancelTask(taskID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "task_cancel_requested",
		Category: "task",
		UserID:   userFromCtx(r),
		TaskID:   taskID,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "cancel requested"})
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	convos, err := s.taskStore.ListConversations()
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to list conversations: %v", err), http.StatusInternalServerError)
		return
	}
	if convos == nil {
		convos = []*core.Conversation{}
	}
	jsonResponse(w, http.StatusOK, convos)
}

func (s *Server) handleConversationByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/conversations/")
	if id == "" {
		jsonError(w, "conversation ID is required", http.StatusBadRequest)
		return
	}

	convo, err := s.taskStore.GetConversation(id)
	if err != nil {
		jsonError(w, "conversation not found", http.StatusNotFound)
		return
	}

	messages, err := s.taskStore.GetMessages(id)
	if err != nil {
		messages = []*core.Message{}
	}

	tasks, err := s.taskStore.ListByConversation(id)
	if err != nil {
		tasks = []*core.Task{}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"conversation": convo,
		"messages":     messages,
		"tasks":        tasks,
	})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	imgData, err := s.screenshot.CaptureBase64(r.Context())
	if err != nil {
		jsonError(w, fmt.Sprintf("screenshot failed: %v", err), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{
		"image":     imgData,
		"format":    "png",
		"encoding":  "base64",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		category := r.URL.Query().Get("category")
		excludeCategory := r.URL.Query().Get("excludeCategory")
		query := r.URL.Query().Get("q")

		var items []*memory.Item
		var err error

		if query != "" {
			items, err = s.memStore.Search(query, 50)
		} else {
			items, err = s.memStore.List(category)
		}

		if err != nil {
			jsonError(w, fmt.Sprintf("failed to list memory: %v", err), http.StatusInternalServerError)
			return
		}
		if excludeCategory != "" {
			filtered := items[:0]
			for _, it := range items {
				if it.Category == excludeCategory {
					continue
				}
				filtered = append(filtered, it)
			}
			items = filtered
		}
		if items == nil {
			items = []*memory.Item{}
		}
		jsonResponse(w, http.StatusOK, items)

	case http.MethodPost:
		if !s.requireControl(w, r) {
			return
		}
		var req struct {
			Category string  `json:"category"`
			Key      string  `json:"key"`
			Value    string  `json:"value"`
			Metadata *string `json:"metadata,omitempty"`
		}

		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Key == "" || req.Value == "" {
			jsonError(w, "key and value are required", http.StatusBadRequest)
			return
		}
		if req.Category == "" {
			req.Category = "general"
		}

		item, err := s.memStore.Set(req.Category, req.Key, req.Value, req.Metadata)
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to set memory: %v", err), http.StatusInternalServerError)
			return
		}

		s.auditLog.Log(audit.Entry{
			Action:   "memory_set",
			Category: "memory",
			UserID:   userFromCtx(r),
			Details:  fmt.Sprintf("%s/%s", req.Category, req.Key),
		})

		jsonResponse(w, http.StatusOK, item)

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleComputerMD reads or replaces /home/vibecraft/COMPUTER.md, the
// per-machine config file shared between the customer and the manager.
// GET → { content, path }. PUT { content } overwrites atomically.
// The file is also auto-created on first read by the agent loop via
// core.ReadOrCreateComputerMD; this endpoint stays cheap because the
// file is bounded (kilobytes at most — it's editorial markdown, not a
// database).
func (s *Server) handleComputerMD(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		content := core.ReadOrCreateComputerMD()
		jsonResponse(w, http.StatusOK, map[string]string{
			"content": content,
			"path":    core.ComputerMDPath,
		})

	case http.MethodPut:
		if !s.requireControl(w, r) {
			return
		}
		var req struct {
			Content string `json:"content"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := core.WriteComputerMD(req.Content); err != nil {
			jsonError(w, fmt.Sprintf("failed to write COMPUTER.md: %v", err), http.StatusInternalServerError)
			return
		}
		s.auditLog.Log(audit.Entry{
			Action:   "computer_md_write",
			Category: "config",
			UserID:   userFromCtx(r),
			Details:  fmt.Sprintf("wrote %d bytes", len(req.Content)),
		})
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMemoryByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/memory/")
	if id == "" {
		jsonError(w, "memory ID is required", http.StatusBadRequest)
		return
	}

	if err := s.memStore.Delete(id); err != nil {
		jsonError(w, fmt.Sprintf("failed to delete memory: %v", err), http.StatusNotFound)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "memory_deleted",
		Category: "memory",
		UserID:   userFromCtx(r),
		Details:  id,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := s.grEngine.ListRules()
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to list rules: %v", err), http.StatusInternalServerError)
			return
		}
		if rules == nil {
			rules = []guardrails.Rule{}
		}
		jsonResponse(w, http.StatusOK, rules)

	case http.MethodPost:
		if !s.requireControl(w, r) {
			return
		}
		var rule guardrails.Rule
		if err := readJSON(r, &rule); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if rule.Name == "" || rule.Pattern == "" || rule.Action == "" {
			jsonError(w, "name, pattern, and action are required", http.StatusBadRequest)
			return
		}
		if rule.ID == "" {
			rule.ID = uuid.New().String()
		}
		rule.Enabled = true

		if err := s.grEngine.AddRule(rule); err != nil {
			jsonError(w, fmt.Sprintf("failed to add rule: %v", err), http.StatusInternalServerError)
			return
		}

		s.auditLog.Log(audit.Entry{
			Action:   "rule_added",
			Category: "guardrail",
			UserID:   userFromCtx(r),
			Details:  fmt.Sprintf("%s: %s (%s)", rule.Name, rule.Pattern, rule.Action),
		})

		jsonResponse(w, http.StatusCreated, rule)

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/rules/")
	if id == "" {
		jsonError(w, "rule ID is required", http.StatusBadRequest)
		return
	}

	if err := s.grEngine.RemoveRule(id); err != nil {
		jsonError(w, fmt.Sprintf("failed to delete rule: %v", err), http.StatusNotFound)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "rule_deleted",
		Category: "guardrail",
		UserID:   userFromCtx(r),
		Details:  id,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	offset := 0

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Check for specialized filters.
	taskID := r.URL.Query().Get("task_id")
	category := r.URL.Query().Get("category")

	var entries []audit.Entry
	var err error

	if taskID != "" {
		entries, err = s.auditLog.QueryByTask(taskID)
	} else if category != "" {
		entries, err = s.auditLog.QueryByCategory(category, limit)
	} else {
		entries, err = s.auditLog.Query(limit, offset)
	}

	if err != nil {
		jsonError(w, fmt.Sprintf("failed to query audit log: %v", err), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []audit.Entry{}
	}

	jsonResponse(w, http.StatusOK, entries)
}

func (s *Server) handleVault(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Return only metadata, never values.
		secrets := s.vaultStore.List()
		jsonResponse(w, http.StatusOK, secrets)

	case http.MethodPost:
		if !s.requireControl(w, r) {
			return
		}
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Label string `json:"label"`
		}

		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Value == "" {
			jsonError(w, "name and value are required", http.StatusBadRequest)
			return
		}
		if req.Label == "" {
			req.Label = req.Name
		}

		if err := s.vaultStore.Set(req.Name, req.Value, req.Label); err != nil {
			jsonError(w, fmt.Sprintf("failed to set secret: %v", err), http.StatusInternalServerError)
			return
		}

		s.auditLog.Log(audit.Entry{
			Action:    "vault_secret_set",
			Category:  "vault",
			UserID:    userFromCtx(r),
			Details:   req.Name,
			RiskLevel: "medium",
		})

		jsonResponse(w, http.StatusOK, map[string]string{
			"status": "secret stored",
			"name":   req.Name,
		})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleVaultByName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireControl(w, r) {
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/vault/")
	if name == "" {
		jsonError(w, "secret name is required", http.StatusBadRequest)
		return
	}

	if err := s.vaultStore.Delete(name); err != nil {
		jsonError(w, fmt.Sprintf("failed to delete secret: %v", err), http.StatusNotFound)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:    "vault_secret_deleted",
		Category:  "vault",
		UserID:    userFromCtx(r),
		Details:   name,
		RiskLevel: "medium",
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleKeys dispatches POST /keys (create) and GET /keys (list).
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireControlTier(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		keys, err := s.db.ListAPIKeys()
		if err != nil {
			jsonError(w, fmt.Sprintf("failed to list keys: %v", err), http.StatusInternalServerError)
			return
		}
		if keys == nil {
			keys = []persistence.APIKey{}
		}
		jsonResponse(w, http.StatusOK, keys)

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			jsonError(w, "name is required", http.StatusBadRequest)
			return
		}

		// Generate a random API key: vc_machine_ + 32 random bytes hex encoded.
		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			jsonError(w, "failed to generate key", http.StatusInternalServerError)
			return
		}
		rawKey := "vc_machine_" + hex.EncodeToString(randomBytes)

		// Hash the key for storage.
		hash := sha256.Sum256([]byte(rawKey))
		keyHash := hex.EncodeToString(hash[:])

		// Hint is last 4 chars of the raw key.
		keyHint := rawKey[len(rawKey)-4:]

		id := uuid.New().String()

		// Get the creating user from JWT context.
		createdBy := ""
		if v, ok := r.Context().Value(ctxKeyUserID).(string); ok {
			createdBy = v
		}

		if err := s.db.CreateAPIKey(id, req.Name, keyHash, keyHint, createdBy); err != nil {
			jsonError(w, fmt.Sprintf("failed to create key: %v", err), http.StatusInternalServerError)
			return
		}

		s.auditLog.Log(audit.Entry{
			Action:   "api_key_created",
			Category: "security",
			UserID:   userFromCtx(r),
			Details:  fmt.Sprintf("name=%s hint=...%s", req.Name, keyHint),
		})

		jsonResponse(w, http.StatusCreated, map[string]string{
			"id":   id,
			"key":  rawKey,
			"hint": keyHint,
		})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleKeyByID dispatches DELETE /keys/{id} (revoke).
func (s *Server) handleKeyByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireControlTier(w, r) {
		return
	}

	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/keys/")
	if id == "" {
		jsonError(w, "key ID is required", http.StatusBadRequest)
		return
	}

	if err := s.db.RevokeAPIKey(id); err != nil {
		jsonError(w, fmt.Sprintf("failed to revoke key: %v", err), http.StatusNotFound)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "api_key_revoked",
		Category: "security",
		UserID:   userFromCtx(r),
		Details:  fmt.Sprintf("id=%s", id),
	})

	jsonResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleManagerKey(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		mode := ""
		configured := false
		setupRequired := false
		canConfigure := false
		if s.cfg != nil {
			mode = s.cfg.ManagerKeyMode
			configured = strings.TrimSpace(s.cfg.OpenAIKey) != ""
			setupRequired = s.cfg.ManagerUnavailableReason != ""
			canConfigure = mode == "operator" || (mode == "" && !configured)
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"provider":       "openai",
			"mode":           mode,
			"configured":     configured,
			"setup_required": setupRequired,
			"can_configure":  canConfigure,
		})

	case http.MethodPost:
		if !hasControlAccess(r) {
			jsonError(w, "Control access required", http.StatusForbidden)
			return
		}
		if s.cfg == nil {
			jsonError(w, "machine configuration is not ready", http.StatusServiceUnavailable)
			return
		}
		if s.cfg.ManagerKeyMode == "platform" && strings.TrimSpace(s.cfg.OpenAIKey) != "" {
			jsonError(w, "Managed computers use the included manager. No key is needed here.", http.StatusConflict)
			return
		}
		if s.cfg.ManagerKeyMode == "relay" {
			jsonError(w, "This computer uses a managed relay. Local key setup is not available.", http.StatusConflict)
			return
		}

		var req struct {
			OpenAIKey string `json:"openai_key"`
			MessageID string `json:"message_id"`
		}
		if err := readJSON(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		key := strings.TrimSpace(req.OpenAIKey)
		if !looksLikeOpenAIKey(key) {
			jsonError(w, "Enter a valid OpenAI API key.", http.StatusBadRequest)
			return
		}
		if err := writeOperatorManagerKey(key); err != nil {
			log.Printf("manager-key setup failed: %v", err)
			jsonError(w, "Could not save the key on this computer. Try again.", http.StatusInternalServerError)
			return
		}

		s.cfg.OpenAIKey = key
		s.cfg.ManagerKeyMode = "operator"
		s.cfg.ManagerUnavailableReason = ""
		if s.manager != nil {
			s.manager.SetAPIKey(key)
		}
		if s.budgetTracker != nil {
			s.budgetTracker.SetManagerKeyMode("operator")
		}

		if req.MessageID != "" && s.taskStore != nil {
			payload := markSetupStored(operatorOpenAIKeySetupPayload(req.MessageID))
			if err := s.taskStore.UpdateMessageContent(req.MessageID, setupPayloadJSON(payload)); err != nil {
				log.Printf("manager-key setup: update message %s: %v", req.MessageID, err)
			}
		}

		s.auditLog.Log(audit.Entry{
			Action:    "manager_key_configured",
			Category:  "security",
			UserID:    userFromCtx(r),
			RiskLevel: "medium",
		})
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"status":        "ready",
			"mode":          "operator",
			"configured":    true,
			"message_id":    req.MessageID,
			"configured_at": time.Now().UTC().Format(time.RFC3339),
		})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func looksLikeOpenAIKey(key string) bool {
	return strings.HasPrefix(key, "sk-") && len(key) >= 20 && !strings.ContainsAny(key, " \n\r\t")
}

func writeOperatorManagerKey(key string) error {
	if err := os.MkdirAll(managerConfigDirPath, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(openAIKeyPath, []byte(strings.TrimSpace(key)), 0600); err != nil {
		return err
	}
	if err := os.Chmod(openAIKeyPath, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(managerKeyModePath, []byte("operator"), 0600); err != nil {
		return err
	}
	return os.Chmod(managerKeyModePath, 0600)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireControl(w, r) {
		return
	}
	routeList, _ := s.routeMgr.List()

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"machine_id":     s.cfg.MachineID,
		"machine_host":   s.cfg.MachineHost,
		"port":           s.cfg.Port,
		"data_dir":       s.cfg.DataDir,
		"screenshot_dir": s.cfg.ScreenshotDir,
		"routes":         routeList,
	})
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		NewToken string `json:"new_token"`
	}

	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.NewToken == "" || len(req.NewToken) < 32 {
		jsonError(w, "new_token must be at least 32 characters", http.StatusBadRequest)
		return
	}

	// Write new token to disk.
	if err := os.WriteFile("/etc/vibecraft/daemon.token", []byte(req.NewToken), 0600); err != nil {
		jsonError(w, fmt.Sprintf("failed to write token: %v", err), http.StatusInternalServerError)
		return
	}

	s.cfg.DaemonToken = req.NewToken

	s.auditLog.Log(audit.Entry{
		Action:    "daemon_token_rotated",
		Category:  "security",
		RiskLevel: "high",
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "token rotated"})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	convoCount := 0
	convos, err := s.taskStore.ListConversations()
	if err == nil {
		convoCount = len(convos)
	}

	memCount, _ := s.memStore.Count()
	auditCount, _ := s.auditLog.Count()
	secretCount := len(s.vaultStore.List())
	managerUsage := map[string]interface{}{"note": "manager client not wired"}
	if s.manager != nil {
		managerUsage = s.manager.GetUsageStats()
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"conversations": convoCount,
		"memory_items":  memCount,
		"audit_entries": auditCount,
		"vault_secrets": secretCount,
		"manager_usage": managerUsage,
		"claude_usage":  managerUsage, // deprecated compatibility alias
		"timestamp":     time.Now().UTC(),
	})
}

func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "checkpoint_requested",
		Category: "system",
	})

	jsonResponse(w, http.StatusOK, map[string]string{
		"status":    "checkpoint complete",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleManagementRefreshJWKS forces an immediate JWKS refresh and
// clears the cache so a freshly-rotated kid is picked up within
// seconds instead of waiting for the 24h TTL (Workstream C
// incident-response rotation).
//
// Behind withHealthAuth — the platform calls this with the machine's
// health token (the platform-held per-machine credential) after
// rotating the KMS signing key. See route registration for why this
// is health-token gated and not daemon-token gated.
func (s *Server) handleManagementRefreshJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Do NOT ClearCache() before fetching. ForceRefresh → refresh()
	// builds a fresh kid→key map and swaps it in only on a successful
	// fetch (preserving the bootstrap PEM kid). Wiping the live cache
	// first meant any transient JWKS fetch failure during incident
	// response left the daemon with zero verification keys — bricking
	// every cookie/JWT until the next successful fetch. Keeping the live
	// keys until the swap makes a failed refresh a no-op for auth.
	if err := s.jwks.ForceRefresh(); err != nil {
		s.auditLog.Log(audit.Entry{
			Action:    "jwks_refresh_forced",
			Category:  "security",
			Details:   fmt.Sprintf("error=%v", err),
			RiskLevel: "high",
		})
		jsonError(w, fmt.Sprintf("refresh failed: %v", err), http.StatusInternalServerError)
		return
	}
	s.auditLog.Log(audit.Entry{
		Action:   "jwks_refresh_forced",
		Category: "security",
		Details:  "successful platform-initiated refresh",
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":    "refreshed",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleManagementRevokeSessions wipes every browser session row.
// Operators re-handshake on their next API call. Used during
// incident-response rotation (Workstream C) so a freshly-signed cookie
// is the only way back into the chat.
func (s *Server) handleManagementRevokeSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessions, err := s.db.DeleteAllSessions()
	if err != nil {
		jsonError(w, fmt.Sprintf("delete sessions: %v", err), http.StatusInternalServerError)
		return
	}
	sso, err := s.db.DeleteAllSSOSessions()
	if err != nil {
		// Don't fail the call — the primary session table is the one
		// that gates chat access. SSO is a refinement.
		log.Printf("management/revoke-sessions: delete sso failed: %v", err)
	}
	s.auditLog.Log(audit.Entry{
		Action:    "sessions_revoked",
		Category:  "security",
		Details:   fmt.Sprintf("sessions=%d sso=%d", sessions, sso),
		RiskLevel: "high",
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":   "revoked",
		"sessions": sessions,
		"sso":      sso,
	})
}

// handleManagementRevokeKeys revokes every brokered vc_machine_* daemon
// key owned by the given userId. The platform calls this (best-effort) on
// any control-access revoke / team-member removal so a removed user can't
// keep driving the machine via a cached CLI key. The vc_machine_* keys
// live only in the daemon's local SQLite (the platform never stores
// them), are validated purely locally with no expiry, and unconditionally
// grant control tier — so without this endpoint the only revoke path was
// the daemon's own JWT-gated DELETE /keys/{id}, which the platform can't
// reach. Body: {"userId":"<workos user id>"}. Returns {"revoked":<int>}.
func (s *Server) handleManagementRevokeKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		UserID string `json:"userId"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		jsonError(w, "userId is required", http.StatusBadRequest)
		return
	}

	// ListAPIKeys returns only non-revoked keys with their id + created_by
	// (owner). Revoke each one owned by the target user. Both helpers are
	// the same ones the daemon's own key management uses, so the local
	// store stays the single source of truth.
	keys, err := s.db.ListAPIKeys()
	if err != nil {
		jsonError(w, fmt.Sprintf("list keys: %v", err), http.StatusInternalServerError)
		return
	}
	revoked := 0
	for _, k := range keys {
		if k.CreatedBy != req.UserID {
			continue
		}
		if err := s.db.RevokeAPIKey(k.ID); err != nil {
			// A concurrent revoke (already gone) is fine; only log real
			// errors and keep going so one bad row can't strand the rest.
			log.Printf("management/revoke-keys: revoke %s: %v", k.ID, err)
			continue
		}
		revoked++
	}

	s.auditLog.Log(audit.Entry{
		Action:    "api_keys_revoked_for_user",
		Category:  "security",
		UserID:    req.UserID,
		Details:   fmt.Sprintf("revoked=%d", revoked),
		RiskLevel: "high",
	})
	jsonResponse(w, http.StatusOK, map[string]any{"revoked": revoked})
}

// handleManagementRevokeSessionsForUser revokes every browser session (the
// vc_session main cookie AND the vc_sso subdomain cookie) belonging to the
// given userId. The platform calls this (best-effort) from every offboarding
// path — control-access revoke, team-member removal, account deactivation —
// alongside revoke-keys. The daemon stamps the access tier into the cookie at
// handshake with a fixed multi-hour expiry and never re-checks it against
// platform access, so without this a fired/downgraded teammate keeps full
// control-tier dashboard access (tasks, vault, browser/terminal, the db.md
// company brain, guardrail edits) until the cookie expires — the browser
// analog of the CLI-key gap revoke-keys closes. Body: {"userId":"<sub>"}.
// Returns {"revoked":<int>} (sessions + sso_sessions rows deleted).
func (s *Server) handleManagementRevokeSessionsForUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		UserID string `json:"userId"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		jsonError(w, "userId is required", http.StatusBadRequest)
		return
	}

	var revoked int64
	n, err := s.db.DeleteSessionsBySub(req.UserID)
	if err != nil {
		jsonError(w, fmt.Sprintf("delete sessions: %v", err), http.StatusInternalServerError)
		return
	}
	revoked += n
	n, err = s.db.DeleteSSOSessionsBySub(req.UserID)
	if err != nil {
		jsonError(w, fmt.Sprintf("delete sso sessions: %v", err), http.StatusInternalServerError)
		return
	}
	revoked += n

	s.auditLog.Log(audit.Entry{
		Action:    "browser_sessions_revoked_for_user",
		Category:  "security",
		UserID:    req.UserID,
		Details:   fmt.Sprintf("revoked=%d", revoked),
		RiskLevel: "high",
	})
	jsonResponse(w, http.StatusOK, map[string]any{"revoked": revoked})
}

// --- CORS middleware ---

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := false

		allowedOrigins := []string{
			"https://vibecraft.so",
			"https://www.vibecraft.so",
			"https://app.vibecraft.so",
		}

		for _, o := range allowedOrigins {
			if origin == o {
				allowed = true
				break
			}
		}

		// Allow localhost only for specific development ports.
		if origin == "http://localhost:3000" || origin == "http://localhost:8420" {
			allowed = true
		}

		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// --- Rate limiter ---

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    int
	window   time.Duration
	stopCh   chan struct{}
}

type visitor struct {
	count       int
	windowStart time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitor),
		limit:    limit,
		window:   window,
		stopCh:   make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

func (rl *rateLimiter) stop() {
	close(rl.stopCh)
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, ok := rl.visitors[ip]
	if !ok || now.Sub(v.windowStart) > rl.window {
		rl.visitors[ip] = &visitor{count: 1, windowStart: now}
		return true
	}

	v.count++
	return v.count <= rl.limit
}

func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, v := range rl.visitors {
				if now.Sub(v.windowStart) > rl.window*2 {
					delete(rl.visitors, ip)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt genuinely-local control-plane traffic. Caddy ALWAYS sets
		// X-Forwarded-For when proxying external traffic (the same property
		// withLoopbackOnly relies on), so a request whose socket is loopback
		// AND carries no X-Forwarded-For can only come from a process on this
		// machine: the AI-credits proxy (high call volume from hosted tools
		// the operator built), Caddy's on-demand-TLS `ask` to /routes/verify,
		// or the manager's own loopback calls. These all share the single
		// 127.0.0.1 bucket, so one busy hosted tool issuing >60 AI calls/min
		// got 429'd AND starved Caddy's TLS-issuance ask (new hosted-tool
		// subdomains then failed to get a cert). They are already gated by
		// withLoopbackOnly / withLocalhostAuth and bounded by the per-machine
		// AI budget cap, so the coarse IP limiter only does harm here. External
		// traffic always carries X-Forwarded-For via Caddy and stays limited.
		if isLocalControlPlane(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Key on the last-hop IP (see clientIP). Keying on the spoofable
		// leftmost X-Forwarded-For let a single attacker rotate the
		// header to get unlimited fresh rate-limit buckets.
		ip := clientIP(r)

		if !rl.allow(strings.TrimSpace(ip)) {
			w.Header().Set("Retry-After", "60")
			jsonError(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isLocalControlPlane reports whether a request originates from a process on
// this machine reaching the daemon directly over loopback (no Caddy hop).
// Caddy's reverse_proxy always sets X-Forwarded-For, so loopback + absent
// X-Forwarded-For uniquely identifies in-machine control-plane callers.
func isLocalControlPlane(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host == "127.0.0.1" || host == "::1"
}

// --- Helpers ---

// userFromCtx extracts the authenticated user ID from the request context.
func userFromCtx(r *http.Request) string {
	uid, _ := r.Context().Value(ctxKeyUserID).(string)
	return uid
}

func accessFromCtx(r *http.Request) string {
	if access, _ := r.Context().Value(ctxKeyCookieAccess).(string); access != "" {
		return access
	}
	access, _ := r.Context().Value(ctxKeyAccess).(string)
	return access
}

func hasControlAccess(r *http.Request) bool {
	return accessFromCtx(r) == "control"
}

// requireControl enforces the control access tier server-side on a
// mutating request. The view/control split was previously gated only in
// the SPA, so a `view` principal that talked to the daemon directly
// could still mutate state. This is the authoritative gate.
//
// Read methods (GET/HEAD/OPTIONS) are always allowed — `view` is a
// read-only tier, not a no-access tier. Any other method on a non-control
// principal gets a 403 and the denial is audited. Returns true when the
// caller may proceed; on denial it has already written the response and
// the handler must return.
func (s *Server) requireControl(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return s.requireControlTier(w, r)
}

// requireControlTier enforces control access even for read methods. Use it
// for endpoints whose read surface leaks control-only credentials or
// administration metadata, such as machine API key management.
func (s *Server) requireControlTier(w http.ResponseWriter, r *http.Request) bool {
	if hasControlAccess(r) {
		return true
	}
	if s.auditLog != nil {
		go s.auditLog.Log(audit.Entry{
			Action:    "access_denied_view_tier",
			Category:  "security",
			UserID:    userFromCtx(r),
			Details:   fmt.Sprintf("endpoint=%s method=%s ip=%s", r.URL.Path, r.Method, remoteIP(r)),
			RiskLevel: "low",
		})
	}
	jsonError(w, "Control access required", http.StatusForbidden)
	return false
}

// withControlTier wraps a handler so that even read methods require the
// control access tier. It is the middleware form of requireControlTier,
// used at registration time to gate endpoints (like the file-read
// surface) whose authenticated middleware records the caller's tier in
// context but does not itself enforce it. Must be applied INSIDE the
// auth middleware (e.g. withCookieOrBearer(withControlTier(h))) so the
// tier is already on the context when this runs.
func (s *Server) withControlTier(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireControlTier(w, r) {
			return
		}
		next(w, r)
	}
}

// presenceUserFromCtx builds a PresenceUser from the request context.
// Cookie-authed requests carry name + email; Bearer-authed requests (API
// keys, CLI) only carry the user id and fall back to that as the
// display label. Returns a zero value if no user is on the context — the
// caller's connection will be invisible in the presence list.
func presenceUserFromCtx(r *http.Request) PresenceUser {
	uid := userFromCtx(r)
	if uid == "" {
		return PresenceUser{}
	}
	name, _ := r.Context().Value(ctxKeyCookieName).(string)
	email, _ := r.Context().Value(ctxKeyCookieEmail).(string)
	return PresenceUser{UserID: uid, Name: name, Email: email}
}

// clientIP returns the caller's IP, trusting only the last hop.
//
// The daemon sits behind exactly one trusted reverse proxy (Caddy on the
// same host — see lib/cloud-init.ts). Caddy's default reverse_proxy
// APPENDS the immediate downstream peer to X-Forwarded-For, so the
// RIGHTMOST entry is the address Caddy actually observed (the real
// client for internet traffic). Every entry to the LEFT is supplied by
// the client and is freely spoofable.
//
// The old code took the leftmost entry, which let an attacker pick any
// X-Forwarded-For value to dodge the rate limiter and poison audit IPs.
// Taking the last entry (or the socket peer when no XFF is present) ties
// the value to what the trusted hop saw.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if last != "" {
			return last
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// remoteIP is the audit-log client IP. It uses the last-hop rule (see
// clientIP) so a forged X-Forwarded-For can't be written into the audit
// trail as if it were the real source.
func remoteIP(r *http.Request) string {
	return clientIP(r)
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("jsonResponse: failed to encode: %v", err)
	}
}

func jsonError(w http.ResponseWriter, message string, status int) {
	jsonResponse(w, status, map[string]string{"error": message})
}

func readJSON(r *http.Request, v interface{}) error {
	const maxJSONBodySize = 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodySize+1))
	if err != nil {
		return err
	}
	if len(body) > maxJSONBodySize {
		return fmt.Errorf("request body exceeds %d byte limit", maxJSONBodySize)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func execDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
