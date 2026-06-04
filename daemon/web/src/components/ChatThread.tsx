// SPDX-License-Identifier: Apache-2.0

import { useMemo } from "react";
import { ArrowRight, ChevronDown, Square } from "lucide-react";
import { ChatMessage } from "./ChatMessage";
import { ScreenshotCard } from "./ScreenshotCard";
import {
  isRunningActivitySummary,
  type ChatMessage as ChatMessageType,
} from "../lib/rehydrate";
import { useChatScroll } from "../lib/use-chat-scroll";

type RenderUnit =
  | { kind: "single"; message: ChatMessageType }
  | { kind: "screenshots"; messages: ChatMessageType[] };

function isGroupableScreenshot(m: ChatMessageType): boolean {
  return m.type === "screenshot" && m.role === "agent" && !!m.imageUrl;
}

export function groupRenderUnits(messages: ChatMessageType[]): RenderUnit[] {
  const units: RenderUnit[] = [];
  let i = 0;
  while (i < messages.length) {
    const m = messages[i];
    if (isGroupableScreenshot(m)) {
      const run: ChatMessageType[] = [m];
      let j = i + 1;
      while (j < messages.length && isGroupableScreenshot(messages[j])) {
        run.push(messages[j]);
        j += 1;
      }
      units.push({ kind: "screenshots", messages: run });
      i = j;
    } else {
      units.push({ kind: "single", message: m });
      i += 1;
    }
  }
  return units;
}

// First-run starters. Tapping one fills the composer (it never auto-sends).
// Ordered to show the breadth in one glance: it watches, remembers, connects,
// automates, and runs work in parallel — not just a chat box.
const STARTERS: Array<{ tag: string; prompt: string }> = [
  {
    tag: "Watch it work",
    prompt:
      "Pull today's top Hacker News headlines and summarize each in one line.",
  },
  {
    tag: "Build memory",
    prompt:
      "Interview me about my business, then start a knowledge base for my company.",
  },
  {
    tag: "Connect a tool",
    prompt: "Open Gmail in the browser and walk me through connecting it.",
  },
  {
    tag: "Automate it",
    prompt: "Every weekday at 8am, send me a brief of what needs my attention.",
  },
  {
    tag: "Go parallel",
    prompt: "Spawn five workers to research my top competitors at once.",
  },
];

