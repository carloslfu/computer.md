// SPDX-License-Identifier: Apache-2.0

"use client";

import { useCallback, useEffect, useRef, useState } from "react";

// Smart auto-scroll for chat. Three modes, switched by a single
// `isPinned` flag:
//
//   1. Pinned (default) — new content auto-scrolls to bottom instantly.
//   2. User scrolls up — pin breaks, auto-scroll stops, a "Jump to
//      bottom" pill appears. Incoming assistant messages tally on the
//      pill so the user sees "↓ 3 new".
//   3. User scrolls back to bottom (within PIN_THRESHOLD_PX) — pin
//      re-engages automatically.
//
// The asymmetric pin rule is the single hardest detail and where
// homegrown implementations usually go wrong:
//   - Any user scroll-up unpins (immediately).
//   - Only a USER scroll back to the bottom re-pins. A programmatic
//     scroll landing at the bottom must NOT re-pin — otherwise the
//     auto-scroll on the next streamed token would "trap" the user
//     who'd just scrolled up to read.
//
// Detection: every scrollTop write is wrapped in an
// isProgrammaticScroll flag, cleared on the next animation frame. The
// scroll-event handler ignores events fired while the flag is set.

const PIN_THRESHOLD_PX = 64;

type MinimalMessage = { id: string; role: string };

export type UseChatScrollResult = {
  // Attach to the scrollable container element (the one with
  // overflow-y-auto).
  containerRef: React.RefObject<HTMLDivElement>;
  // Attach to the INNER content element (the one whose height changes
  // when messages are added or stream). ResizeObserver watches this.
  contentRef: React.RefObject<HTMLDivElement>;
  isPinned: boolean;
  // Count of assistant messages that arrived while unpinned. Resets
  // to 0 when the user re-pins (manually or via jumpToBottom).
  newMessageCount: number;
  jumpToBottom: () => void;
};

