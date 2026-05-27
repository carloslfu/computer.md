// SPDX-License-Identifier: Apache-2.0

// Package schema defines the agent-facing JSON wire format emitted by the CLI.
//
// Two shapes:
//
//  1. Unary envelope — every non-streaming command writes exactly one of:
//     {"v":1,"ok":true,"data":<command-specific object>}
//     {"v":1,"ok":false,"error":{"code":"...","message":"...","hint":"..."}}
//     Success goes to stdout, errors to stderr.
//
//  2. Stream event — streaming commands (`task stream`, `events --tail`,
//     `task messages --follow`) emit JSON Lines:
//     {"v":1,"event":"<type>","ts":"<iso8601>",<event-specific fields>}
//     One event per line, no outer envelope, no array wrapping.
//
// The envelope's `ok` field reflects whether the CLI succeeded in talking
// to the daemon and getting an answer — NOT whether the task itself
// succeeded. Task outcome is signalled via the process exit code (see
// the cli/exit package).
package schema

// Version is the current wire-format version. Bump on breaking changes.
// Additive changes (new optional fields, new event types) do not bump.
const Version = 1

// Success builds a success envelope around the given data.
func Success(data any) Envelope {
	return Envelope{V: Version, OK: true, Data: data}
}

// Failure builds an error envelope.
func Failure(err *Error) Envelope {
	return Envelope{V: Version, OK: false, Error: err}
}

// Envelope is the shape of every non-streaming CLI response.
//
// Exactly one of Data or Error is set, matched to OK. We keep both fields
// `omitempty` so a success envelope never has a stray null `error` and
// vice versa, which keeps the wire format stable across Go's reflection
// quirks and any future renderer (e.g. jq, fx).
type Envelope struct {
	V     int    `json:"v"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}
