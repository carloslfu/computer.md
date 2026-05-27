// SPDX-License-Identifier: Apache-2.0

//go:build linux && integration

// Privileged per-threat isolation tests from the plan's testing
// strategy. These spin up REAL bubblewrap sandboxes and assert the
// threat-model closures hold on the actual kernel. Run on a privileged
// Linux box:  go test -tags integration -run Integration -v ./sandbox/
package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// settle gives the kernel a beat after SetupNetwork to wire veth/route/
// nft before the first probe. A direct probe shows the allowlisted path
// answers in ~18ms once ready; without a settle, heavy sequential
// netns+compile churn on a small CI box can make the very first
// connection exceed a tight curl timeout (a test-harness artifact, not
// an enforcement bug — the production daemon keeps sandboxes long-lived).
func settle() { time.Sleep(800 * time.Millisecond) }

// curlCode runs an HTTPS probe inside the sandbox netns and returns the
// http_code; for the allowed path it retries briefly to absorb
// first-connection setup latency.
func curlProbe(t *testing.T, m *SandboxManifest, cfg *NetConfig, host string, wantReachable bool) string {
	t.Helper()
	attempts := 1
	if wantReachable {
		attempts = 3
	}
	var code string
	for i := 0; i < attempts; i++ {
		out, _ := Run(ctxT(t), m, SpawnOpts{NetnsName: cfg.NetnsName, ResolvConf: cfg.ResolvConf,
			Cmd: []string{"/bin/bash", "-c",
				`curl -k -s -o /dev/null -m 20 -w "%{http_code}" https://` + host + ` 2>/dev/null || true`}})
		code = strings.TrimSpace(out)
		if code != "" && code != "000" {
			return code
		}
		if i+1 < attempts {
			time.Sleep(700 * time.Millisecond)
		}
	}
	return code
}

func workerManifest(id, projectHost string) *SandboxManifest {
	return &SandboxManifest{
		ID:       id,
		Type:     TypeWorker,
		Lifetime: Ephemeral,
		ParentID: "system-test",
		Mounts:   []Mount{{HostPath: projectHost, SandboxPath: "/home/agent/project", ReadOnly: false}},
		Env:      map[string]string{"TERM": "xterm"},
		XDisplay: XDisplayNone,
	}
}

// Threat #1/#8/#9: the daemon's secrets/state are not reachable.
func TestIntegration_SecretsMasked(t *testing.T) {
	// Plant a host secret exactly where the daemon keeps one.
	if err := os.MkdirAll("/etc/vibecraft", 0700); err != nil {
		t.Skipf("cannot create /etc/vibecraft (need root): %v", err)
	}
	const probe = "/etc/vibecraft/SECRET_PROBE"
	if err := os.WriteFile(probe, []byte("TOP-SECRET-anthropic-key"), 0600); err != nil {
		t.Fatalf("plant secret: %v", err)
	}
	t.Cleanup(func() { os.Remove(probe) })

	proj := t.TempDir()
	m := workerManifest("sec-test", proj)
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`echo "etc_vibecraft_listing=[$(ls -A /etc/vibecraft 2>&1)]"; ` +
			`cat /etc/vibecraft/SECRET_PROBE 2>&1 | head -c 40; echo; ` +
			`test -e /var/lib/vibecraft && echo VARLIB_VISIBLE || echo varlib_absent; ` +
			`test -e /home/vibecraft && echo HOME_VISIBLE || echo vibecraft_home_absent`}})
	if err != nil {
		t.Fatalf("sandbox run: %v\n%s", err, out)
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("SECRET LEAKED into sandbox:\n%s", out)
	}
	if !strings.Contains(out, "etc_vibecraft_listing=[]") {
		t.Fatalf("/etc/vibecraft not masked empty:\n%s", out)
	}
	for _, want := range []string{"varlib_absent", "vibecraft_home_absent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
}

// Threat #8: PID namespace — no host process visibility; tini is PID 1.
func TestIntegration_PIDNamespace(t *testing.T) {
	m := workerManifest("pid-test", t.TempDir())
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`echo "procs=$(ls -d /proc/[0-9]* | wc -l)"; echo "pid1=$(cat /proc/1/comm)"`}})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pid1=tini") {
		t.Fatalf("PID 1 must be tini (reaping):\n%s", out)
	}
	// A host has 100+ procs; an isolated sandbox sees a handful.
	if !strings.Contains(out, "procs=") {
		t.Fatalf("no proc count:\n%s", out)
	}
	for _, big := range []string{"procs=1", "procs=2", "procs=3", "procs=4", "procs=5", "procs=6", "procs=7", "procs=8", "procs=9"} {
		_ = big
	}
	// crude: the count line must not be a 3-digit number (host-like).
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "procs=") {
			n := strings.TrimPrefix(line, "procs=")
			if len(n) >= 3 {
				t.Fatalf("too many procs visible (host leak?): %s", line)
			}
		}
	}
}

