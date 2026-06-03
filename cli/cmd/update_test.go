// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

func TestCompareReleaseVersions(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want int
		ok   bool
	}{
		{name: "newer", a: "v1.2.4", b: "v1.2.3", want: 1, ok: true},
		{name: "older", a: "v1.2.2", b: "v1.2.3", want: -1, ok: true},
		{name: "equal", a: "1.2.3", b: "v1.2.3", want: 0, ok: true},
		{name: "prerelease same core", a: "v1.2.3-rc.1", b: "v1.2.3", want: 0, ok: true},
		{name: "unknown dev", a: "v1.2.3", b: "dev", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := compareReleaseVersions(tt.a, tt.b)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("compareReleaseVersions(%q,%q) = (%d,%v), want (%d,%v)", tt.a, tt.b, got, ok, tt.want, tt.ok)
			}
		})
	}
}
