# Daemon security — attack-surface inventory & hardening

The daemon is the trust root: after per-workload sandboxing it is the
only process on the host with full privileges, so every byte that
crosses into its address space is attack surface. This file inventories
those inputs (where it comes from, what parses it, what test covers it)
and tracks the Phase 6 hardening items.

## Attack-surface inventory

| Input point | Source | Parser | Coverage |
|---|---|---|---|
| HTTP `/api/*` JSON bodies | network (Caddy→:8420) / per-sandbox socket | `readJSON` per handler | handler tests (`*_test.go`) |
| Multipart upload | operator browser | `/upload` handler, `MaxBytesReader` cap | `upload_test.go` |
| Screenshot image bytes | host `scrot` (trusted) | `image/png` decode | `core` screenshot tests |
| **Per-sandbox unix socket requests** | a (possibly compromised) sandbox | the daemon mux via `SandboxServer` (socket = identity) | `sandbox/socket.go`, `daemon/local_token_test.go`, integration `PerSandboxSocket` |
| **Per-sandbox DNS proxy queries** | a (possibly compromised) sandbox | hand-rolled `parseQuestion` (`sandbox/dnsproxy.go`) | `FuzzParseDNSQuestion` (`sandbox/fuzz_test.go`), integration `FQDNAllowlistViaDNSProxy` |
| Caddyfile regen + `caddy` exec | agent-supplied route name/port via `POST /routes` JSON | `routes/manager.go` `validateRoute` (regex `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` + port range/reserved) → `buildCaddyfile` (verbatim substitution, no escaping) → `caddy validate` | `routes/manager_test.go` `TestValidateRoute_CaddyfileInjectionRejected`, `TestRegister_RejectsInjectionBeforeDBWrite` (newline/brace/space/traversal/null/over-long names rejected at the gate before reaching the template) |
| Customer-app host→sandbox forwarder | the sandboxed app + Caddy | `sandbox/app_linux.go` `proxyConn` (raw TCP copy, no parse) | integration `CustomerAppSandbox` |
| SPA static serving | network (Caddy→:8420, `/` catch-all) | `webserver.go` `//go:embed all:web/dist` → `fs.Sub` → `http.FileServer` over `embed.FS` + SPA fallback + `isAPIRoute` anti-fingerprint guard (embed.FS has no `..`/OS path semantics) | `webserver_test.go` `TestSPAHandler_PathTraversalCannotEscapeEmbed`, `TestSPAHandler_AntiFingerprintGuard`, `TestSPAHandler_FallbackAndAssetsBehaveCorrectly`, `TestIsAPIRoute` |
| Cookie / grant-code | network (`?code=` query, `vc_session`/`vc_sso` cookies) | `auth_cookie.go` `verifyGrantCode` (JWKS, RS256 pinned — rejects alg=none/HS256/forged/expired/missing-claims), `ConsumeGrantNonce` single-use, `withCookieOrBearer` CSRF content-type gate | `auth_cookie_test.go` `TestVerifyGrantCode_RejectsMalformedAndForged`, `TestConsumeGrantNonce_SingleUse`, `TestAuthCallback_RejectsNonceReplayEndToEnd`, `TestWithCookieAuth_OversizedCookieRejected`, `TestWithCookieOrBearer_CSRFGate`, `incident_auth_test.go` |
| Local-token gate | daemon-spawned shell / sandbox socket | `withLocalhostAuth` (`crypto/subtle` compare) | `local_token_test.go` |
| Health / management / incident endpoints | platform cron (health token) | `withHealthAuth` | `incident_auth_test.go` |

The two genuinely-untrusted parsers (a compromised sandbox can send
arbitrary bytes) are the **per-sandbox DNS proxy** and the **per-sandbox
unix socket**. Of these the DNS proxy hand-rolls wire-format parsing —
fuzzed (`FuzzParseDNSQuestion`); the socket reuses the audited stdlib
`net/http`.

## Findings recorded during implementation (real-environment)

These were surfaced by running the design on a real kernel / prod, not
by reasoning, and are part of the security record:

1. **Guardrail over-block on a sensitive-path trip.** A single command
   touching `/etc/vibecraft` (correctly blocked) wedged the bash tool
   for *all subsequent commands* in that conversation until daemon
   restart. The block is too sticky / too broad. **Hardening:** scope
   the block to the offending command, not the tool; clear on next
   non-sensitive command. (Owner: guardrails; tracked here.)
