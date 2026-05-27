// SPDX-License-Identifier: Apache-2.0

package schema

// Stream event types emitted by `task stream`, `events --tail`, and
// `task messages --follow`. Each line of the stream is a JSON object
// with at least `{v, event, ts}`; event-specific fields are listed
// per-type below.
//
// Agents key off the `event` field to dispatch. New event types are
// additive — older agents seeing an unknown `event` value should skip
// the line, not error.
const (
	// Lifecycle.
	EventStatus = "status" // task transitioned to a new status
	EventFinal  = "final"  // task reached a terminal state; stream ends after this
	EventError  = "error"  // stream-level error; stream ends after this
	EventEnd    = "end"    // clean stream close with no final task event (e.g. events --tail SIGINT)

	// Conversation content.
	EventMessage    = "message"     // a new message from the manager (or user)
	EventToolCall   = "tool_call"   // the manager called a tool
	EventToolResult = "tool_result" // a tool call returned

	// Heartbeats.
	EventHeartbeat = "heartbeat" // periodic keepalive so agents know the stream is alive
)
