// SPDX-License-Identifier: Apache-2.0

// Package output is the single emission path for every CLI command.
//
// Two output modes:
//
//   - "json" (default): commands emit a schema.Envelope to stdout, errors
//     emit a schema.Envelope to stderr. JSON Lines for streaming commands.
//
//   - "text" (--text flag): commands emit human-readable text. Data types
//     opt into a text rendering by implementing the TextFormatter
//     interface; types that don't, fall back to pretty-printed JSON.
//
// All command code SHOULD route through Emit / EmitError / EmitEvent
// rather than calling fmt.Println directly. This keeps the JSON contract
// uniform and lets us add features (color, paging, validation) in one
// place.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/carloslfu/computer.md/cli/exit"
	"github.com/carloslfu/computer.md/cli/schema"
)

// Mode is one of "json" or "text". JSON is the default — the primary
// CLI persona is an agent reading stdout into context.
type Mode string

const (
	ModeJSON Mode = "json"
	ModeText Mode = "text"
)

var (
	mu          sync.RWMutex
	currentMode = ModeJSON
)

// SetMode changes the active output mode. Called once from the root
// command's PersistentPreRun based on the --json / --text flag value.
func SetMode(m Mode) {
	mu.Lock()
	defer mu.Unlock()
	currentMode = m
}

// CurrentMode returns the active output mode.
func CurrentMode() Mode {
	mu.RLock()
	defer mu.RUnlock()
	return currentMode
}

// TextFormatter is implemented by data types that have a humane text
// rendering. In --text mode, Emit calls TextFormat() instead of
// JSON-encoding. Types that don't implement this interface fall back to
// indented JSON in --text mode.
type TextFormatter interface {
	TextFormat() string
}

// Emit writes a success envelope to stdout.
//
// In JSON mode: `{"v":1,"ok":true,"data":<data>}` followed by a newline.
// In text mode: data.TextFormat() if implemented; else indented JSON.
//
// Commands call this exactly once per successful invocation, before
// returning nil (or a *exit.OutcomeError for non-success task outcomes).
func Emit(data any) error {
	return EmitTo(os.Stdout, data)
}

// EmitTo is Emit with a caller-supplied writer. Tests pass a bytes.Buffer.
//
// G4: every JSON-emitting path goes through `validateEnvelope` before
// the bytes hit the wire. In tests (Validator.FailLoud) a malformed
// envelope panics; in prod it surfaces as an `internal_error` envelope
// on stderr so we never ship un-shaped output but also never silently
// hide a wire-format regression from the developer.
func EmitTo(w io.Writer, data any) error {
	if CurrentMode() == ModeText {
		if tf, ok := data.(TextFormatter); ok {
			_, err := fmt.Fprintln(w, tf.TextFormat())
			return err
		}
		// Fallback for types without a text formatter: pretty JSON.
		// Better than nothing, and makes the gap obvious when a
		// developer expected text and got JSON.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(data)
	}

	env := schema.Success(data)
	if err := validateEnvelope(env); err != nil {
		// Loud in tests, soft in prod — see validateEnvelope's docstring.
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(env)
}

// validateEnvelope checks that a constructed success envelope is
// well-formed before we write it.
//
// Checks: v == schema.Version, OK == true, Error == nil, Data round-trips
// through encoding/json (catches non-marshalable types like channels or
// function values that would otherwise produce a half-empty JSON object).
//
// In test builds (FailLoudOnSchemaError == true) a validation failure
// panics so CI catches it; in normal builds it returns an error so the
// caller can emit a CLI-level `internal_error` envelope.
func validateEnvelope(env schema.Envelope) error {
	if env.V != schema.Version {
		return fmt.Errorf("envelope.v = %d, want %d", env.V, schema.Version)
	}
	if !env.OK {
		return fmt.Errorf("success envelope has ok=false")
	}
	if env.Error != nil {
		return fmt.Errorf("success envelope has non-nil error")
	}
	if env.Data == nil {
		// Allowed — a few verbs (auth logout) emit an empty data type.
		return nil
	}
	// Round-trip to JSON to catch unmarshalable types.
	if _, err := json.Marshal(env.Data); err != nil {
		return fmt.Errorf("envelope.data not JSON-marshalable: %w", err)
	}
	return nil
}

// EmitError writes a failure envelope to stderr.
//
// Special cases:
//   - *exit.OutcomeError → no-op. The success envelope was already
//     written by the command; we just propagate the exit code via main().
//   - *schema.Error → emitted verbatim.
//   - any other error → wrapped in schema.Error{Code: "internal_error"}.
//
// EmitError is idempotent and safe to call from a deferred handler.
func EmitError(err error) {
	EmitErrorTo(os.Stderr, err)
}

// EmitErrorTo is EmitError with a caller-supplied writer.
func EmitErrorTo(w io.Writer, err error) {
	if err == nil {
		return
	}
	var oe *exit.OutcomeError
	if errors.As(err, &oe) {
		// Outcome-level non-zero exit. The command already wrote the
		// success envelope describing the terminal task state; nothing
		// to emit here. main() picks up the exit code.
		return
	}

	var se *schema.Error
	if !errors.As(err, &se) {
		se = schema.Newf(schema.CodeInternal, "%s", err.Error())
	}

	if CurrentMode() == ModeText {
		fmt.Fprintf(w, "Error: %s\n", se.Message)
		if se.Hint != "" {
			fmt.Fprintf(w, "Hint:  %s\n", se.Hint)
		}
		return
	}

	env := schema.Failure(se)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(env)
}

// EmitEvent writes one streaming event (JSON Lines) to stdout.
//
// Streaming commands call this once per event, in order. The event is
// emitted in BOTH json and text modes — there is no "text mode" for a
// stream of structured events; text mode is for unary commands.
//
// `fields` is the event-specific payload. The {v, event, ts} envelope
// fields are added by EmitEvent and MUST NOT be set by callers.
func EmitEvent(eventType, ts string, fields map[string]any) error {
	return EmitEventTo(os.Stdout, eventType, ts, fields)
}

// EmitEventTo is EmitEvent with a caller-supplied writer.
func EmitEventTo(w io.Writer, eventType, ts string, fields map[string]any) error {
	if fields == nil {
		fields = map[string]any{}
	}
	// Defensive: refuse to silently overwrite reserved keys. A caller
	// passing "v" or "event" in fields is almost certainly a bug.
	for _, reserved := range []string{"v", "event", "ts"} {
		if _, taken := fields[reserved]; taken {
			return fmt.Errorf("output: event field %q is reserved", reserved)
		}
	}
	out := make(map[string]any, len(fields)+3)
	out["v"] = schema.Version
	out["event"] = eventType
	out["ts"] = ts
	for k, v := range fields {
		out[k] = v
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