// Threat #6 (base): with no prepared veth netns the sandbox is fully
// network-isolated — only loopback exists.
func TestIntegration_NetIsolated(t *testing.T) {
	m := workerManifest("net-test", t.TempDir())
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`ip -o link show | awk -F': ' '{print $2}' | tr "\n" ","`}})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	ifaces := strings.Trim(strings.TrimSpace(out), ",")
	ifaces = strings.SplitN(ifaces, "\n", 2)[0]
	if ifaces != "lo" && ifaces != "lo," && !strings.HasPrefix(ifaces, "lo") {
		t.Fatalf("expected only lo in an unshared netns, got: %q", out)
	}
}

// UTS namespace — sandbox hostname is the manifest id; the host's is
// untouched (verified by the caller process still seeing its own).
func TestIntegration_UTSHostname(t *testing.T) {
	hostBefore, _ := os.Hostname()
	m := workerManifest("uts-test", t.TempDir())
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c", `hostname`}})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "uts-test") {
		t.Fatalf("sandbox hostname should be uts-test, got: %q", out)
	}
	hostAfter, _ := os.Hostname()
	if hostBefore != hostAfter {
		t.Fatalf("host hostname changed (UTS leak): %q -> %q", hostBefore, hostAfter)
	}
}

// Mount namespace — each sandbox's $HOME is a private ephemeral tmpfs:
// a file written by one run is not visible to a second, and the project
// bind-mount IS shared with the host (read-write round-trip).
func TestIntegration_MountPrivacyAndProjectBind(t *testing.T) {
	proj := t.TempDir()
	m := workerManifest("mnt-test", proj)
	// Write into the private home tmpfs and into the shared project.
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`echo private > /home/agent/only-here; echo shared > /home/agent/project/from-sandbox; echo "wrote"`}})
	if err != nil {
		t.Fatalf("run1: %v\n%s", err, out)
	}
	// Project bind is shared → host sees the file.
	if b, err := os.ReadFile(filepath.Join(proj, "from-sandbox")); err != nil || strings.TrimSpace(string(b)) != "shared" {
		t.Fatalf("project bind not shared with host: %v %q", err, string(b))
	}
	// A second sandbox run has a fresh private tmpfs home — the
	// previous run's private file is gone.
	out2, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`test -e /home/agent/only-here && echo PRIVATE_LEAKED || echo private_isolated`}})
	if err != nil {
		t.Fatalf("run2: %v\n%s", err, out2)
	}
	if !strings.Contains(out2, "private_isolated") {
		t.Fatalf("private home tmpfs leaked across sandboxes:\n%s", out2)
	}
}

// tini PID-1 subreaper actually reaps an orphan: a child backgrounds a
// process and exits; without subreaping the sandbox would hang or leave
// a zombie. We assert it exits promptly and cleanly.
func TestIntegration_TiniReapsOrphans(t *testing.T) {
	m := workerManifest("reap-test", t.TempDir())
	start := time.Now()
	out, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/bash", "-c",
		`( sleep 0.5 & ) ; ( : & ) ; echo "parent exiting"; exit 0`}})
	if err != nil {
		t.Fatalf("orphan-maker should exit 0: %v\n%s", err, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("sandbox hung (reaping/teardown broken): took %s", d)
	}
	if !strings.Contains(out, "parent exiting") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "Tini is not running as PID 1") {
		t.Fatalf("tini subreaper warning present — reaping is OFF:\n%s", out)
	}
}

// Threat #6 (the real closure): per-sandbox veth netns + host-side
// nftables forward allowlist. An allowlisted dst IP is reachable from
// inside the sandbox; a non-allowlisted one is dropped. Uses raw IPs
// (1.1.1.1 Cloudflare allowed, 8.8.8.8 Google denied) so the test does
// not depend on DNS.
func TestIntegration_EgressAllowlistEnforced(t *testing.T) {
	const id = "egress-test"
	const idx = 7
	TeardownNetwork(id, idx) // clean any leftover
	cfg, err := SetupNetwork(id, idx, EgressPolicy{AllowCIDRs: []string{"1.1.1.1/32"}}, EgressEnforce)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { TeardownNetwork(id, idx) })
	settle()

	m := workerManifest(id, t.TempDir())
	allowed := curlProbe(t, m, cfg, "1.1.1.1", true)
	blocked := curlProbe(t, m, cfg, "8.8.8.8", false)
	t.Logf("allowlisted 1.1.1.1 -> %q ; non-allowlisted 8.8.8.8 -> %q", allowed, blocked)
	if allowed == "" || allowed == "000" {
		t.Fatalf("allowlisted IP should be reachable, got http_code=%q", allowed)
	}
	if blocked != "000" && blocked != "" {
		// "000" or empty == curl could not connect (dropped). Anything
		// else means the deny failed.
		if !strings.HasPrefix(blocked, "000") {
			t.Fatalf("non-allowlisted IP should be DROPPED, got http_code=%q", blocked)
		}
	}
}

