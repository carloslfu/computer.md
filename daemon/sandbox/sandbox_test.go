// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validWorker() SandboxManifest {
	return SandboxManifest{
		ID:       "worker-23",
		Type:     TypeWorker,
		Lifetime: Ephemeral,
		ParentID: "system-invoice-bot",
		Mounts: []Mount{
			{HostPath: "/home/vibecraft/systems/invoice-bot", SandboxPath: "/home/agent/project", ReadOnly: false},
		},
		Env:      map[string]string{"WORKER_TOKEN": "x", "TERM": "xterm"},
		Egress:   EgressPolicy{AllowFQDNs: []string{"api.anthropic.com", "*.npmjs.org"}, AllowCIDRs: []string{"10.0.0.0/8"}},
		XDisplay: XDisplayShared,
	}
}

func TestManifestValidate_Good(t *testing.T) {
	m := validWorker()
	if err := m.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestManifestValidate_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SandboxManifest)
		want   string
	}{
		{"bad id chars", func(m *SandboxManifest) { m.ID = "Worker_23!" }, "invalid sandbox id"},
		{"empty id", func(m *SandboxManifest) { m.ID = "" }, "invalid sandbox id"},
		{"id too long", func(m *SandboxManifest) { m.ID = strings.Repeat("a", 64) }, "invalid sandbox id"},
		{"worker without parent", func(m *SandboxManifest) { m.ParentID = "" }, "must declare a ParentID"},
		{"non-worker with parent", func(m *SandboxManifest) { m.Type = TypeSystem }, "only worker sandboxes may set ParentID"},
		{"relative host mount", func(m *SandboxManifest) { m.Mounts[0].HostPath = "rel/path" }, "must be absolute"},
		{"relative sandbox mount", func(m *SandboxManifest) { m.Mounts[0].SandboxPath = "rel" }, "must be absolute"},
		{"forbidden /etc/vibecraft mount", func(m *SandboxManifest) {
			m.Mounts[0].HostPath = "/etc/vibecraft/openai.key"
		}, "never bind-mountable"},
		{"forbidden /var/lib/vibecraft mount", func(m *SandboxManifest) {
			m.Mounts[0].HostPath = "/var/lib/vibecraft"
		}, "never bind-mountable"},
		{"forbidden bashrc mount", func(m *SandboxManifest) {
			m.Mounts[0].HostPath = "/home/vibecraft/.bashrc"
		}, "never bind-mountable"},
		{"bad env name", func(m *SandboxManifest) { m.Env = map[string]string{"BAD=NAME": "x"} }, "invalid env var name"},
		{"bad egress fqdn", func(m *SandboxManifest) { m.Egress.AllowFQDNs = []string{"not a domain"} }, "invalid egress FQDN"},
		{"bad egress cidr", func(m *SandboxManifest) { m.Egress.AllowCIDRs = []string{"999.999/x"} }, "invalid egress CIDR"},
		{"bad xdisplay", func(m *SandboxManifest) { m.XDisplay = XDisplayMode(99) }, "invalid XDisplay"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validWorker()
			c.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("expected rejection containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestForbiddenHostMount_PrefixNotSubstring(t *testing.T) {
	// /var/lib/vibecraft-decoy is NOT under /var/lib/vibecraft/ — a naive
	// HasPrefix without the trailing slash would wrongly reject it.
	m := validWorker()
	m.Mounts[0].HostPath = "/var/lib/vibecraft-decoy/data"
	if err := m.Validate(); err != nil {
		t.Fatalf("sibling path wrongly rejected as forbidden: %v", err)
	}
	m.Mounts[0].HostPath = "/var/lib/vibecraft"
	if err := m.Validate(); err == nil {
		t.Fatal("exact forbidden path should be rejected")
	}
}

func TestSortedEnvKeysDeterministic(t *testing.T) {
	m := SandboxManifest{Env: map[string]string{"Z": "1", "A": "2", "M": "3"}}
	got := strings.Join(m.SortedEnvKeys(), ",")
	if got != "A,M,Z" {
		t.Fatalf("SortedEnvKeys = %q, want A,M,Z", got)
	}
}

func TestNameDerivation(t *testing.T) {
	if got := CgroupName("worker-23"); got != "vibecraft.sandbox.worker-23" {
		t.Fatalf("CgroupName = %q", got)
	}
	if got := NftSetName("worker-23"); got != "allow_worker_23" {
		t.Fatalf("NftSetName = %q", got)
	}
	// veth names must fit Linux's 15-byte IFNAMSIZ-1 limit.
	long := "system-a-very-long-sandbox-identifier"
	for _, n := range []string{VethHostName(long), VethSandboxName(long)} {
		if len(n) > 15 {
			t.Fatalf("veth name %q exceeds 15 bytes (%d)", n, len(n))
		}
	}
	if VethHostName("x") == VethSandboxName("x") {
		t.Fatal("host and sandbox veth names must differ")
	}
}

func TestRenderNftables_Enforce(t *testing.T) {
	pol := EgressPolicy{AllowCIDRs: []string{"10.0.0.0/8", "192.168.1.0/24"}}
	out, err := RenderNftables("worker-23", pol, EgressEnforce)
	if err != nil {
		t.Fatalf("RenderNftables: %v", err)
	}
	for _, want := range []string{
		"table inet vc_sb_worker_23 {",
		"set allow_worker_23 {",
		"socket cgroupv2 level 2 \"vibecraft.sandbox.worker-23\" jump sandbox_egress",
		"ct state established,related accept",
		"ip daddr 10.0.0.0/8 accept",
		"ip daddr 192.168.1.0/24 accept",
		"ip daddr @allow_worker_23 accept",
		"udp dport 53 accept",
		"log prefix \"vc-egress-drop \" drop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("enforce ruleset missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "vc-egress-audit") {
		t.Fatalf("enforce mode must not contain the audit-accept line\n%s", out)
	}
}

func TestRenderNftables_AuditDoesNotDrop(t *testing.T) {
	out, err := RenderNftables("system-bookkeeping", EgressPolicy{AllowCIDRs: []string{"1.2.3.0/24"}}, EgressAudit)
	if err != nil {
		t.Fatalf("RenderNftables: %v", err)
	}
	if !strings.Contains(out, "log prefix \"vc-egress-audit \" accept") {
		t.Fatalf("audit mode must log+accept, got:\n%s", out)
	}
	if strings.Contains(out, " drop\n") {
		t.Fatalf("audit mode must NOT drop (CDN-churn hedge), got:\n%s", out)
	}
}

func TestRenderNftables_Deterministic(t *testing.T) {
	pol := EgressPolicy{AllowCIDRs: []string{"192.168.1.0/24", "10.0.0.0/8", "172.16.0.0/12"}}
	a, _ := RenderNftables("w1", pol, EgressEnforce)
	b, _ := RenderNftables("w1", pol, EgressEnforce)
	if a != b {
		t.Fatal("RenderNftables must be byte-identical for identical inputs")
	}
	// CIDRs sorted regardless of input order.
	i10 := strings.Index(a, "10.0.0.0/8")
	i172 := strings.Index(a, "172.16.0.0/12")
	i192 := strings.Index(a, "192.168.1.0/24")
	if !(i10 < i172 && i172 < i192) {
		t.Fatalf("CIDR lines not sorted: %d %d %d", i10, i172, i192)
	}
}

func TestRenderNftables_RejectsBadInput(t *testing.T) {
	if _, err := RenderNftables("Bad ID", EgressPolicy{}, EgressEnforce); err == nil {
		t.Fatal("bad sandbox id should error")
	}
	if _, err := RenderNftables("w1", EgressPolicy{AllowCIDRs: []string{"nope"}}, EgressEnforce); err == nil {
		t.Fatal("bad CIDR should error")
	}
}

func TestBwrapArgs_SecurityInvariants(t *testing.T) {
	m := validWorker()
	m.XDisplay = XDisplayNone
	a := BwrapArgs(&m, SpawnOpts{
		HostSocketPath: "/run/vibecraft/worker-23.sock",
		ExtraEnv:       map[string]string{"CUSTOMER_API_TOKEN": "sek"},
		Cmd:            []string{"/usr/bin/xterm", "-e", "claude"},
	})
	j := strings.Join(a, " ")

	mustHave := []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc",
		"--unshare-uts", "--unshare-cgroup",
		"--hostname worker-23",
		"--clearenv",             // sandbox never inherits daemon env
		"--ro-bind /etc /etc",    // base
		"--tmpfs /etc/vibecraft", // ...then secrets masked (real-kernel fix)
		"--proc /proc", "--dev /dev", "--tmpfs /tmp",
		"--bind /run/vibecraft/worker-23.sock /run/vibecraft.sock",
		"--setenv CUSTOMER_API_TOKEN sek",   // secret via env, never argv
		"--as-pid-1 -- /usr/bin/tini -s --", // tini PID1+subreaper fix
		"/usr/bin/xterm -e claude",
	}
	for _, s := range mustHave {
		if !strings.Contains(j, s) {
			t.Fatalf("BwrapArgs missing %q\nargv: %s", s, j)
		}
	}
	// /etc/vibecraft tmpfs MUST come after the /etc ro-bind or it won't mask.
	if strings.Index(j, "--ro-bind /etc /etc") > strings.Index(j, "--tmpfs /etc/vibecraft") {
		t.Fatal("/etc/vibecraft tmpfs must be applied AFTER the /etc ro-bind")
	}
	// No prepared netns → must isolate net.
	if !strings.Contains(j, "--unshare-net") {
		t.Fatal("no netns prepared: must --unshare-net")
	}
	// The plaintext secret must never appear as a bare argv token (only
	// as the value of --setenv).
	for i, tok := range a {
		if tok == "sek" && (i == 0 || a[i-1] != "CUSTOMER_API_TOKEN") {
			t.Fatalf("secret leaked into argv position %d", i)
		}
	}
}

func TestBwrapArgs_PreparedNetnsSkipsUnshareNet(t *testing.T) {
	m := validWorker()
	a := strings.Join(BwrapArgs(&m, SpawnOpts{NetnsName: "vc-worker-23", Cmd: []string{"/bin/true"}}), " ")
	if strings.Contains(a, "--unshare-net") {
		t.Fatal("with a prepared veth netns, bwrap must NOT --unshare-net (would discard the veth)")
	}
}

func TestRenderForwardNftables(t *testing.T) {
	out, err := RenderForwardNftables("worker-23", "vchworker23", "10.77.7.0/30",
		EgressPolicy{AllowCIDRs: []string{"1.1.1.1/32"}}, EgressEnforce)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, w := range []string{
		"table inet vc_fwd_worker_23 {",
		"type filter hook forward priority 0;",
		`iifname "vchworker23" jump sb_egress`,
		"ct state established,related accept",
		"ip daddr 1.1.1.1/32 accept",
		"ip daddr @allow_worker_23 accept",
		`log prefix "vc-egress-drop " drop`,
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in:\n%s", w, out)
		}
	}
	// audit mode must not drop
	a, _ := RenderForwardNftables("w1", "vchw1", "10.77.1.0/30", EgressPolicy{}, EgressAudit)
	if strings.Contains(a, " drop\n") || !strings.Contains(a, `log prefix "vc-egress-audit " accept`) {
		t.Fatalf("audit mode must log+accept, not drop:\n%s", a)
	}
	// bad inputs
	if _, e := RenderForwardNftables("bad id", "v", "10.0.0.0/8", EgressPolicy{}, EgressEnforce); e == nil {
		t.Fatal("bad id should error")
	}
	if _, e := RenderForwardNftables("w1", "bad iface!", "10.0.0.0/8", EgressPolicy{}, EgressEnforce); e == nil {
		t.Fatal("bad iface should error")
	}
}

