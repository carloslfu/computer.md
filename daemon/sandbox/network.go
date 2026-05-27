// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file is the PURE-LOGIC half of the Phase 1 network story:
// deterministic name derivation + nftables ruleset *rendering*. It does
// NOT touch the kernel. Actually creating the veth pair, moving it into
// a netns, classifying the cgroup, and `nft -f`-applying the rendered
// ruleset is the privileged Linux-only layer (network_linux.go +
// integration_test.go on a privileged CI runner), deliberately not in
// this package yet — see the package doc in manifest.go and the plan's
// Decision D5 (bwrap vs nspawn vs podman is still Pending; the argv
// builder is intentionally NOT written until D5 is confirmed against a
// real bubblewrap on Linux, to avoid baking in unvalidated flag
// semantics this OS can't test).

// EgressMode selects audit-mode (log, do not block — Phase 3 default,
// hedges CDN IP churn) vs enforce-mode (default-deny).
type EgressMode int

const (
	EgressAudit EgressMode = iota
	EgressEnforce
)

// CgroupName is the per-sandbox cgroup leaf the nftables `socket
// cgroupv2` / `meta cgroup` match keys on. Derived purely from the
// (already-validated) sandbox ID so the renderer and the privileged
// applier agree without sharing state.
func CgroupName(sandboxID string) string {
	return "vibecraft.sandbox." + sandboxID
}

// VethHostName / VethSandboxName are the two ends of the veth pair.
// Linux caps interface names at 15 bytes, so the sandbox ID (≤63 by
// manifest rule) is truncated with a stable prefix. Both sides are
// derived the same way so host teardown can find the peer by name.
func VethHostName(sandboxID string) string  { return ifname("vch", sandboxID) }
func VethSandboxName(sandboxID string) string { return ifname("vcs", sandboxID) }