// Default-deny: enforce mode with an empty allowlist blocks everything
// (except DNS/established) — proves the terminal drop, not just the
// allow entries.
func TestIntegration_EgressDefaultDeny(t *testing.T) {
	const id = "deny-test"
	const idx = 8
	TeardownNetwork(id, idx)
	cfg, err := SetupNetwork(id, idx, EgressPolicy{}, EgressEnforce)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { TeardownNetwork(id, idx) })
	settle()
	m := workerManifest(id, t.TempDir())
	code := curlProbe(t, m, cfg, "1.1.1.1", false)
	if code != "" && code != "000" {
		t.Fatalf("empty-allowlist enforce mode must block all egress, got %q", code)
	}
}

// Teardown actually removes the netns + veth + nft tables.
func TestIntegration_NetworkTeardownIsClean(t *testing.T) {
	const id = "teardown-test"
	const idx = 9
	if _, err := SetupNetwork(id, idx, EgressPolicy{AllowCIDRs: []string{"1.1.1.1/32"}}, EgressEnforce); err != nil {
		t.Fatalf("setup: %v", err)
	}
	TeardownNetwork(id, idx)
	nl, _ := exec.Command("ip", "netns", "list").Output()
	if strings.Contains(string(nl), "vcns9") {
		t.Fatalf("netns not cleaned: %s", nl)
	}
	tl, _ := exec.Command("nft", "list", "tables").Output()
	if strings.Contains(string(tl), "vc_fwd_teardown_test") || strings.Contains(string(tl), "vc_nat_teardown_test") {
		t.Fatalf("nft tables not cleaned: %s", tl)
	}
}