2. **Caddyfile generator is brick-critical.** A bad regen
   simultaneously downs the dashboard, the apex site, and every hosted
   app. Phase 4 deliberately routes sandboxed apps through a
   daemon-managed host→sandbox TCP forwarder so the generated Caddyfile
   stays byte-identical (`reverse_proxy localhost:<port>`) — same
   isolation, near-zero blast radius. Any future generator change must
   be `caddy validate`-gated and staged behind the prod-E2E loop.
3. **`/etc/resolv.conf` is a systemd-resolved symlink** bwrap cannot
   overlay, and `127.0.0.53` is unreachable from a sandbox netns. Fixed
   by the daemon DNS proxy on the per-sandbox gateway IP + binding
   resolv.conf at the symlink's resolved real path.
4. **`tini` must be `--as-pid-1` + `-s`** or zombie reaping is silently
   off (bwrap is PID 1 otherwise). Baked into `BwrapArgs`.
5. **A wholesale `/etc` ro-bind exposes `/etc/vibecraft`** — masked
   with an overlaid `--tmpfs /etc/vibecraft` applied after the bind.
6. **`--unshare-user` ⇒ `setgroups=deny` ⇒ Vixie `cron` cannot run
   jobs.** bubblewrap 0.9.0, even when run as root, unconditionally
   writes `setgroups deny` for the new user namespace. Debian/Vixie
   `cron`'s per-job child does `setgid()`+`initgroups()`+`setuid()`
   before exec; `setgroups()` returns `EPERM` under `deny`, so every
   scheduled job dies *before* exec (it logs `CMD` first, so it falsely
   looks like it ran). Found only by stracing cron's child on a real
   kernel. **The fix is NOT to drop `--unshare-user`** — that was
   evaluated and **rejected**: without the user namespace a compromised
   sandbox is *real host root* gated only by a hand-maintained
   capability blacklist (the spike's own list already retained
   `CAP_DAC_OVERRIDE`/`CAP_SETUID` and missed escalations until
   iteratively patched). The user namespace is the cornerstone of the
   isolation model (plan §"Namespace setup") and is non-negotiable.
   Resolution: the per-sandbox scheduler is **`supercronic`** (pinned +
   SHA256-verified, like the daemon binary), which runs jobs as the
   sandbox identity via `/bin/sh` with **no** privilege-drop /
   `setgroups`, so it works under full user-ns isolation. Same
   "standard small binary for the container case" precedent as `tini`
   (D5). Per-sandbox crontab is a plain file in the identity-mapped
   `$HOME` (`~/crontab`, `~/systems/<name>/crontab`) — no `/var/spool`
   bind, no setgid `crontab` helper.
