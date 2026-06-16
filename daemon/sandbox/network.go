// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"sort"
	"strings"
)

// sandboxMeshCIDR is the /16 carved up into per-sandbox /30s
// (SetupNetwork: idx selects 10.77.<idx>.0/30). The DNS proxy must never
// fold an address from this range into the allow set, AND the rendered
// rulesets must never let an operator AllowCIDRs entry that overlaps it
// (e.g. 10.0.0.0/8) accept into it — either would let a sandbox reach a
// peer sandbox's gateway/host IP and smuggle lateral access past
// enforce-mode default-deny.
const sandboxMeshCIDR = "10.77.0.0/16"

var sandboxIPRange = func() *net.IPNet {
	_, n, _ := net.ParseCIDR(sandboxMeshCIDR)
	return n
}()

// cgnatRange is RFC6598 carrier-grade NAT space (100.64.0.0/10). Go's
// net.IP.IsPrivate covers only RFC1918 + RFC4193, NOT RFC6598, yet CGNAT
// is internal/host-routable on some cloud and carrier networks, so the
// allow-set guard rejects it explicitly.
var cgnatRange = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// allowSetIPAllowed reports whether a resolved address is safe to add to
// a sandbox's runtime nft allow set. It is the SSRF / DNS-rebinding guard
// on the DNS-proxy hot path: allowlisting a NAME must not implicitly
// allowlist whatever IP that name happens to resolve to at request time,
// so any address that targets the host, the cloud instance-metadata
// endpoint (IMDS, 169.254.169.254), an RFC1918/CGNAT internal service,
// or the sandbox /30 mesh is rejected here regardless of the FQDN match.
//
// The one escape hatch: an address the operator EXPLICITLY allowlisted as
// an exact static CIDR via EgressPolicy.AllowCIDRs is permitted, because
// that is a deliberate, reviewed choice (and such CIDRs are emitted as
// their own `ip daddr <cidr> accept` lines anyway). ipStr is a bare IPv4
// literal as produced by net.IP.String().
//
// The escape hatch is NOT unconditional. The truly dangerous ranges — the
// host/link-local space (loopback, unspecified, link-local, multicast; this
// covers the cloud IMDS endpoint 169.254.169.254) and the sandbox /30 mesh
// (10.77.0.0/16) — are HARD-denied first and can never be overridden by a
// static CIDR, no matter how broad. Otherwise a broad operator entry
// (0.0.0.0/0 re-opening the SSRF/IMDS hole, or a 10.0.0.0/8 that overlaps
// the mesh and grants cross-sandbox lateral access) would defeat the guard.
// Operator CIDRs may still reach OTHER internal/RFC1918/CGNAT addresses they
// deliberately listed, since those are not in the hard-deny set.
func allowSetIPAllowed(ipStr string, pol EgressPolicy) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// Hard deny: never overridable by AllowCIDRs (fail closed).
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || sandboxIPRange.Contains(ip) {
		return false
	}
	// Operator-declared static CIDRs are the deliberate escape hatch for the
	// remaining internal ranges (RFC1918 + CGNAT) below.
	if staticCIDRAllows(ip, pol) {
		return true
	}
	if ip.IsPrivate() || cgnatRange.Contains(ip) {
		return false
	}
	return true
}

// staticCIDRAllows reports whether ip falls inside a CIDR the operator
// explicitly listed in EgressPolicy.AllowCIDRs. Malformed CIDRs are
// skipped (RenderForwardNftables already rejects the ruleset on those).
func staticCIDRAllows(ip net.IP, pol EgressPolicy) bool {
	for _, c := range pol.AllowCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// filterAllowSetIPs keeps only the addresses safe to push to the nft
// allow set (see allowSetIPAllowed). Order is preserved; the caller dedups.
func filterAllowSetIPs(ips []string, pol EgressPolicy) []string {
	out := ips[:0:0]
	for _, ip := range ips {
		if allowSetIPAllowed(ip, pol) {
			out = append(out, ip)
		}
	}
	return out
}

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
func VethHostName(sandboxID string) string    { return ifname("vch", sandboxID) }
func VethSandboxName(sandboxID string) string { return ifname("vcs", sandboxID) }

func ifname(prefix, id string) string {
	name := prefix + strings.ReplaceAll(id, "-", "")
	if len(name) > 15 {
		compact := strings.ReplaceAll(id, "-", "")
		h := fnv.New32a()
		_, _ = h.Write([]byte(id))
		suffix := fmt.Sprintf("%06x", h.Sum32()&0xffffff)
		keep := 15 - len(prefix) - 1 - len(suffix)
		if keep < 1 {
			keep = 1
		}
		if len(compact) > keep {
			compact = compact[:keep]
		}
		name = prefix + compact + "_" + suffix
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
	// Hard-deny the sandbox /30 mesh BEFORE any static accept, so a broad
	// operator AllowCIDRs entry overlapping it (e.g. 10.0.0.0/8) can't open
	// cross-sandbox lateral access. nft evaluates top-down; this wins for
	// mesh-destined packets regardless of the accepts below. Enforce-only:
	// audit mode never drops (CDN-churn hedge) and its terminal rule accepts
	// everything anyway, so a mesh drop here would break the hedge.
	if mode == EgressEnforce {
		fmt.Fprintf(&b, "    ip daddr %s drop\n", sandboxMeshCIDR)
	}
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
	// Always allow loopback. DNS proxy traffic stays on loopback in the
	// shared-host-netns model; external DNS on arbitrary port 53 does not
	// get a blanket bypass and must match the allowlist like anything else.
	b.WriteString("    oifname \"lo\" accept\n")
	// Hard-deny the sandbox /30 mesh BEFORE any static accept, so a broad
	// operator AllowCIDRs entry overlapping it (e.g. 10.0.0.0/8) can't open
	// cross-sandbox lateral access. nft evaluates top-down; this wins for
	// mesh-destined packets regardless of the accepts below. Enforce-only:
	// audit mode never drops (CDN-churn hedge) and its terminal rule accepts
	// everything anyway, so a mesh drop here would break the hedge.
	if mode == EgressEnforce {
		fmt.Fprintf(&b, "    ip daddr %s drop\n", sandboxMeshCIDR)
	}
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