func ifname(prefix, id string) string {
	name := prefix + strings.ReplaceAll(id, "-", "")
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

// NftSetName is the named set holding this sandbox's allowed dst IPs
// (the daemon DNS proxy keeps it fresh with short-TTL entries).
func NftSetName(sandboxID string) string {
	return "allow_" + strings.ReplaceAll(sandboxID, "-", "_")
}

// RenderForwardNftables renders the HOST-side ruleset enforced when the
// sandbox lives in its own veth network namespace (the architecture the
// plan + the proven bwrap path actually use). Enforcing on the host's
// `forward` hook keyed by the sandbox's veth interface is bypass-proof:
// the sandbox has no CAP_NET_ADMIN over the host's nftables, and ALL of
// its egress traverses the host end of the veth. (The cgroup-socket
// variant below, RenderNftables, is the shared-host-netns model — kept
// for the agent-shell case that may not get its own netns.)
//
// vethHost is the host-side veth iface (VethHostName(id)); sandboxCIDR
// is the sandbox's /30 source subnet. staticIPs are the always-allowed
// dst CIDRs; the named set is populated at runtime by the DNS proxy.
// Audit mode logs the would-be-deny then accepts (CDN-churn hedge);
// enforce mode logs then drops.
func RenderForwardNftables(sandboxID, vethHost, sandboxCIDR string, pol EgressPolicy, mode EgressMode) (string, error) {
	if !sandboxIDPattern.MatchString(sandboxID) {
		return "", fmt.Errorf("invalid sandbox id %q", sandboxID)
	}
	if !ifnamePattern.MatchString(vethHost) {
		return "", fmt.Errorf("invalid veth iface %q", vethHost)
	}
	if !cidrPattern.MatchString(sandboxCIDR) {
		return "", fmt.Errorf("invalid sandbox CIDR %q", sandboxCIDR)
	}
	for _, c := range pol.AllowCIDRs {
		if !cidrPattern.MatchString(c) {
			return "", fmt.Errorf("invalid CIDR %q", c)
		}
	}
	table := "vc_fwd_" + strings.ReplaceAll(sandboxID, "-", "_")
	set := NftSetName(sandboxID)
	static := append([]string(nil), pol.AllowCIDRs...)
	sort.Strings(static)

	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", table)
	// Plain ipv4_addr set: the DNS proxy populates it with individual
	// resolved A-record /32s, not ranges. (An `interval` set would
	// reject bare addresses on this nft version — a real-kernel finding.
	// Static CIDR allows are emitted as explicit `ip daddr` lines, not
	// via this set, so no interval support is needed here.)
	fmt.Fprintf(&b, "  set %s { type ipv4_addr; }\n", set)
	b.WriteString("  chain forward {\n")
	b.WriteString("    type filter hook forward priority 0; policy accept;\n")
	// Only this sandbox's veth is governed; nothing else on the host is.
	fmt.Fprintf(&b, "    iifname %q jump sb_egress\n", vethHost)
	b.WriteString("  }\n")
	b.WriteString("  chain sb_egress {\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    udp dport 53 accept\n")
	b.WriteString("    tcp dport 53 accept\n")
	for _, c := range static {
		fmt.Fprintf(&b, "    ip daddr %s accept\n", c)
	}
	fmt.Fprintf(&b, "    ip daddr @%s accept\n", set)
	if mode == EgressEnforce {
		b.WriteString("    log prefix \"vc-egress-drop \" drop\n")
	} else {
		b.WriteString("    log prefix \"vc-egress-audit \" accept\n")
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

var ifnamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,14}$`)

// RenderNftables renders the per-sandbox `table inet` ruleset as the
// text the privileged layer will feed to `nft -f -`. Pure and
// deterministic: same inputs → byte-identical output, so it is fully
// unit-testable on any OS and auditable in review.
//
// staticIPs are CIDRs/addresses known up front (EgressPolicy.AllowCIDRs
// + any pre-resolved FQDN IPs). The named set NftSetName(id) is declared
// (empty here) for the DNS proxy to populate at runtime.
//
// Audit mode renders the same allowlist but replaces the terminal
// `drop` with `log prefix "vc-egress-deny " ` followed by `accept`, so
// denied traffic is recorded but not broken (the Phase 3 default).
func RenderNftables(sandboxID string, pol EgressPolicy, mode EgressMode) (string, error) {
	if !sandboxIDPattern.MatchString(sandboxID) {
		return "", fmt.Errorf("invalid sandbox id %q for nftables render", sandboxID)
	}
	for _, c := range pol.AllowCIDRs {
		if !cidrPattern.MatchString(c) {
			return "", fmt.Errorf("invalid CIDR %q", c)
		}
	}

	table := "vc_sb_" + strings.ReplaceAll(sandboxID, "-", "_")
	set := NftSetName(sandboxID)
	cg := CgroupName(sandboxID)

	staticIPs := append([]string(nil), pol.AllowCIDRs...)
	sort.Strings(staticIPs) // deterministic output

	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", table)
	fmt.Fprintf(&b, "  set %s {\n", set)
	b.WriteString("    type ipv4_addr\n")
	b.WriteString("    # populated at runtime by the daemon DNS proxy (short TTL)\n")
	b.WriteString("  }\n")
	b.WriteString("  chain output {\n")
	b.WriteString("    type filter hook output priority 0; policy accept;\n")
	b.WriteString("    ct state established,related accept\n")
	// Only this sandbox's cgroup is filtered; everything else on the host
	// is untouched by this table.
	fmt.Fprintf(&b, "    socket cgroupv2 level 2 %q jump sandbox_egress\n", cg)
	b.WriteString("  }\n")
	b.WriteString("  chain sandbox_egress {\n")
	// Always-allow loopback + DNS to the daemon resolver (the proxy needs
	// to answer before any allowlisted FQDN can resolve).
	b.WriteString("    oifname \"lo\" accept\n")
	b.WriteString("    udp dport 53 accept\n")
	b.WriteString("    tcp dport 53 accept\n")
	for _, c := range staticIPs {
		fmt.Fprintf(&b, "    ip daddr %s accept\n", c)
	}
	fmt.Fprintf(&b, "    ip daddr @%s accept\n", set)
	if mode == EgressEnforce {
		b.WriteString("    log prefix \"vc-egress-drop \" drop\n")
	} else {
		// Audit: record the would-be-denied flow, then let it through so a
		// CDN IP rotation can't silently break a customer system.
		b.WriteString("    log prefix \"vc-egress-audit \" accept\n")
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}
