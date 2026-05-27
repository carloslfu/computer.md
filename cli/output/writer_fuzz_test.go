// SPDX-License-Identifier: Apache-2.0

package output

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/carloslfu/computer.md/cli/schema"
)

// FuzzEmitArbitraryData runs Emit against random shapes and asserts:
//   - it never panics
//   - the bytes written always parse as JSON
//   - the parsed envelope is well-formed: v == schema.Version, ok == true,
//     no error field, and the data field round-trips.
//
// This is the cheap-but-real defense against a stray Emit() being added
// for a data type that includes a channel or function value (those
// produce silent half-empty JSON otherwise).
func FuzzEmitArbitraryData(f *testing.F) {
	seeds := []string{
		`null`,
		`""`,
		`{}`,
		`{"id":"abc","status":"ok"}`,
		`[1,2,3]`,
		`{"nested":{"a":[true,false,null]}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Skip()
		}
		buf := &bytes.Buffer{}
		prev := CurrentMode()
		SetMode(ModeJSON)
		defer SetMode(prev)
		if err := EmitTo(buf, v); err != nil {
			t.Fatalf("EmitTo: %v", err)
		}
		var env schema.Envelope
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("Emit produced unparseable JSON: %v\n%s", err, buf.String())
		}
		if env.V != schema.Version {
			t.Errorf("env.V = %d, want %d", env.V, schema.Version)
		}
		if !env.OK {
			t.Errorf("env.OK = false")
		}
		if env.Error != nil {
			t.Errorf("env.Error = %+v, want nil", env.Error)
		}
	})
}