func TestSocketPathAndIDValidation(t *testing.T) {
	if SocketPath("worker-23") != "/run/vibecraft/worker-23.sock" {
		t.Fatalf("SocketPath = %q", SocketPath("worker-23"))
	}
	if _, err := NewSandboxServer("Bad ID", nil); err == nil {
		t.Fatal("NewSandboxServer must reject an invalid id")
	}
}

func TestSandboxIDContextInjection(t *testing.T) {
	// SandboxID must read the id the server bound into the context,
	// never a client-supplied value.
	req := httptest.NewRequest("GET", "/whoami", nil)
	if SandboxID(req) != "" {
		t.Fatal("no context value -> empty id expected")
	}
	ctx := context.WithValue(req.Context(), sandboxCtxKey{}, "worker-9")
	if got := SandboxID(req.WithContext(ctx)); got != "worker-9" {
		t.Fatalf("SandboxID = %q, want worker-9", got)
	}
}

func TestAgentShellManifest(t *testing.T) {
	const home = "/home/vibecraft"
	m := AgentShellManifest(home,
		[]Mount{{HostPath: "/opt/tools", SandboxPath: "/opt/tools"}},
		EgressPolicy{AllowFQDNs: []string{"*.anthropic.com"}})
	if err := m.Validate(); err != nil {
		t.Fatalf("AgentShellManifest must be valid: %v", err)
	}
	if m.Type != TypeAgentShell || m.Lifetime != Persistent {
		t.Fatalf("agent-shell must be persistent AgentShell, got type=%v life=%v", m.Type, m.Lifetime)
	}
	// Home bound at the SAME path (identity map) so daemon-supplied
	// absolute paths resolve in and out of the sandbox.
	if m.Mounts[0].SandboxPath != home || m.Mounts[0].HostPath != home || m.HomeDir != home {
		t.Fatalf("home must be identity-mapped at %q, got mount=%+v homedir=%q", home, m.Mounts[0], m.HomeDir)
	}
	a := strings.Join(BwrapArgs(m, SpawnOpts{Cmd: []string{"/bin/bash"}}), " ")
	if strings.Contains(a, "--tmpfs "+home) {
		t.Fatalf("persistent agent-shell must NOT tmpfs the home:\n%s", a)
	}
	if !strings.Contains(a, "--bind "+home+" "+home) {
		t.Fatalf("agent-shell identity home bind missing:\n%s", a)
	}
}

