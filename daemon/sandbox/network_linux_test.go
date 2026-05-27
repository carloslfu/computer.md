// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"reflect"
	"testing"
)

func TestIptablesSandboxRulesAllowDNSAndForwarding(t *testing.T) {
	got := iptablesSandboxRules("vchagent", "10.77.1.0/30")
	want := []iptablesRule{
		{table: "filter", chain: "INPUT", args: []string{"-i", "vchagent", "-p", "udp", "--dport", "53", "-j", "ACCEPT"}},
		{table: "filter", chain: "INPUT", args: []string{"-i", "vchagent", "-p", "tcp", "--dport", "53", "-j", "ACCEPT"}},
		{table: "filter", chain: "FORWARD", args: []string{"-i", "vchagent", "-j", "ACCEPT"}},
		{table: "filter", chain: "FORWARD", args: []string{"-o", "vchagent", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},
		{table: "nat", chain: "POSTROUTING", args: []string{"-s", "10.77.1.0/30", "-j", "MASQUERADE"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("iptablesSandboxRules mismatch\nwant %#v\n got %#v", want, got)
	}
}

func TestIptablesCommandArgs(t *testing.T) {
	rule := iptablesRule{table: "nat", chain: "POSTROUTING", args: []string{"-s", "10.77.1.0/30", "-j", "MASQUERADE"}}
	got := iptablesCommandArgs(rule, "-I", "1")
	want := []string{"-w", "5", "-t", "nat", "-I", "POSTROUTING", "1", "-s", "10.77.1.0/30", "-j", "MASQUERADE"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("iptablesCommandArgs mismatch\nwant %#v\n got %#v", want, got)
	}
}
