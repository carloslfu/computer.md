// SPDX-License-Identifier: Apache-2.0

import { useState } from "react";
import { ScreenshotModal } from "./ScreenshotModal";
import type { ChatMessage as ChatMessageType } from "../lib/rehydrate";

export function ScreenshotCard({
  screenshots,
}: {
  screenshots: ChatMessageType[];
}) {
  const [lightboxIndex, setLightboxIndex] = useState<number | null>(null);

  const visible = screenshots.filter((s) => !!s.imageUrl);
  if (visible.length === 0) return null;

  const headerCaption =
    visible.find((s) => s.content?.trim())?.content?.trim() ?? "";

  const firstTimestamp = visible[0]?.timestamp ?? "";
  const parsed = firstTimestamp ? new Date(firstTimestamp) : null;
  const time =
    parsed && !isNaN(parsed.getTime())
      ? parsed.toLocaleTimeString("en-US", {
          hour: "numeric",
          minute: "2-digit",
        })
      : "";

  const isGroup = visible.length > 1;

  return (
    <div className="flex justify-start">
      <div className="max-w-[85%]">
        {headerCaption && (
          <p className="mb-1.5 text-xs leading-snug text-slate-500">
            {headerCaption}
          </p>
        )}
        <div className="flex flex-wrap items-start gap-1.5">
          {visible.map((s, i) => (
            <button
              key={s.id}
              type="button"
              onClick={() => setLightboxIndex(i)}
              aria-label={
                isGroup
                  ? `View screenshot ${i + 1} of ${visible.length}`
                  : "View screenshot"
              }
              className="group overflow-hidden rounded-lg border border-slate-200/60 bg-white/40 shadow-sm transition hover:-translate-y-0.5 hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400/60"
            >
              <img
                src={s.imageUrl}
                alt={s.content || "Agent screenshot"}
                className={
                  isGroup
                    ? "h-24 w-40 object-cover"
                    : "max-h-40 w-auto"
                }
              />
            </button>
          ))}
        </div>
        <div className="mt-1 flex items-center gap-2 text-[10px] text-slate-400">
          {time && <span>{time}</span>}
          {isGroup && (
            <>
              {time && <span aria-hidden="true">·</span>}
              <span>
                {visible.length} snapshot{visible.length === 1 ? "" : "s"}
              </span>
            </>
          )}
        </div>

        {lightboxIndex !== null && (
          <ScreenshotModal
            images={visible.map((s) => ({
              url: s.imageUrl!,
              caption: s.content?.trim() || undefined,
            }))}
            initialIndex={lightboxIndex}
            onClose={() => setLightboxIndex(null)}
          />
        )}
      </div>
    </div>
  );
}
