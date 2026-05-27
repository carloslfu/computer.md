// SPDX-License-Identifier: Apache-2.0

package sandbox

// SystemProposalHook, if set by package main, is called when a
// discovery-mode (unmanifested) system's observation window closes. It
// receives the system name and the out-of-policy FQDNs the system
// reached. main wires this to: write ~/systems/<name>/manifest.proposed
// .json, push a customer notification, and audit-log it. Kept as a hook
// (and cross-platform so the daemon builds on the macOS dev host) so the
// sandbox package stays free of the audit/notify deps.
var SystemProposalHook func(system string, observed []string)