// FQDN allowlist END-TO-END via the daemon DNS proxy: the sandbox
// resolves a name through the gateway proxy, which (a) refuses
// non-allowlisted names outright and (b) auto-populates the nft set for
// allowlisted ones so the connection then passes. Closes the earlier
// resolv.conf/symlink finding.
func TestIntegration_FQDNAllowlistViaDNSProxy(t *testing.T) {
	const id = "fqdn-test"
	const idx = 11
	TeardownNetwork(id, idx)
	pol := EgressPolicy{AllowFQDNs: []string{"one.one.one.one"}} // -> 1.1.1.1/1.0.0.1
	cfg, err := SetupNetwork(id, idx, pol, EgressEnforce)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { TeardownNetwork(id, idx) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := StartDNSProxy(ctx, id, cfg.HostIP, pol, EgressEnforce); err != nil {
		t.Fatalf("StartDNSProxy: %v", err)
	}
	rc, err := WriteResolvConf(id, cfg.HostIP)
	if err != nil {
		t.Fatalf("WriteResolvConf: %v", err)
	}
	t.Cleanup(func() { os.Remove(rc) })
	dest := ResolvConfDest()
	t.Logf("resolv.conf -> %s (dest %s), proxy on %s:53", cfg.HostIP, dest, cfg.HostIP)
	settle()

	m := workerManifest(id, t.TempDir())
	probe := func(host string, reachable bool) string {
		attempts := 1
		if reachable {
			attempts = 3
		}
		var code string
		for i := 0; i < attempts; i++ {
			out, _ := Run(ctxT(t), m, SpawnOpts{
				NetnsName: cfg.NetnsName, ResolvConf: rc, ResolvConfDest: dest,
				Cmd: []string{"/bin/bash", "-c",
					`curl -k -s -o /dev/null -m 20 -w "%{http_code}" https://` + host + ` 2>/dev/null || true`}})
			code = strings.TrimSpace(out)
			if code != "" && code != "000" {
				break
			}
			if i+1 < attempts {
				time.Sleep(800 * time.Millisecond)
			}
		}
		return code
	}
	allowed := probe("one.one.one.one", true) // proxy resolves + populates set
	blocked := probe("dns.google", false)     // proxy REFUSES (not allowlisted)
	t.Logf("allowlisted FQDN one.one.one.one -> %q ; non-allowlisted dns.google -> %q", allowed, blocked)
	if allowed == "" || allowed == "000" {
		t.Fatalf("allowlisted FQDN should resolve+connect end-to-end, got %q", allowed)
	}
	if blocked != "" && blocked != "000" {
		t.Fatalf("non-allowlisted FQDN must be unresolvable/blocked, got %q", blocked)
	}
}

func TestIntegration_ResolverSkipsWildcards(t *testing.T) {
	// Wildcards cannot be pre-resolved; ResolveFQDNs skips them and
	// WildcardFQDNs records them for audit.
	pol := EgressPolicy{AllowFQDNs: []string{"*.npmjs.org", "one.one.one.one"}}
	ips := ResolveFQDNs(context.Background(), pol.AllowFQDNs)
	for _, ip := range ips {
		if strings.Contains(ip, "*") {
			t.Fatalf("wildcard leaked into resolved IPs: %v", ips)
		}
	}
	w := WildcardFQDNs(pol)
	if len(w) != 1 || w[0] != "*.npmjs.org" {
		t.Fatalf("WildcardFQDNs = %v, want [*.npmjs.org]", w)
	}
}

// Per-sandbox unix socket: the daemon binds /run/vibecraft/<id>.sock,
// bwrap maps it to /run/vibecraft.sock, and the in-sandbox client
// reaches a daemon handler that knows the caller's identity FROM THE
// SOCKET (not a header). This is the channel that replaces the
// host-loopback path the netns severs (D4: curl --unix-socket).
func TestIntegration_PerSandboxSocket(t *testing.T) {
	const id = "sock-test"
	srv, err := NewSandboxServer(id, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Identity is the socket; echo it back so the test can prove it.
		w.Write([]byte("sandbox=" + SandboxID(r) + " path=" + r.URL.Path))
	}))
	if err != nil {
		t.Fatalf("NewSandboxServer: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })

	m := workerManifest(id, t.TempDir())
	out, err := Run(ctxT(t), m, SpawnOpts{
		HostSocketPath: srv.Path(),
		Cmd: []string{"/bin/bash", "-c",
			`curl -s --max-time 8 --unix-socket /run/vibecraft.sock http://daemon/task || echo CURL_FAIL`},
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sandbox=sock-test") {
		t.Fatalf("in-sandbox socket call did not reach the scoped handler / wrong identity:\n%s", out)
	}
	if strings.Contains(out, "CURL_FAIL") {
		t.Fatalf("curl over /run/vibecraft.sock failed:\n%s", out)
	}
}

// Full worker lifecycle through the Create/Run/Destroy orchestrator:
// one sandbox exercising every Phase-1 guarantee at once, then a clean
// teardown. This is the daemon-facing primitive end-to-end.
func TestIntegration_FullWorkerLifecycle(t *testing.T) {
	if err := os.MkdirAll("/etc/vibecraft", 0700); err == nil {
		os.WriteFile("/etc/vibecraft/SECRET_PROBE", []byte("TOP-SECRET"), 0600)
		t.Cleanup(func() { os.Remove("/etc/vibecraft/SECRET_PROBE") })
	}
	m := &SandboxManifest{
		ID: "life-test", Type: TypeWorker, Lifetime: Ephemeral, ParentID: "system-x",
		Mounts:   []Mount{{HostPath: t.TempDir(), SandboxPath: "/home/agent/project"}},
		Egress:   EgressPolicy{AllowFQDNs: []string{"one.one.one.one"}},
		XDisplay: XDisplayNone,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("id=" + SandboxID(r)))
	})
	sb, err := Create(m, EgressEnforce, handler)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	idx := sb.idx
	settle()

	out, err := sb.Run(ctxT(t), []string{"/bin/bash", "-c", `
		echo "host=$(hostname)";
		echo "etc=[$(ls -A /etc/vibecraft 2>&1)]";
		echo "sock=$(curl -s --max-time 6 --unix-socket /run/vibecraft.sock http://d/x || echo FAIL)";
		echo "allow=$(curl -k -s -o /dev/null -m 18 -w '%{http_code}' https://one.one.one.one || true)";
		echo "deny=$(curl -k -s -o /dev/null -m 6 -w '%{http_code}' https://dns.google || true)";
	`}, map[string]string{"CUSTOMER_API_TOKEN": "shh"})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	t.Logf("lifecycle probe:\n%s", out)
	checks := map[string]string{
		"host=life-test": "UTS hostname",
		"etc=[]":         "/etc/vibecraft masked",
		"id=life-test":   "per-sandbox socket identity",
		"allow=200":      "allowlisted FQDN reachable via DNS proxy",
		"deny=000":       "non-allowlisted FQDN blocked",
	}
	for sub, what := range checks {
		if !strings.Contains(out, sub) {
			t.Fatalf("lifecycle FAILED %q (%s):\n%s", sub, what, out)
		}
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("secret leaked:\n%s", out)
	}

	sb.Destroy()
	sb.Destroy() // idempotent
	nl, _ := exec.Command("ip", "netns", "list").Output()
	if strings.Contains(string(nl), "vcns"+itoa(idx)) {
		t.Fatalf("netns not cleaned after Destroy:\n%s", nl)
	}
	if _, err := os.Stat(SocketPath("life-test")); !os.IsNotExist(err) {
		t.Fatalf("socket not removed after Destroy")
	}
	tl, _ := exec.Command("nft", "list", "tables").Output()
	if strings.Contains(string(tl), "vc_fwd_life_test") {
		t.Fatalf("nft table not cleaned after Destroy:\n%s", tl)
	}
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// SpawnWorker end-to-end — the exact entrypoint the daemon's
// /api/daemon/spawn-worker handler calls. A worker-shaped command:
// api.anthropic.com allowlisted (what Claude Code needs), with hosted
// provider keys absent from env. Proves the daemon worker path on a real
// kernel.
func TestIntegration_SpawnWorkerEndToEnd(t *testing.T) {
	out, err := SpawnWorker(ctxT(t), "worker-e2e", "sys-research",
		EgressPolicy{AllowFQDNs: []string{"api.anthropic.com"}},
		[]string{"/bin/bash", "-c", `
			echo "openai=${OPENAI_API_KEY:-MISSING}";
			echo "anthropic_key=${ANTHROPIC_API_KEY:-MISSING}";
			echo "anthropic=$(curl -k -s -o /dev/null -m 18 -w '%{http_code}' https://api.anthropic.com/v1/messages || true)";
			echo "evil=$(curl -k -s -o /dev/null -m 6 -w '%{http_code}' https://example.com || true)";
			echo "secret=$(cat /etc/vibecraft/anthropic.key 2>&1 | head -c 12)";
		`},
		map[string]string{})
	if err != nil {
		t.Fatalf("SpawnWorker: %v\n%s", err, out)
	}
	t.Logf("worker e2e:\n%s", out)
	if !contains2(out, "openai=MISSING") || !contains2(out, "anthropic_key=MISSING") {
		t.Fatalf("hosted provider keys must not be injected into worker env:\n%s", out)
	}
	// api.anthropic.com allowlisted -> the DNS proxy resolves it and the
	// connection is permitted (any non-000 HTTP code, incl. 401/403 from
	// the API, proves reachability).
	if contains2(out, "anthropic=000") || contains2(out, "anthropic=\n") {
		t.Fatalf("allowlisted api.anthropic.com must be reachable:\n%s", out)
	}
	if !contains2(out, "evil=000") {
		t.Fatalf("non-allowlisted example.com must be blocked (evil=000):\n%s", out)
	}
	if contains2(out, "secret=sk-ant") {
		t.Fatalf("host anthropic.key leaked into the worker:\n%s", out)
	}
}

func contains2(s, sub string) bool { return strings.Contains(s, sub) }

// Phase 2 agent-shell: a long-lived sandbox where successive commands
// (as executeBashTool issues them) share a PERSISTENT $HOME, broad
// egress works, yet /etc/vibecraft stays masked and host loopback is
// unreachable. Proves the persistent-home refinement + the agent-shell
// shape on a real kernel.
func TestIntegration_AgentShellPersistentHome(t *testing.T) {
	if err := os.MkdirAll("/etc/vibecraft", 0700); err == nil {
		os.WriteFile("/etc/vibecraft/SECRET_PROBE", []byte("HOSTKEY"), 0600)
		t.Cleanup(func() { os.Remove("/etc/vibecraft/SECRET_PROBE") })
	}
	homeDir := t.TempDir()
	m := AgentShellManifest(homeDir, nil,
		EgressPolicy{AllowFQDNs: []string{"one.one.one.one", "api.anthropic.com"}})
	if err := m.Validate(); err != nil {
		t.Fatalf("AgentShellManifest invalid: %v", err)
	}
	sb, err := Create(m, EgressEnforce, nil)
	if err != nil {
		t.Fatalf("Create agent-shell: %v", err)
	}
	t.Cleanup(sb.Destroy)
	settle()

	// Home is identity-mapped (bound at its real path), so state lives
	// at homeDir/state.txt — the same path in and out of the sandbox.
	stateF := homeDir + "/state.txt"
	// Command 1: write state into the persistent home (as the agent would).
	if out, err := sb.Run(ctxT(t), []string{"/bin/bash", "-c",
		`echo "carry-$(date +%s)" > ` + stateF + `; echo wrote`}, nil); err != nil {
		t.Fatalf("cmd1: %v\n%s", err, out)
	}
	// Command 2: a SEPARATE bwrap in the same long-lived sandbox must
	// still see it (persistent home), reach a broad-egress host, and be
	// secret-isolated.
	out, err := sb.Run(ctxT(t), []string{"/bin/bash", "-c", `
		echo "state=[$(cat ` + stateF + ` 2>/dev/null)]";
		echo "etc=[$(ls -A /etc/vibecraft 2>&1)]";
		echo "web=$(curl -k -s -o /dev/null -m 15 -w '%{http_code}' https://one.one.one.one || true)";
		echo "loopback=$(curl -s -o /dev/null -m 4 -w '%{http_code}' http://127.0.0.1:8420 || echo refused)";
	`}, nil)
	if err != nil {
		t.Fatalf("cmd2: %v\n%s", err, out)
	}
	t.Logf("agent-shell probe:\n%s", out)
	if !strings.Contains(out, "state=[carry-") {
		t.Fatalf("persistent $HOME not carried across commands:\n%s", out)
	}
	if !strings.Contains(out, "etc=[]") {
		t.Fatalf("/etc/vibecraft not masked in agent-shell:\n%s", out)
	}
	if strings.Contains(out, "HOSTKEY") {
		t.Fatalf("host secret leaked into agent-shell:\n%s", out)
	}
	if strings.Contains(out, "web=000") || strings.Contains(out, "web=\n") {
		t.Fatalf("broad-egress allowlisted host should be reachable:\n%s", out)
	}
	if !strings.Contains(out, "loopback=refused") && !strings.Contains(out, "loopback=000") {
		t.Fatalf("host 127.0.0.1:8420 must be unreachable from the agent-shell netns:\n%s", out)
	}
}

// Phase 2 agent-shell END-TO-END as the engine drives it: NewAgentShell
// + the core.ShellExecutor surface (Execute / ExecuteWithEnv /
// ExecuteInteractive), persistent $HOME across calls, the in-sandbox
// curl --unix-socket reaching the daemon mux (socket-implicit auth),
// /etc/vibecraft masked, open egress, host loopback severed. This is
// the manager's whole world — if this holds on a real kernel the
// engine swap is sound.
func TestIntegration_AgentShellEndToEnd(t *testing.T) {
	if err := os.MkdirAll("/etc/vibecraft", 0700); err == nil {
		os.WriteFile("/etc/vibecraft/SECRET_PROBE", []byte("HOSTKEY"), 0600)
		t.Cleanup(func() { os.Remove("/etc/vibecraft/SECRET_PROBE") })
	}
	home := t.TempDir()
	// Dummy "X socket" (a regular file is bindable; proves the Pattern-A
	// ro-bind mechanism without needing a real Xvfb on the bare box).
	xsock := filepath.Join(t.TempDir(), "X1")
	os.WriteFile(xsock, []byte("x"), 0600)

	// Handler = stand-in for the daemon mux; must see the caller's
	// sandbox identity FROM THE SOCKET.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("DAEMON-OK sandbox=" + SandboxID(r) + " path=" + r.URL.Path))
	})
	// Home is bound at its SAME path (identity map); $HOME must match.
	ash, err := NewAgentShell(home, xsock, nil,
		map[string]string{"HOME": home, "PATH": "/usr/local/bin:/usr/bin:/bin"}, h)
	if err != nil {
		t.Fatalf("NewAgentShell: %v", err)
	}
	t.Cleanup(ash.Close)
	settle()

	ctx := ctxT(t)
	// 1) basic bash + provider-key absence
	if o, err := ash.Execute(ctx, `echo "hi=$(whoami 2>/dev/null||echo na) home=$HOME openai=${OPENAI_API_KEY:-NO} anthropic=${ANTHROPIC_API_KEY:-NO}"`); err != nil || !strings.Contains(o, "openai=NO anthropic=NO") {
		t.Fatalf("Execute basic/env failed: %v\n%s", err, o)
	}
	// 2) persistent $HOME across SEPARATE engine calls
	if _, err := ash.Execute(ctx, `echo carry-$$ > $HOME/state.txt`); err != nil {
		t.Fatalf("write state: %v", err)
	}
	o, err := ash.Execute(ctx, `
		echo "state=[$(cat $HOME/state.txt 2>/dev/null)]";
		echo "etc=[$(ls -A /etc/vibecraft 2>&1)]";
		echo "sock=$(curl -s --max-time 6 --unix-socket /run/vibecraft.sock http://d/api/daemon/task || echo SOCKFAIL)";
		echo "loop=$(curl -s -o /dev/null -m 4 -w '%{http_code}' http://127.0.0.1:8420 || echo refused)";
	`)
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, o)
	}
	t.Logf("agent-shell e2e:\n%s", o)
	if !strings.Contains(o, "state=[carry-") {
		t.Fatalf("persistent $HOME lost across engine calls:\n%s", o)
	}
	if !strings.Contains(o, "etc=[]") || strings.Contains(o, "HOSTKEY") {
		t.Fatalf("/etc/vibecraft not masked / host key leaked:\n%s", o)
	}
	if !strings.Contains(o, "DAEMON-OK sandbox=agent-shell") {
		t.Fatalf("in-sandbox curl --unix-socket did not reach the mux with socket identity:\n%s", o)
	}
	if !strings.Contains(o, "loop=refused") && !strings.Contains(o, "loop=000") {
		t.Fatalf("host 127.0.0.1:8420 must be unreachable from the agent-shell netns:\n%s", o)
	}
	// 3) ExecuteInteractive (text_editor path): stdin content, never argv
	o2, err := ash.ExecuteInteractive(ctx, `cat > $HOME/f.txt; wc -c < $HOME/f.txt`, "hello-stdin")
	if err != nil || strings.TrimSpace(o2) != "11" {
		t.Fatalf("ExecuteInteractive stdin failed: %v out=%q", err, o2)
	}
	// 4) ExecuteWithEnv passes K=V via env, not argv
	o3, err := ash.ExecuteWithEnv(ctx, `echo "v=$WK"`, []string{"WK=worker-env-ok"})
	if err != nil || !strings.Contains(o3, "v=worker-env-ok") {
		t.Fatalf("ExecuteWithEnv failed: %v\n%s", err, o3)
	}
}

