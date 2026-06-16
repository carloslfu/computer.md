// SPDX-License-Identifier: Apache-2.0

"use client";

// Tiny pub/sub for task lifecycle SSE events. Used to bridge the
// dashboard's single live SSE consumer (ChatLayout owns the stream — it
// reads /api/stream with a manual fetch reader rather than EventSource,
// so it can set credentials and reconnect with backoff) with components
// like HistorySettings that want to react to task lifecycle without
// opening a duplicate stream.
//
// Flow:
//   1. ChatLayout receives an SSE event of interest.
//   2. ChatLayout calls publishTaskEvent({...}).
//   3. Any component that called subscribeTaskEvents() receives it.
//
// Module-level state is fine here because the dashboard is a single-
// page surface — one ChatLayout, zero or one Settings panel, all
// living in the same browser tab.

export type TaskEventKind =
  | "task:created"
  | "task:started"
  | "task:completed"
  | "task:cancelled"
  | "task:failed"
  | "task:activity";

export type TaskEvent = {
  kind: TaskEventKind;
  taskId?: string;
  conversationId?: string;
  status?: string;
};

type Listener = (event: TaskEvent) => void;

const listeners = new Set<Listener>();

export function publishTaskEvent(event: TaskEvent): void {
  // Take a snapshot so a listener that unsubscribes itself mid-fan-out
  // doesn't mutate the iteration set.
  for (const l of Array.from(listeners)) {
    try {
      l(event);
    } catch (err) {
      // Listener errors must NOT cascade — one component throwing
      // shouldn't break delivery to the others.
      console.error("task-events listener threw", err);
    }
  }
}

export function subscribeTaskEvents(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

// Test-only: clear all listeners between tests. Not exported in the
// public surface for components.
export function _resetTaskEventsForTest(): void {
  listeners.clear();
}