export function ChatThread({
  messages,
  onApproval,
  isWorking,
  loading,
  conversationId,
  vaultNames,
  queuedBehind,
  onOpenBlocker,
  onStopBlocker,
  blockerStopping,
  onPickPrompt,
}: {
  messages: ChatMessageType[];
  onApproval: (messageId: string, action: string) => void;
  isWorking?: boolean;
  loading?: boolean;
  conversationId: string | null;
  vaultNames: string[];
  // When set, the engine is currently occupied by a task in a DIFFERENT
  // conversation. We render a panel naming the blocking conversation in
  // place of the "thinking" dots. This is the honest version of the
  // engine state: nothing is running here yet, and the user can see why.
  queuedBehind?: { id: string; title: string } | null;
  // Switches to the blocking conversation when the user wants to look
  // at what's happening over there.
  onOpenBlocker?: (id: string) => void;
  // Cancels the blocking task, freeing the engine for this conversation.
  // Only wired when the viewer has control access; view-only watchers
  // see the panel without the stop button.
  onStopBlocker?: () => void;
  // Mirrors the header's stop-button stopping flag so the queued panel's
  // Stop button shows the same disabled-spinning state.
  blockerStopping?: boolean;
  // When set (control access only), the empty-state starters become tappable
  // and fill the composer through this callback. Omitted for view-only.
  onPickPrompt?: (text: string) => void;
}) {
  const { containerRef, contentRef, isPinned, newMessageCount, jumpToBottom } =
    useChatScroll({ messages, resetKey: conversationId });

  const units = useMemo(() => groupRenderUnits(messages), [messages]);
  const hasRunningActivity = useMemo(
    () =>
      messages.some(
        (m) => m.type === "activity_summary" && isRunningActivitySummary(m.content),
      ),
    [messages],
  );

  const screenshotCountByActivityId = useMemo(() => {
    const byId = new Map<string, number>();
    let runningCount = 0;
    for (const m of messages) {
      if (m.role === "user") {
        runningCount = 0;
        continue;
      }
      if (isGroupableScreenshot(m)) {
        runningCount += 1;
        continue;
      }
      if (m.type === "activity_summary") {
        if (runningCount > 0) byId.set(m.id, runningCount);
        runningCount = 0;
      }
    }
    return byId;
  }, [messages]);

  return (
    <div className="relative flex flex-1 flex-col overflow-hidden">
      <div
        ref={containerRef}
        className="flex-1 overflow-y-auto px-4 py-6 sm:px-6"
        aria-live="polite"
        aria-label="Chat messages"
      >
        <div ref={contentRef} className="mx-auto max-w-2xl space-y-4">
          {loading && messages.length === 0 && (
            <div className="animate-pulse space-y-4">
              <div className="flex justify-start">
                <div className="w-2/3 space-y-2 rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/50 px-4 py-3">
                  <div className="h-3 w-full rounded bg-slate-200/50" />
                  <div className="h-3 w-4/5 rounded bg-slate-200/50" />
                </div>
              </div>
              <div className="flex justify-end">
                <div className="w-1/2 space-y-2 rounded-2xl rounded-br-md bg-slate-950/5 px-4 py-3">
                  <div className="h-3 w-full rounded bg-slate-200/60" />
                  <div className="h-3 w-2/3 rounded bg-slate-200/60" />
                </div>
              </div>
              <div className="flex justify-start">
                <div className="w-3/5 space-y-2 rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/50 px-4 py-3">
                  <div className="h-3 w-full rounded bg-slate-200/50" />
                  <div className="h-3 w-3/4 rounded bg-slate-200/50" />
                  <div className="h-3 w-1/2 rounded bg-slate-200/50" />
                </div>
              </div>
            </div>
          )}

          {!loading && messages.length === 0 && (
            <div className="py-16 text-center">
              <p className="font-poppins text-lg font-semibold text-slate-950">
                Your computer is ready.
              </p>
              <p className="mx-auto mt-3 max-w-md text-sm leading-relaxed text-slate-500">
                Tell the manager what to do in plain language. It runs one-shot
                jobs, builds systems that run on their own, and spawns worker
                agents when a job needs parallel hands. Pick a starting point or
                just type.
              </p>
              <div className="mx-auto mt-8 grid max-w-md gap-2 text-left">
                {STARTERS.map((s) =>
                  onPickPrompt ? (
                    <button
                      key={s.prompt}
                      type="button"
                      onClick={() => onPickPrompt(s.prompt)}
                      className="rounded-xl border border-slate-200/60 bg-white/50 px-4 py-2.5 transition hover:-translate-y-0.5 hover:border-slate-300 hover:bg-white hover:shadow-sm"
                    >
                      <span className="block text-[11px] font-medium uppercase tracking-wider text-slate-400">
                        {s.tag}
                      </span>
                      <span className="mt-0.5 block text-sm text-slate-600">
                        {s.prompt}
                      </span>
                    </button>
                  ) : (
                    <div
                      key={s.prompt}
                      className="rounded-xl border border-slate-200/60 bg-white/50 px-4 py-2.5"
                    >
                      <span className="block text-[11px] font-medium uppercase tracking-wider text-slate-400">
                        {s.tag}
                      </span>
                      <span className="mt-0.5 block text-sm text-slate-600">
                        {s.prompt}
                      </span>
                    </div>
                  ),
                )}
              </div>
            </div>
          )}

          {units.map((unit, i) => {
            const key =
              unit.kind === "screenshots"
                ? `ss-group-${unit.messages[0].id}`
                : unit.message.id;
            return (
              <div
                key={key}
                className="animate-fade-in"
                style={{ animationDelay: `${Math.min(i * 50, 300)}ms` }}
              >
                {unit.kind === "screenshots" ? (
                  <ScreenshotCard screenshots={unit.messages} />
                ) : (
                  <ChatMessage
                    message={unit.message}
                    onApproval={onApproval}
                    conversationId={conversationId}
                    vaultNames={vaultNames}
                    snapshotCount={
                      unit.message.type === "activity_summary"
                        ? screenshotCountByActivityId.get(unit.message.id)
                        : undefined
                    }
                  />
                )}
              </div>
            );
          })}

          {/* Cross-conversation queued state takes precedence over the
              generic "thinking" dots: when the engine is busy on another
              chat, the dots in THIS chat would be a lie (nothing is
              happening here yet). Show the blocker panel instead. The
              dots still render when the work is actually in this chat. */}
          {queuedBehind ? (
            <QueuedBehindPanel
              title={queuedBehind.title}
              onOpen={
                onOpenBlocker ? () => onOpenBlocker(queuedBehind.id) : undefined
              }
              onStop={onStopBlocker}
              stopping={blockerStopping}
            />
          ) : (
            isWorking &&
            !hasRunningActivity && (
              <div className="flex justify-start">
                <div className="rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/50 px-4 py-3">
                  <div
                    className="flex items-center gap-1"
                    role="status"
                    aria-label="Agent is thinking"
                  >
                    <span
                      className="h-1.5 w-1.5 animate-bounce rounded-full bg-slate-400"
                      style={{ animationDelay: "0ms" }}
                    />
                    <span
                      className="h-1.5 w-1.5 animate-bounce rounded-full bg-slate-400"
                      style={{ animationDelay: "150ms" }}
                    />
                    <span
                      className="h-1.5 w-1.5 animate-bounce rounded-full bg-slate-400"
                      style={{ animationDelay: "300ms" }}
                    />
                  </div>
                </div>
              </div>
            )
          )}
        </div>
      </div>

      {!isPinned && (
        <button
          type="button"
          onClick={jumpToBottom}
          aria-label={
            newMessageCount > 0
              ? `Jump to bottom, ${newMessageCount} new ${
                  newMessageCount === 1 ? "message" : "messages"
                }`
              : "Jump to bottom"
          }
          className="animate-fade-in absolute bottom-4 left-1/2 z-10 flex h-9 w-9 -translate-x-1/2 items-center justify-center rounded-full border border-slate-200/80 bg-white text-slate-600 shadow-md transition hover:-translate-y-0.5 hover:bg-slate-50 hover:text-slate-900 hover:shadow-lg"
        >
          <ChevronDown className="h-4 w-4" aria-hidden="true" />
          {newMessageCount > 0 && (
            <span
              aria-hidden="true"
              className="absolute -right-1 -top-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-slate-950 px-1 text-[10px] font-medium leading-none text-white"
            >
              {newMessageCount > 99 ? "99+" : newMessageCount}
            </span>
          )}
        </button>
      )}
    </div>
  );
}