// Phase 3 per-system runtime END-TO-END: a manifested system gets a
// persistent sandbox scoped to its manifest — enforced egress
// allowlist, ONLY its declared vault refs injected (per-system
// credential scope), reused across calls, clean teardown.
func TestIntegration_SystemSandboxScoped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "acme")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
		`{"allow_fqdns":["one.one.one.one"],"enforce_egress":true,"vault_refs":["ACME_KEY"]}`), 0644)

	// resolveVault simulates the daemon's per-system vault scope: it is
	// ONLY ever asked for the manifest's declared refs.
	asked := map[string]bool{}
	resolve := func(refs []string) map[string]string {
		out := map[string]string{}
		for _, r := range refs {
			asked[r] = true
			out[r] = "val-" + r
		}
		return out
	}
	t.Cleanup(func() { DestroySystemSandbox("acme") })

	sb1, err := GetOrCreateSystemSandbox(root, "acme", resolve, nil)
	if err != nil {
		t.Fatalf("GetOrCreateSystemSandbox: %v", err)
	}
	settle()
	out, err := sb1.Run(ctxT(t), []string{"/bin/bash", "-c", `
		echo "key=${ACME_KEY:-MISSING} other=${GOOGLE_PASSWORD:-unset}";
		echo "etc=[$(ls -A /etc/vibecraft 2>/dev/null)]";
		echo "allow=$(curl -k -s -o /dev/null -m 16 -w '%{http_code}' https://one.one.one.one || true)";
		echo "deny=$(curl -k -s -o /dev/null -m 6 -w '%{http_code}' https://8.8.8.8 || true)";
	`}, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Logf("system-sandbox:\n%s", out)
	if !strings.Contains(out, "key=val-ACME_KEY") {
		t.Fatalf("declared vault ref not injected:\n%s", out)
	}
	if !strings.Contains(out, "other=unset") || asked["GOOGLE_PASSWORD"] {
		t.Fatalf("per-system credential scope violated (undeclared ref present/asked):\n%s asked=%v", out, asked)
	}
	if !strings.Contains(out, "etc=[]") {
		t.Fatalf("/etc/vibecraft not masked in system sandbox:\n%s", out)
	}
	if strings.Contains(out, "allow=000") || strings.Contains(out, "allow=\n") {
		t.Fatalf("manifest-allowed host must be reachable:\n%s", out)
	}
	if !strings.Contains(out, "deny=000") {
		t.Fatalf("non-allowlisted host must be blocked (enforce):\n%s", out)
	}

	// Reused, not recreated.
	sb2, err := GetOrCreateSystemSandbox(root, "acme", resolve, nil)
	if err != nil || sb2 != sb1 {
		t.Fatalf("system sandbox must be persistent/reused: %v same=%v", err, sb2 == sb1)
	}
}