7. **Agent bash tool bricked in v0.27–v0.36 (regression). TRUE root
   cause: bwrap-as-root cannot `chdir` into the bound real home
   (v0.37.0). Two earlier diagnoses (caps; bare `ip`) were wrong — both
   recorded below because the misdiagnosis trail is the lesson.** The
   one change that actually mattered was making spawn failures
   self-describing (v0.36.0): it converted nine releases of opaque
   "empty output, exit 1" into the exact error
   `bwrap: Can't chdir to /home/vibecraft: Permission denied` in a
   single prod probe. **Mechanism:** the agent-shell binds the *real*
   `/home/vibecraft` (owned by uid 1000, mode 0750). `bwrap
   --unshare-user` run **as root** maps only `0→0`, so that home is
   owned by an unmapped/overflow uid inside the userns and the
   `--chdir` EPERMs — every agent bash command dies before it runs.
   Integration tests never saw it: workers/systems use a *tmpfs* home
   owned by the sandbox itself; only the agent-shell binds a real
   foreign-owned home. **Fix (v0.37.0):** the agent-shell runs bwrap
   **as the host agent user** — `ip netns exec <ns>` (root, for the
   setns) then `setpriv --reuid=<uid> --regid=<gid> --init-groups`
   then `bwrap …`. bwrap's rootless userns then maps host-1000 →
   sandbox, so the real home is accessible (the standard bubblewrap
   model; exactly what the proven legacy `computer.Shell` path always
   did via SysProcAttr creds). Workers/systems keep root+tmpfs
   (unchanged; integration-safe). Tripwire:
   `spawn_linux_test.go::TestSpawnCmd_AgentShellDropsToHostUser`. The
   bare-`ip` → absolute-`resolveBin` change (v0.36.0) stays as correct
   latent-bug hardening, and the self-describing `Run()` is the
   permanent fix for the *meta*-bug (opaque spawn failures). **Earlier
   (incorrect) diagnoses, kept for the lesson:** caps were a
   misdiagnosis (a bwrap cap failure is verbose on stderr; the symptom
   was empty); the bare-`ip` PATH theory was real but latent (not the
   bash brick). Original trail follows. Symptom: every
   agent bash command returned empty output + exit 1, found by an E2E
   pass through the prod chat UI (a benign `echo` probe) — *not* by the
   integration suite. **First (wrong) fix:** the residual cap-drop kept
   only `{DAC_OVERRIDE,NET_ADMIN,SYS_ADMIN}`; `bwrap --unshare-user`
   id-maps need `CAP_SETUID/SETGID`, so that looked plausible and shipped
   as v0.35.0. It did **not** fix it — and in hindsight a cap failure in
   bwrap is *verbose on stderr*, inconsistent with the **empty** symptom.
   The wider keep-set `{CHOWN,DAC_OVERRIDE,FOWNER,SETGID,SETUID,
   NET_ADMIN,SYS_CHROOT,SYS_ADMIN}` is retained as correct hardening
   (guarded by `caps_linux_test.go::TestResidualKeepSet_…`), but it was
   not this bug. **True root cause:** the systemd unit
   (`/etc/systemd/system/vibecraft-daemon.service`) sets **no
   `Environment=PATH`** and **no `User=`**, so the root daemon runs with
   systemd's bare `DefaultPath`. `SpawnCmd` invoked the netns wrapper as
   the **BARE name `"ip"`**; `ip` lives in `/usr/sbin`. When the service
   PATH lacked the sbin dirs, `exec.LookPath("ip")` failed → `cmd.Run()`
   returned before any process existed → **empty output + generic exec
   error, every command, deterministic, cap-independent**, and invisible
   to the integration suite (it runs from a shell with a full PATH; many
   isolation tests don't even take the `ip netns exec` wrapper). `bwrap`
   (/usr/bin, always in PATH) and the hardcoded-absolute `/usr/bin/tini`
   were never affected — only `ip`. **Fix (v0.36.0):** `resolveBin()`
   resolves `ip`/`bwrap` to absolute paths at init (LookPath → known
   absolute fallbacks → never silently a bare name); a daemon must never
   depend on ambient PATH for the binaries it shells out to. `Run()` now
   wraps spawn failures with the program, resolution status, argv head
   and captured output **and logs them**, and returns a non-empty
   self-describing string — the opaque "empty output, exit status 1"
   that hid this for nine releases can never recur. Tripwire:
   `spawn_linux_test.go` asserts the daemon never invokes `ip`/`bwrap`
   by bare name. **Test-gap lessons (the reason this shipped nine
   times):** (a) `release.yml` (the tag→ship pipeline) runs **no tests**
   — only `ci.yml` (push to `main`) does, and only when daemon files
   change; (b) `ci.yml`'s daemon job had been **red** on an unrelated,
   pre-existing macOS-assumption test (`TestSpawnWorker_AuthAndLegacyPath`
   asserted `Supported()==false` on the Linux runner) so even its signal
   was being ignored — fixed to branch on `sandbox.Supported()`;
   (c) integration tests run privileged from a full-PATH shell and never
   exercise the daemon's actual service PATH. **Standing rule: a
   privileged daemon resolves every external binary to an absolute path,
   and a spawn failure must name itself.**

## Vulnerability checklist for new daemon contributors

Concrete, daemon-specific. Run this against any PR that adds a handler,
a parser, a route input, or a file write. Each item maps to a real
attack surface above, not generic advice.

- [ ] **Every new HTTP handler names its auth middleware in code.** Wire
      it through exactly one of `withAuth` (JWT/API key), `withCookieOrBearer`
      (browser SPA + legacy Bearer), `withHealthAuth` (platform cron /
      incident), `withLocalhostAuth` (token + loopback, e.g.
      `/daemon/task`, `/routes`), or `withLoopbackOnly` (tokenless
      loopback, `/routes/verify` only). A handler with no middleware in
      `registerRoutes` is a bug — there is no "public" tier.
- [ ] **`/routes/verify` stays `withLoopbackOnly` and only that.** It is
      tokenless by contract (Caddy on-demand-TLS `ask`); the
      `X-Forwarded-For` external-probe rejection must keep passing
      (`local_token_test.go`).
- [ ] **No string-concatenated SQL.** Every query in `persistence/`
      uses `?` placeholders passed as args to `db.conn.Exec/Query/QueryRow`
      (see `persistence/sessions.go` `ConsumeGrantNonce`:
      `Exec(\`INSERT INTO grant_nonces (nonce, ...) VALUES (?, ?, ?)\`, nonce, ...)`).
      There is zero `fmt.Sprintf` into a SQL string in the codebase —
      keep it that way; identifiers are never interpolated either.
- [ ] **Untrusted sandbox bytes go only through the two inventoried
      parsers.** A compromised per-workload sandbox may emit arbitrary
      bytes. Those reach the daemon only via (1) the per-sandbox DNS
      proxy `parseQuestion` (fuzzed) and (2) the per-sandbox unix socket
      (stdlib `net/http`, socket = identity). Do not add a third
      sandbox-facing parser; if you must, fuzz it and inventory it here
      before merge.
- [ ] **New route-name / port input passes `routes.validateRoute`.**
      The route name is substituted verbatim into the generated
      Caddyfile with no escaping (`manager.go` `buildCaddyfile`).
      `validateRoute` (DNS-label regex + port range + reserved-port
      set) is the *only* defense against Caddyfile injection — never
      build a Caddyfile fragment from an un-validated name, and never
      add a code path that reaches `buildCaddyfile` / `CreateRoute`
      before `validateRoute` has run.
- [ ] **Grant/JWT verification stays RS256-pinned.** `verifyGrantCode`
      asserts `*jwt.SigningMethodRSA`; never widen it. alg=none and
      HS256-confusion tokens must keep failing
      (`TestVerifyGrantCode_RejectsMalformedAndForged`).
- [ ] **Grant codes are single-use.** Any new grant/handshake path must
      call `ConsumeGrantNonce` (atomic UNIQUE-constraint insert) and
      reject `ErrNonceAlreadyConsumed` — replay protection is not
      optional.
- [ ] **Mutating cookie requests keep the CSRF content-type gate.** A
      cookie-authed POST/PUT/PATCH/DELETE without
      `application/json`/`multipart/form-data` must 415. Do not exempt a
      new mutating route from `withCookieOrBearer`'s gate.
- [ ] **Secrets never in argv.** Pass tokens/keys via env or files, not
      command-line args to `exec.Command` (argv is world-readable via
      `/proc`). The daemon token, vault key, and OpenAI manager key are all
      env/file-sourced — keep new secrets the same.
- [ ] **New files under `/etc/vibecraft` are `0600` and root-owned.**
      Match `config.go` `ensureRandomHexFile`/`ensureRandomBytesFile`
      and the `daemon.token` write (`os.WriteFile(..., 0600)`); a
      wholesale `/etc` ro-bind exposes this dir to sandboxes (masked by
      an overlaid tmpfs — do not regress that ordering).
- [ ] **The SPA fallback must not mask API paths.** Any new top-level
      API path prefix must be added to `webserver.go` `isAPIRoute` so a
      mux miss 404s instead of serving `index.html` (fingerprinting).
      `TestIsAPIRoute` is the contract.
- [ ] **Per-handler body-size caps stay.** Reuse `http.MaxBytesReader`
      (see `upload.go`); never read an unbounded request body into
      memory.
- [ ] **No new daemon-level orchestration/registry/spawn primitive.**
      (Product invariant, but also attack surface: each primitive is new
      privileged code on the trust root. See repo `CLAUDE.md` "What we
      will not build".)

## Hardening status

- [x] `SECURITY.md` (repo) + `daemon/SECURITY.md` (this file); attack-
      surface inventory; `security@vibecraft.so` disclosure path.
- [x] Fuzz harness for the untrusted DNS wire parser
      (`sandbox/fuzz_test.go` `FuzzParseDNSQuestion`) — gate releases on
      no crashes.
- [ ] **Capability drop (spike-first, per the plan).** Run the daemon
      with `AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN
      CAP_DAC_OVERRIDE`; run the full integration + E2E catalog;
      identify regressions (bwrap/nftables/veth/`caddy reload`). If
      clean, ship the service-user transition; else ship the
      unconditional residual `cap_set_proc` (drop all but those three
      after startup). NOT done blind — it can break the entire
      sandboxing layer; staged behind the prod-E2E loop.
- [ ] **Signed daemon releases (cosign/Sigstore).** Updater verifies
      signature before binary swap; reject unsigned. Touches the
      release pipeline — a bad verify wedges all future fleet updates,
      so staged behind a release-pipeline dry run.
- [ ] **Write-only remote audit sink.** Stream audit entries to a
      platform-side append-only sink (Managed); opt-in for BYOM.
- [ ] Pen-test pass on a freshly provisioned machine (internal first).