func TestSystemManifest_LoadValidateMap(t *testing.T) {
	root := t.TempDir()

	// Missing manifest -> (nil,nil): discovery mode, not an error.
	if m, err := LoadSystemManifest(root, "invoice-bot"); m != nil || err != nil {
		t.Fatalf("missing manifest must be (nil,nil), got %v %v", m, err)
	}

	// Bad system name rejected.
	if _, err := LoadSystemManifest(root, "Bad Name!"); err == nil {
		t.Fatal("invalid system name must error")
	}

	// Write + load a valid manifest.
	dir := filepath.Join(root, "invoice-bot")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"allow_fqdns":["api.stripe.com","*.mailgun.org"],"allow_cidrs":["10.0.0.0/8"],
	          "enforce_egress":true,"vault_refs":["STRIPE_KEY","MAILGUN_KEY"],
	          "mounts":[{"HostPath":"/opt/shared","SandboxPath":"/opt/shared","ReadOnly":true}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadSystemManifest(root, "invoice-bot")
	if err != nil || m == nil {
		t.Fatalf("valid manifest load failed: %v", err)
	}
	if m.Mode() != EgressEnforce {
		t.Fatalf("enforce_egress true -> EgressEnforce, got %v", m.Mode())
	}

	// Per-system credential scope: only declared refs.
	sm := m.ToSandboxManifest("invoice-bot", dir, map[string]string{"STRIPE_KEY": "sk", "MAILGUN_KEY": "mg"})
	if err := sm.Validate(); err != nil {
		t.Fatalf("derived system sandbox invalid: %v", err)
	}
	if sm.Type != TypeSystem || sm.HomeDir != dir || sm.Mounts[0].SandboxPath != dir {
		t.Fatalf("system sandbox must be TypeSystem, identity-home %q: %+v", dir, sm)
	}
	a := strings.Join(BwrapArgs(sm, SpawnOpts{Cmd: []string{"/bin/sh"}}), " ")
	if !strings.Contains(a, "--bind "+dir+" "+dir) || strings.Contains(a, "--tmpfs "+dir) {
		t.Fatalf("system home must be identity-bound, not tmpfs:\n%s", a)
	}

	// Rejections.
	for name, js := range map[string]string{
		"bad fqdn":  `{"allow_fqdns":["not a host"]}`,
		"bad cidr":  `{"allow_cidrs":["999/8"]}`,
		"bad ref":   `{"vault_refs":["lower-case"]}`,
		"forbidden": `{"mounts":[{"HostPath":"/etc/vibecraft","SandboxPath":"/x"}]}`,
	} {
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(js), 0644)
		if _, err := LoadSystemManifest(root, "invoice-bot"); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
}

func TestAppManifest_ValidateMap(t *testing.T) {
	good := &AppManifest{Port: 3000, StartCmd: []string{"node", "server.js"}, RestartPolicy: "always"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid app manifest rejected: %v", err)
	}
	sm := good.ToSandboxManifest("blog", "/home/vibecraft/apps/blog", nil)
	if sm.Type != TypeCustomerApp || sm.HomeDir != "/home/vibecraft/apps/blog" || sm.XDisplay != XDisplayNone {
		t.Fatalf("app sandbox shape wrong: %+v", sm)
	}
	if err := sm.Validate(); err != nil {
		t.Fatalf("derived app sandbox invalid: %v", err)
	}
	for name, m := range map[string]*AppManifest{
		"no port":   {StartCmd: []string{"x"}},
		"port 8420": {Port: 8420, StartCmd: []string{"x"}},
		"no cmd":    {Port: 3000},
		"bad pol":   {Port: 3000, StartCmd: []string{"x"}, RestartPolicy: "weird"},
		"bad fqdn":  {Port: 3000, StartCmd: []string{"x"}, AllowFQDNs: []string{"not host"}},
	} {
		if err := m.Validate(); err == nil {
			t.Fatalf("%s should be rejected", name)
		}
	}
}

func TestBwrapArgs_Deterministic(t *testing.T) {
	m := validWorker()
	o := SpawnOpts{ExtraEnv: map[string]string{"B": "2", "A": "1"}, Cmd: []string{"/bin/sh"}}
	if strings.Join(BwrapArgs(&m, o), " ") != strings.Join(BwrapArgs(&m, o), " ") {
		t.Fatal("BwrapArgs must be deterministic")
	}
}
