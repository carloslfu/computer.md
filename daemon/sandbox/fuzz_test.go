// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import "testing"

// The per-sandbox DNS proxy hand-rolls DNS wire-format parsing on bytes
// a COMPROMISED sandbox fully controls (it points resolv.conf at the
// gateway and can send anything to :53). parseQuestion + buildResponse
// must never panic or run unboundedly on hostile input. Phase 6
// hardening: gate releases on no fuzz crashes.
//
//	go test -tags integration -run x -fuzz FuzzParseDNSQuestion -fuzztime 60s ./sandbox/
func FuzzParseDNSQuestion(f *testing.F) {
	// Seeds: a well-formed A query, truncated, label-length lies,
	// compression-pointer in the question (rejected), empty.
	f.Add([]byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0,
		3, 'a', 'b', 'c', 0, 0, 1, 0, 1})
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xff})
	f.Add([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C})
	f.Add(make([]byte, 1500))

	f.Fuzz(func(t *testing.T, msg []byte) {
		name, qtype, ok := parseQuestion(msg)
		if !ok {
			return
		}
		if len(name) > 4096 {
			t.Fatalf("parseQuestion returned an absurd name len %d", len(name))
		}
		// buildResponse must also survive whatever name parseQuestion
		// accepted, for both an empty answer and the refused path.
		_ = buildResponse(msg, name, qtype, nil, rcodeRefused)
		_ = buildResponse(msg, name, qtype, nil, rcodeNoError)
		// allowedFQDN must never panic on a parsed name.
		_ = allowedFQDN(name, EgressPolicy{AllowFQDNs: []string{"*.x.test", "a.test"}})
	})
}