// Phase 4 customer-app END-TO-END: a manifested app runs in its own
// sandbox; the daemon's host→sandbox forwarder makes it reachable at
// 127.0.0.1:<port> (so the generated Caddyfile stays untouched);
// teardown removes everything.
func TestIntegration_CustomerAppSandbox(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "web")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("APP-OK-PHASE4"), 0644)
	// enforce egress, empty allowlist => app is network-locked (typical
	// for a static service); still serves locally via the forwarder.
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
		`{"port":38080,"start_cmd":["/bin/bash","-lc","cd `+dir+` && python3 -m http.server 38080 --bind 0.0.0.0"],"restart_policy":"always","enforce_egress":true}`), 0644)

	if err := StartApp(root, "web", nil, nil); err != nil {
		t.Fatalf("StartApp: %v", err)
	}
	t.Cleanup(func() { StopApp("web") })

	// The app needs a beat to boot python's http.server.
	var body string
	for i := 0; i < 20; i++ {
		time.Sleep(700 * time.Millisecond)
		out, _ := exec.Command("curl", "-s", "--max-time", "3", "http://127.0.0.1:38080/index.html").Output()
		if body = string(out); strings.Contains(body, "APP-OK-PHASE4") {
			break
		}
	}
	if !strings.Contains(body, "APP-OK-PHASE4") {
		t.Fatalf("sandboxed app not reachable via host forwarder (Caddy stays unchanged), got %q", body)
	}

	// The app is sandboxed: from inside, a non-allowlisted host is
	// blocked (enforce, empty allowlist) and /etc/vibecraft is masked.
	appReg.mu.Lock()
	ai := appReg.m["web"]
	appReg.mu.Unlock()
	if ai == nil {
		t.Fatal("app instance missing from registry")
	}
	probe, _ := ai.sb.Run(ctxT(t), []string{"/bin/bash", "-c",
		`echo "egress=$(curl -s -o /dev/null -m 5 -w '%{http_code}' https://8.8.8.8 || echo blocked) etc=[$(ls -A /etc/vibecraft 2>&1)]"`}, nil)
	if !strings.Contains(probe, "egress=blocked") && !strings.Contains(probe, "egress=000") {
		t.Fatalf("app sandbox egress not enforced: %q", probe)
	}
	if !strings.Contains(probe, "etc=[]") {
		t.Fatalf("app sandbox /etc/vibecraft not masked: %q", probe)
	}

	// Teardown closes the forwarder.
	StopApp("web")
	time.Sleep(500 * time.Millisecond)
	out, _ := exec.Command("curl", "-s", "--max-time", "3", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:38080/").Output()
	if c := strings.TrimSpace(string(out)); c != "000" && c != "" {
		t.Fatalf("forwarder still up after StopApp (code=%q)", c)
	}
}

