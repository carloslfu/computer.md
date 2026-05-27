// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// network_linux.go is the privileged half of the egress story: it
// creates a per-sandbox network namespace with a veth pair into the
// host, NATs it out, and installs the host-side forward-chain nftables
// allowlist (RenderForwardNftables). Enforcement is HOST-side and
// bypass-proof — the sandbox has no CAP_NET_ADMIN over the host's
// nftables and every packet it sends crosses the host veth.
//
// All of this needs CAP_NET_ADMIN; the daemon runs as root. Validated
// on a real EC2 kernel by the integration tests.

// NetConfig is the result of SetupNetwork — what SpawnCmd needs to put
// the sandbox into the prepared netns.
type NetConfig struct {
	NetnsName   string
	VethHost    string
	SandboxCIDR string // /30, e.g. 10.77.7.0/30
	HostIP      string // 10.77.7.1
	SandboxIP   string // 10.77.7.2
	ResolvConf  string // per-sandbox /etc/resolv.conf to bind in
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

// defaultUplink returns the host's default-route egress interface.
func defaultUplink() (string, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	for i, t := range f {
		if t == "dev" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return "", fmt.Errorf("no default route")
}

// SetupNetwork builds the netns + veth + NAT + nftables allowlist for a
// sandbox. idx (1..63) selects a /30 inside 10.77.0.0/16 so concurrent
// sandboxes don't collide. Idempotent-ish: call TeardownNetwork first if
// re-creating.
func SetupNetwork(id string, idx int, pol EgressPolicy, mode EgressMode) (*NetConfig, error) {
	if !sandboxIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid sandbox id %q", id)
	}
	if idx < 1 || idx > 63 {
		return nil, fmt.Errorf("idx %d out of range 1..63", idx)
	}
	ns := "vcns" + fmt.Sprint(idx)
	vh := VethHostName(id)
	vs := VethSandboxName(id)
	base := fmt.Sprintf("10.77.%d", idx)
	cfg := &NetConfig{
		NetnsName:   ns,
		VethHost:    vh,
		SandboxCIDR: base + ".0/30",
		HostIP:      base + ".1",
		SandboxIP:   base + ".2",
		// ResolvConf intentionally left empty: see the comment on
		// sandboxResolver. In-sandbox DNS is the daemon-DNS-proxy
		// workstream, not a naive bind (the host /etc/resolv.conf is a
		// systemd-resolved symlink that bwrap can't overlay cleanly, and
		// the architecturally-correct resolver is the daemon proxy on
		// the bridge IP — a real-kernel finding, recorded in the plan).
	}
	uplink, err := defaultUplink()
	if err != nil {
		return nil, fmt.Errorf("detect uplink: %w", err)
	}

	steps := [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "link", "add", vh, "type", "veth", "peer", "name", vs},
		{"ip", "link", "set", vs, "netns", ns},
		{"ip", "addr", "add", cfg.HostIP + "/30", "dev", vh},
		{"ip", "link", "set", vh, "up"},
		{"ip", "-n", ns, "addr", "add", cfg.SandboxIP + "/30", "dev", vs},
		{"ip", "-n", ns, "link", "set", vs, "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "add", "default", "via", cfg.HostIP},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if out, err := run(s[0], s[1:]...); err != nil {
			TeardownNetwork(id, idx) // best-effort rollback
			return nil, fmt.Errorf("netsetup: %w (%s)", err, out)
		}
	}

	fwd, err := RenderForwardNftables(id, vh, cfg.SandboxCIDR, pol, mode)
	if err != nil {
		TeardownNetwork(id, idx)
		return nil, err
	}
	nat := fmt.Sprintf(`table ip vc_nat_%s {
  chain post {
    type nat hook postrouting priority 100; policy accept;
    ip saddr %s oifname "%s" masquerade
  }
}
`, strings.ReplaceAll(id, "-", "_"), cfg.SandboxCIDR, uplink)

	if out, err := nftApply(fwd + nat); err != nil {
		TeardownNetwork(id, idx)
		return nil, fmt.Errorf("nft apply: %w (%s)", err, out)
	}
	if err := ensureIptablesSandboxRules(vh, cfg.SandboxCIDR); err != nil {
		TeardownNetwork(id, idx)
		return nil, fmt.Errorf("iptables sandbox rules: %w", err)
	}
	return cfg, nil
}

