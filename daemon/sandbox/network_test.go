// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"reflect"
	"testing"
)

// TestAllowSetIPAllowed_FiltersInternalAndMetadata pins the SSRF /
// DNS-rebinding guard: a resolved address that targets the cloud IMDS
// endpoint, the host loopback, an RFC1918/link-local internal service,
// or the sandbox /30 mesh must NEVER be eligible for the runtime nft
// allow set, while ordinary public addresses must be. Before the fix the
// DNS proxy pushed every resolved IP into the set unconditionally, so an
// allowlisted FQDN that resolved (by attacker DNS / rebinding / CNAME /
// misconfig) to 169.254.169.254 handed a compromised enforce-mode
// workload the box's IAM-role credentials.
func TestAllowSetIPAllowed_FiltersInternalAndMetadata(t *testing.T) {
	empty := EgressPolicy{}
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		{"169.254.169.254", false, "cloud IMDS endpoint (IAM credential theft)"},
		{"169.254.0.1", false, "link-local"},
		{"127.0.0.1", false, "loopback"},
		{"0.0.0.0", false, "unspecified"},
		{"10.0.0.5", false, "RFC1918 (10/8)"},
		{"172.16.4.4", false, "RFC1918 (172.16/12)"},
		{"192.168.1.1", false, "RFC1918 (192.168/16)"},
		{"10.77.7.1", false, "sandbox /30 gateway (lateral mesh)"},
		{"10.77.255.255", false, "sandbox /16 mesh"},
		{"100.64.0.1", false, "CGNAT (RFC6598, treated as private by net.IP.IsPrivate)"},
		{"224.0.0.1", false, "multicast"},
		{"1.1.1.1", true, "public resolver"},
		{"8.8.8.8", true, "public resolver"},
		{"93.184.216.34", true, "public host (example.com)"},
		{"not-an-ip", false, "unparseable"},
	}
	for _, c := range cases {
		if got := allowSetIPAllowed(c.ip, empty); got != c.want {
			t.Errorf("allowSetIPAllowed(%q) = %v, want %v (%s)", c.ip, got, c.want, c.why)
		}
	}
}

// TestAllowSetIPAllowed_StaticCIDREscapeHatch verifies the one
// intentional exception: an internal address the operator EXPLICITLY
// listed as a static AllowCIDRs entry is permitted (a deliberate,
// reviewed choice), but an internal address NOT covered by any
// AllowCIDRs stays blocked.
func TestAllowSetIPAllowed_StaticCIDREscapeHatch(t *testing.T) {
	pol := EgressPolicy{AllowCIDRs: []string{"10.50.0.0/16"}}
	if !allowSetIPAllowed("10.50.0.9", pol) {
		t.Errorf("operator-allowlisted internal CIDR member 10.50.0.9 must be allowed")
	}
	// An internal address outside the declared CIDR is still rejected.
	if allowSetIPAllowed("10.99.0.9", pol) {
		t.Errorf("internal IP outside declared AllowCIDRs must stay blocked")
	}
	// The IMDS endpoint is never reachable just because some unrelated
	// internal CIDR was declared.
	if allowSetIPAllowed("169.254.169.254", pol) {
		t.Errorf("IMDS endpoint must stay blocked even with an unrelated AllowCIDRs entry")
	}
}

// TestFilterAllowSetIPs_DropsInternalKeepsPublic exercises the slice
// filter the static pre-population path and the live DNS hook both feed
// through: a mixed answer keeps only the public addresses.
func TestFilterAllowSetIPs_DropsInternalKeepsPublic(t *testing.T) {
	in := []string{"1.1.1.1", "169.254.169.254", "10.0.0.1", "8.8.8.8", "127.0.0.1"}
	got := filterAllowSetIPs(in, EgressPolicy{})
	want := []string{"1.1.1.1", "8.8.8.8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterAllowSetIPs mismatch\nwant %#v\n got %#v", want, got)
	}
	// A name resolving ONLY to filtered addresses yields an empty set, so
	// a rebinding answer can't smuggle an internal target into the set.
	onlyInternal := filterAllowSetIPs([]string{"169.254.169.254", "10.0.0.1"}, EgressPolicy{})
	if len(onlyInternal) != 0 {
		t.Fatalf("all-internal answer must filter to empty, got %#v", onlyInternal)
	}
}
