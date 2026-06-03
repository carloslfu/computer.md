// SPDX-License-Identifier: Apache-2.0

package vault

import "testing"

// TestResolveReferencesOverlappingNames pins the boundary-safe behavior of
// Store.ResolveReferences for overlapping secret names (A vs ABC).
//
// The old implementation walked the secrets map and did a raw
// strings.ReplaceAll for each "$"+name. That is order-dependent: Go map
// iteration order is randomized, so for input "$ABC" the loop might hit
// secret A first and rewrite the "$A" prefix, yielding "<valueA>BC" and
// never matching "$ABC" at all. The token "$ABC" would resolve to the
// wrong value on some runs and the right value on others — a flaky,
// corrupting bug.
//
// Routing through the regex-backed Resolver fixes this: it matches whole
// $NAME / ${NAME} tokens with a longest-name boundary, so "$ABC" always
// resolves to ABC's value and the "$A" inside it is never treated as a
// separate reference.
func TestResolveReferencesOverlappingNames(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("A", "valueA", ""); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := s.Set("ABC", "valueABC", ""); err != nil {
		t.Fatalf("Set ABC: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "longer overlapping name resolves to its own value",
			in:   "$ABC",
			want: "valueABC",
		},
		{
			name: "shorter name resolves independently",
			in:   "$A",
			want: "valueA",
		},
		{
			name: "brace form of the longer name",
			in:   "${ABC}",
			want: "valueABC",
		},
		{
			name: "both names in one string, longest-token wins each",
			in:   "x=$A y=$ABC",
			want: "x=valueA y=valueABC",
		},
		{
			name: "longer name is not corrupted by the shorter prefix",
			// The whole point: this must NOT become "valueABC" via a
			// "$A" prefix match, and must NOT become "valueABC" with a
			// trailing "BC" left dangling either.
			in:   "prefix-$ABC-suffix",
			want: "prefix-valueABC-suffix",
		},
		{
			name: "unknown overlapping token left untouched",
			// "$AB" is not a secret; only "$A" and "$ABC" are. The regex
			// matches the full "$AB" token, finds no secret named AB, and
			// leaves it verbatim — it does NOT fall back to expanding the
			// "$A" prefix.
			in:   "$AB",
			want: "$AB",
		},
	}

	// Run each case many times: the original bug was nondeterministic
	// (driven by map iteration order), so a single pass could pass by luck.
	// Repeating makes a regression overwhelmingly likely to be caught.
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 200; i++ {
				if got := s.ResolveReferences(tc.in); got != tc.want {
					t.Fatalf("ResolveReferences(%q) = %q, want %q (iteration %d)", tc.in, got, tc.want, i)
				}
			}
		})
	}
}
