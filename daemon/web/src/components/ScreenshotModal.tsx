// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronLeft, ChevronRight, X } from "lucide-react";

export type ScreenshotModalImage = {
  url: string;
  caption?: string;
};

export function ScreenshotModal({
  imageUrl,
  images,
  initialIndex = 0,
  onClose,
}: {
  imageUrl?: string;
  images?: ScreenshotModalImage[];
  initialIndex?: number;
  onClose: () => void;
}) {
  const list: ScreenshotModalImage[] =
    images && images.length > 0
      ? images
      : imageUrl
        ? [{ url: imageUrl }]
        : [];
  const [index, setIndex] = useState(
    Math.max(0, Math.min(initialIndex, list.length - 1)),
  );
  const closeButtonRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    closeButtonRef.current?.focus();
  }, []);

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        onClose();
        return;
      }
      if (list.length <= 1) return;
      if (e.key === "ArrowLeft") {
        e.preventDefault();
        setIndex((i) => Math.max(0, i - 1));
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        setIndex((i) => Math.min(list.length - 1, i + 1));
      }
    }
    document.addEventListener("keydown", handleKey);
    return () => document.removeEventListener("keydown", handleKey);
  }, [list.length, onClose]);

  if (list.length === 0) return null;
  const current = list[index];
  const canGoPrev = list.length > 1 && index > 0;
  const canGoNext = list.length > 1 && index < list.length - 1;

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4"
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label="Screenshot preview"
    >
      <div
        className="relative flex max-h-[92vh] max-w-[92vw] flex-col items-center gap-3"
        onClick={(e) => e.stopPropagation()}
      >
        <button
          ref={closeButtonRef}
          onClick={onClose}
          aria-label="Close screenshot"
          className="absolute -right-3 -top-3 z-10 flex h-8 w-8 items-center justify-center rounded-full bg-white shadow-lg transition hover:bg-slate-50"
        >
          <X className="h-4 w-4 text-slate-600" aria-hidden="true" />
        </button>

        <div className="relative">
          {canGoPrev && (
            <button
              type="button"
              onClick={() => setIndex((i) => Math.max(0, i - 1))}
              aria-label="Previous screenshot"
              className="absolute left-2 top-1/2 z-10 -translate-y-1/2 flex h-10 w-10 items-center justify-center rounded-full bg-white/90 text-slate-700 shadow-lg transition hover:bg-white"
            >
              <ChevronLeft className="h-5 w-5" aria-hidden="true" />
            </button>
          )}
          {canGoNext && (
            <button
              type="button"
              onClick={() =>
                setIndex((i) => Math.min(list.length - 1, i + 1))
              }
              aria-label="Next screenshot"
              className="absolute right-2 top-1/2 z-10 -translate-y-1/2 flex h-10 w-10 items-center justify-center rounded-full bg-white/90 text-slate-700 shadow-lg transition hover:bg-white"
            >
              <ChevronRight className="h-5 w-5" aria-hidden="true" />
            </button>
          )}
          <img
            src={current.url}
            alt={current.caption || "Screenshot"}
            className="max-h-[80vh] max-w-[88vw] rounded-xl shadow-2xl"
          />
        </div>

        {(current.caption || list.length > 1) && (
          <div className="flex flex-col items-center gap-0.5 px-2 text-center">
            {current.caption && (
              <p className="max-w-xl text-sm text-white/90">
                {current.caption}
              </p>
            )}
            {list.length > 1 && (
              <p className="text-xs text-white/50" aria-live="polite">
                {index + 1} / {list.length}
              </p>
            )}
          </div>
        )}
      </div>
    </div>,
    document.body,
  );
}