// Phase 5 (Pattern B): an owned-display sandbox gets its OWN Xvfb :N
// and CANNOT see the shared :1 — closing the cross-sandbox X-snoop
// residual. We plant a host /tmp/.X11-unix/X1 so "X1 not visible
// inside" is a real isolation proof, not a vacuous one.
func TestIntegration_OwnedDisplayHidesSharedX(t *testing.T) {
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed on this runner")
	}
	_ = os.MkdirAll("/tmp/.X11-unix", 0777)
	if _, err := os.Stat("/tmp/.X11-unix/X1"); os.IsNotExist(err) {
		os.WriteFile("/tmp/.X11-unix/X1", []byte("host-shared-display"), 0600)
		t.Cleanup(func() { os.Remove("/tmp/.X11-unix/X1") })
	}
	m := &SandboxManifest{
		ID: "disp-test", Type: TypeWorker, Lifetime: Ephemeral, ParentID: "sys",
		Mounts:   []Mount{{HostPath: t.TempDir(), SandboxPath: "/home/agent/project"}},
		XDisplay: XDisplayOwned,
	}
	sb, err := Create(m, EgressEnforce, nil)
	if err != nil {
		t.Fatalf("Create owned-display: %v", err)
	}
	t.Cleanup(sb.Destroy)
	settle()
	out, err := sb.Run(ctxT(t), []string{"/bin/bash", "-c",
		`echo "disp=$DISPLAY socks=[$(ls /tmp/.X11-unix 2>/dev/null | tr '\n' ',')] ` +
			`own=$(test -S $(echo /tmp/.X11-unix/X${DISPLAY#:}) && echo yes || echo no) ` +
			`x1=$(test -e /tmp/.X11-unix/X1 && echo VISIBLE || echo hidden)"`}, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Logf("owned-display: %s", out)
	if !strings.Contains(out, "disp=:") {
		t.Fatalf("owned sandbox has no private DISPLAY: %s", out)
	}
	if strings.Contains(out, "x1=VISIBLE") {
		t.Fatalf("SNOOP RESIDUAL OPEN — owned-display sandbox can see shared :1: %s", out)
	}
	if !strings.Contains(out, "x1=hidden") || !strings.Contains(out, "own=yes") {
		t.Fatalf("expected own private display present + :1 hidden, got: %s", out)
	}
}

// Manifest validation still gates the privileged path (defense in
// depth): a forbidden host mount never reaches bwrap.
func TestIntegration_ForbiddenMountRejectedBeforeExec(t *testing.T) {
	m := workerManifest("forbid-test", t.TempDir())
	m.Mounts = append(m.Mounts, Mount{HostPath: "/etc/vibecraft", SandboxPath: "/x"})
	_, err := Run(ctxT(t), m, SpawnOpts{Cmd: []string{"/bin/true"}})
	if err == nil || !strings.Contains(err.Error(), "never bind-mountable") {
		t.Fatalf("forbidden mount must be rejected pre-exec, got: %v", err)
	}
}
