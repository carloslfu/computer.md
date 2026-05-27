// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { fetchInboxObjectURL } from "../lib/attachments";

// Same-origin variant: the daemon SPA fetches /api/inbox/... directly
// with the vc_session cookie. No host/token args needed.
export function AttachmentImage({
  conversationId,
  filename,
  alt,
  onClick,
  className,
}: {
  conversationId: string;
  filename: string;
  alt?: string;
  onClick?: () => void;
  className?: string;
}) {
  const [src, setSrc] = useState<string | null>(null);
  const [error, setError] = useState(false);

  useEffect(() => {
    let cancelled = false;
    let currentUrl: string | null = null;
    const controller = new AbortController();

    fetchInboxObjectURL({
      conversationId,
      filename,
      signal: controller.signal,
    })
      .then((url) => {
        if (cancelled) {
          URL.revokeObjectURL(url);
          return;
        }
        currentUrl = url;
        setSrc(url);
      })
      .catch((err) => {
        if (cancelled || err?.name === "AbortError") return;
        setError(true);
      });

    return () => {
      cancelled = true;
      controller.abort();
      if (currentUrl) URL.revokeObjectURL(currentUrl);
    };
  }, [conversationId, filename]);

  if (error) {
    return (
      <div
        className={`flex items-center justify-center rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 text-[11px] text-slate-400 ${className ?? ""}`}
        role="img"
        aria-label={alt || "Attachment failed to load"}
      >
        Couldn&apos;t load image
      </div>
    );
  }

  if (!src) {
    return (
      <div
        className={`animate-pulse rounded-lg bg-slate-100 ${className ?? ""}`}
        role="status"
        aria-label="Loading attachment"
      />
    );
  }

  const img = (
    <img
      src={src}
      alt={alt || "Attachment"}
      className="h-full w-full object-cover"
      draggable={false}
    />
  );

  if (onClick) {
    return (
      <button
        type="button"
        onClick={onClick}
        aria-label={alt || "View attachment"}
        className={`overflow-hidden rounded-lg border border-slate-200/60 shadow-sm transition hover:shadow-md ${className ?? ""}`}
      >
        {img}
      </button>
    );
  }

  return (
    <div className={`overflow-hidden rounded-lg border border-slate-200/60 ${className ?? ""}`}>
      {img}
    </div>
  );
}
