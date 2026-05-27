// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/carloslfu/computer.md/daemon/audit"
)

// handleDaemonTask is the localhost-only "inbox" for systems the manager
// authored. Cron jobs, watchers, sub-agents — anything the manager set
// running on this machine — can POST a follow-up task here without
// holding a JWT or API key.
//
// Threat model: identical to /routes/verify and /routes. The only
// processes that can reach 127.0.0.1:8420 are processes the manager
// itself spawned (or the customer running things from the desktop).
// Caddy never forwards external traffic to loopback; the
// withLocalhostAuth middleware enforces the binding (rejects any
// request with X-Forwarded-For set or a non-127.0.0.1 RemoteAddr).
//
// Body:
//
//	{
//	  "instruction":     "<the task prompt for the manager>",
//	  "conversation_id": "<optional; defaults to system:<name> or random uuid>",
//	  "system":          "<optional name of the system that triggered this>"
//	}
//
// Returns the created task (201).
func (s *Server) handleDaemonTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Instruction    string `json:"instruction"`
		Message        string `json:"message"`
		ConversationID string `json:"conversation_id"`
		System         string `json:"system"`
	}

	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Accept both "instruction" and "message" — the public /task does
	// the same; mirror it so daemon callers don't have to remember
	// which field name applies where.
	if req.Instruction == "" {
		req.Instruction = req.Message
	}
	if req.Instruction == "" {
		jsonError(w, "instruction is required", http.StatusBadRequest)
		return
	}

	// If a system name was supplied and no conversation id, group every
	// wake-up from that system under a stable id so the manager has
	// continuity across firings. Pattern: "system:<name>". Otherwise
	// generate a fresh uuid (one-shot scripts).
	if req.ConversationID == "" {
		if req.System != "" {
			req.ConversationID = "system:" + req.System
		} else {
			req.ConversationID = uuid.New().String()
		}
	}

	task, err := s.taskStore.CreateTask(req.ConversationID, req.Instruction)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to create task: %v", err), http.StatusInternalServerError)
		return
	}

	s.auditLog.Log(audit.Entry{
		Action:   "task_created_from_daemon",
		Category: "task",
		TaskID:   task.ID,
		Details:  fmt.Sprintf("system=%s instruction=%s", req.System, req.Instruction),
	})

	if s.broker != nil {
		s.broker.Emit("task:created", task)
	}

	jsonResponse(w, http.StatusCreated, task)
}