func nftApply(ruleset string) (string, error) {
	c := exec.Command("nft", "-f", "-")
	c.Stdin = strings.NewReader(ruleset)
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// PopulateAllowSet adds resolved IPs to the sandbox's runtime allow set
// (the DNS-proxy hook). ips are bare IPv4 addresses.
func PopulateAllowSet(id string, ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	table := "vc_fwd_" + strings.ReplaceAll(id, "-", "_")
	set := NftSetName(id)
	elems := strings.Join(ips, ", ")
	_, err := run("nft", "add", "element", "inet", table, set, "{ "+elems+" }")
	return err
}

// TeardownNetwork removes everything SetupNetwork created. Best-effort:
// every step's error is ignored so a partial setup still fully cleans.
func TeardownNetwork(id string, idx int) {
	ns := "vcns" + fmt.Sprint(idx)
	vh := VethHostName(id)
	base := fmt.Sprintf("10.77.%d", idx)
	removeIptablesSandboxRules(vh, base+".0/30")
	exec.Command("nft", "delete", "table", "inet", "vc_fwd_"+strings.ReplaceAll(id, "-", "_")).Run()
	exec.Command("nft", "delete", "table", "ip", "vc_nat_"+strings.ReplaceAll(id, "-", "_")).Run()
	exec.Command("ip", "netns", "del", ns).Run()
	exec.Command("ip", "link", "del", vh).Run()
	os.Remove("/run/vc-resolv-" + id + ".conf")
}

type iptablesRule struct {
	table string
	chain string
	args  []string
}

func ensureIptablesSandboxRules(vethHost, sandboxCIDR string) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	for _, rule := range iptablesSandboxRules(vethHost, sandboxCIDR) {
		if err := iptablesEnsure(rule); err != nil {
			return err
		}
	}
	return nil
}

func iptablesEnsure(rule iptablesRule) error {
	check := iptablesCommandArgs(rule, "-C")
	if err := exec.Command("iptables", check...).Run(); err == nil {
		return nil
	}
	insert := iptablesCommandArgs(rule, "-I", "1")
	out, err := exec.Command("iptables", insert...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(insert, " "), err, string(out))
	}
	return nil
}

func removeIptablesSandboxRules(vethHost, sandboxCIDR string) {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	for _, rule := range iptablesSandboxRules(vethHost, sandboxCIDR) {
		deleteArgs := iptablesCommandArgs(rule, "-D")
		for exec.Command("iptables", deleteArgs...).Run() == nil {
		}
	}
}

func iptablesCommandArgs(rule iptablesRule, op string, insertPosition ...string) []string {
	args := []string{"-w", "5"}
	if rule.table != "" && rule.table != "filter" {
		args = append(args, "-t", rule.table)
	}
	args = append(args, op, rule.chain)
	args = append(args, insertPosition...)
	args = append(args, rule.args...)
	return args
}

func iptablesSandboxRules(vethHost, sandboxCIDR string) []iptablesRule {
	return []iptablesRule{
		{table: "filter", chain: "INPUT", args: []string{"-i", vethHost, "-p", "udp", "--dport", "53", "-j", "ACCEPT"}},
		{table: "filter", chain: "INPUT", args: []string{"-i", vethHost, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"}},
		{table: "filter", chain: "FORWARD", args: []string{"-i", vethHost, "-j", "ACCEPT"}},
		{table: "filter", chain: "FORWARD", args: []string{"-o", vethHost, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{table: "nat", chain: "POSTROUTING", args: []string{"-s", sandboxCIDR, "-j", "MASQUERADE"}},
	}
}