// QueuedBehindPanel is what renders in the current chat when the engine
// is occupied on a task that lives in a different conversation. The
// design goal is honesty over animation: don't pretend something is
// happening here — name what's actually blocking, offer the two
// recoverable actions (jump to the blocking chat, or stop it).
function QueuedBehindPanel({
  title,
  onOpen,
  onStop,
  stopping,
}: {
  title: string;
  onOpen?: () => void;
  onStop?: () => void;
  stopping?: boolean;
}) {
  return (
    <div className="flex justify-start" role="status" aria-live="polite">
      <div className="max-w-[85%] rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/60 px-4 py-3 shadow-sm">
        <div className="flex flex-wrap items-center gap-2">
          <span className="inline-flex items-center gap-1 rounded-full bg-amber-50 px-2 py-0.5 text-[10px] font-medium uppercase tracking-widest text-amber-700 ring-1 ring-amber-200/60">
            Queued
          </span>
          <p className="text-[13px] text-slate-700">
            Your manager is working in{" "}
            <span className="font-medium text-slate-900">{title}</span>. This
            chat will start when that finishes.
          </p>
        </div>
        {(onOpen || onStop) && (
          <div className="mt-2.5 flex flex-wrap gap-1.5">
            {onOpen && (
              <button
                type="button"
                onClick={onOpen}
                className="inline-flex items-center gap-1 rounded-full border border-slate-200/80 bg-white px-2.5 py-1 text-[11px] font-medium text-slate-700 transition hover:border-slate-300 hover:text-slate-950"
              >
                Open that chat
                <ArrowRight className="h-3 w-3" aria-hidden="true" />
              </button>
            )}
            {onStop && (
              <button
                type="button"
                onClick={onStop}
                disabled={stopping}
                className="inline-flex items-center gap-1 rounded-full border border-slate-200/80 bg-white px-2.5 py-1 text-[11px] font-medium text-slate-600 transition hover:border-slate-300 hover:text-slate-950 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {stopping ? (
                  <span
                    className="h-2.5 w-2.5 animate-spin rounded-full border-[1.5px] border-slate-300 border-t-slate-600"
                    aria-hidden="true"
                  />
                ) : (
                  <Square className="h-2.5 w-2.5 fill-current" aria-hidden="true" />
                )}
                {stopping ? "Stopping" : "Stop that task"}
              </button>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