export function useChatScroll<T extends MinimalMessage>(opts: {
  messages: T[];
  // Reset key — changing this resets pin state to true and scrolls to
  // bottom (e.g., when the user switches between conversations).
  resetKey?: string | null;
}): UseChatScrollResult {
  const { messages, resetKey } = opts;

  const containerRef = useRef<HTMLDivElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);

  const [isPinned, setIsPinned] = useState(true);
  const [newMessageCount, setNewMessageCount] = useState(0);

  // Refs that mirror state, so effect callbacks can read the current
  // value without re-binding listeners on every change.
  const isPinnedRef = useRef(true);
  const isProgrammaticRef = useRef(false);

  // Track message identity to detect ADDITIONS (vs. swaps, edits, or
  // a wholesale conversation switch).
  const lastCountRef = useRef(messages.length);
  const lastTailIdRef = useRef<string | null>(
    messages.length > 0 ? messages[messages.length - 1].id : null,
  );

  useEffect(() => {
    isPinnedRef.current = isPinned;
  }, [isPinned]);

  const isAtBottom = useCallback((): boolean => {
    const el = containerRef.current;
    if (!el) return true;
    return el.scrollHeight - el.scrollTop - el.clientHeight <= PIN_THRESHOLD_PX;
  }, []);

  const scrollToBottom = useCallback((behavior: ScrollBehavior = "auto") => {
    const el = containerRef.current;
    if (!el) return;
    isProgrammaticRef.current = true;
    if (behavior === "smooth") {
      el.scrollTo({ top: el.scrollHeight, behavior: "smooth" });
      // A smooth scroll animates over MANY frames. Clearing the guard
      // after a single rAF would expose the intermediate (not-yet-at-
      // bottom) scroll events to the handler, which — with the pin
      // already re-engaged by jumpToBottom — would read !atBottom and
      // immediately unpin again. Hold the guard until the animation has
      // actually landed at the bottom, with a frame-count cap so a
      // never-settling element can't pin the flag forever.
      let frames = 0;
      const MAX_FRAMES = 60; // ~1s at 60fps; smooth scrolls finish well within this.
      const settle = () => {
        if (!isProgrammaticRef.current) return; // a newer scroll superseded us
        frames += 1;
        const done =
          el.scrollHeight - el.scrollTop - el.clientHeight <= PIN_THRESHOLD_PX;
        if (done || frames >= MAX_FRAMES) {
          isProgrammaticRef.current = false;
          return;
        }
        requestAnimationFrame(settle);
      };
      requestAnimationFrame(settle);
      return;
    }
    // Direct assignment is the fastest path — no smooth animation,
    // no scrollTo overhead. Right thing during streaming when this
    // fires on every token.
    el.scrollTop = el.scrollHeight;
    // The scroll event from this write will fire next; clear the flag
    // AFTER it. requestAnimationFrame is enough — the synthesized
    // scroll event is dispatched within the same frame as the
    // scrollTop write.
    requestAnimationFrame(() => {
      isProgrammaticRef.current = false;
    });
  }, []);

  const jumpToBottom = useCallback(() => {
    const reduceMotion =
      typeof window !== "undefined" &&
      window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    scrollToBottom(reduceMotion ? "auto" : "smooth");
    setIsPinned(true);
    isPinnedRef.current = true;
    setNewMessageCount(0);
  }, [scrollToBottom]);

  // ── Initial mount: scroll to bottom when the chat first has content.
  // Run only once after the first non-empty render so the user lands
  // at the latest message.
  const didInitialScrollRef = useRef(false);
  useEffect(() => {
    if (didInitialScrollRef.current) return;
    if (messages.length === 0) return;
    didInitialScrollRef.current = true;
    // Wait one frame for the DOM to lay out.
    requestAnimationFrame(() => scrollToBottom("auto"));
  }, [messages.length, scrollToBottom]);

  // ── Reset on conversation switch. resetKey is the conversation id;
  // when it changes, we forget any unpin state from the previous
  // conversation and snap to the bottom of the new one.
  useEffect(() => {
    setIsPinned(true);
    isPinnedRef.current = true;
    setNewMessageCount(0);
    // Reset the initial-scroll guard so the new conversation also
    // scrolls to bottom on its first render.
    didInitialScrollRef.current = false;
    requestAnimationFrame(() => scrollToBottom("auto"));
  }, [resetKey, scrollToBottom]);

  // ── ResizeObserver on the inner content. Fires on every height
  // change: streaming tokens, image loads, code blocks expanding,
  // typing-indicator appearance, anything. This is the SOLE trigger
  // for auto-scroll-while-pinned.
  useEffect(() => {
    const content = contentRef.current;
    if (!content) return;
    const ro = new ResizeObserver(() => {
      if (isPinnedRef.current) {
        scrollToBottom("auto");
      }
    });
    ro.observe(content);
    return () => ro.disconnect();
  }, [scrollToBottom]);

  // ── Scroll handler. Distinguishes user-driven scrolls from our own
  // programmatic ones (via isProgrammaticRef). Updates the pin flag
  // based on whether the user has scrolled to / away from the bottom.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const onScroll = () => {
      if (isProgrammaticRef.current) return;
      const atBottom = isAtBottom();
      if (atBottom && !isPinnedRef.current) {
        // Re-pin: user voluntarily scrolled back to bottom.
        setIsPinned(true);
        isPinnedRef.current = true;
        setNewMessageCount(0);
      } else if (!atBottom && isPinnedRef.current) {
        // Unpin: user scrolled up.
        setIsPinned(false);
        isPinnedRef.current = false;
      }
    };
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => el.removeEventListener("scroll", onScroll);
  }, [isAtBottom]);

  // ── New-message counter for the pill. Only counts messages with
  // role !== "user" (the user's own messages always scroll into view;
  // they're not "new arrivals" to surface). Only increments while
  // unpinned — when pinned the user is already seeing them live.
  useEffect(() => {
    const prevCount = lastCountRef.current;
    const newCount = messages.length;
    const newTailId =
      messages.length > 0 ? messages[messages.length - 1].id : null;

    lastCountRef.current = newCount;
    const prevTailId = lastTailIdRef.current;
    lastTailIdRef.current = newTailId;

    if (newCount <= prevCount) return;
    if (newTailId === prevTailId) return;
    if (isPinnedRef.current) return;

    // Count how many of the newly-appended items are from the agent.
    const added = messages.slice(prevCount);
    const agentAdded = added.filter((m) => m.role !== "user").length;
    if (agentAdded > 0) {
      setNewMessageCount((c) => c + agentAdded);
    }
  }, [messages]);

  return {
    containerRef,
    contentRef,
    isPinned,
    newMessageCount,
    jumpToBottom,
  };
}
