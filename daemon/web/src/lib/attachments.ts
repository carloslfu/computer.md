// SPDX-License-Identifier: Apache-2.0

// Attachment types + same-origin uploader for the daemon SPA. Adapted
// from lib/chat/attachments.ts on the platform: host/token args are
// gone because the SPA is already on the daemon host and authenticates
// via the vc_session cookie. XMLHttpRequest is still used so we can
// surface progress on big file uploads.

import { platformGrantURL } from "../auth/types";

export type Attachment = {
  name: string;
  path: string;
  mime: string;
  size: number;
  original?: string;
};

export type ComposerAttachment = {
  localId: string;
  file: File;
  previewUrl?: string;
  status: "uploading" | "ready" | "error";
  progress?: number;
  attachment?: Attachment;
  error?: string;
};

export const MAX_COMPOSER_ATTACHMENTS = 10;
export const MAX_UPLOAD_FILE_BYTES = 25 * 1024 * 1024;

type UploadResult = {
  attachments: Attachment[];
};

export function isImageMime(mime: string | undefined | null): boolean {
  if (!mime) return false;
  return mime.toLowerCase().startsWith("image/");
}

export function uploadFile({
  conversationId,
  file,
  onProgress,
  signal,
}: {
  conversationId: string;
  file: File;
  onProgress?: (fraction: number) => void;
  signal?: AbortSignal;
}): Promise<Attachment> {
  return new Promise((resolve, reject) => {
    if (file.size > MAX_UPLOAD_FILE_BYTES) {
      reject(
        new Error(
          `File is too large (max ${formatBytes(MAX_UPLOAD_FILE_BYTES)}).`,
        ),
      );
      return;
    }

    const form = new FormData();
    form.append("conversation_id", conversationId);
    form.append("file", file, file.name);

    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/upload");
    xhr.withCredentials = true; // ride the vc_session cookie

    xhr.upload.onprogress = (e) => {
      if (!onProgress || !e.lengthComputable) return;
      onProgress(e.loaded / e.total);
    };

    xhr.onload = () => {
      if (xhr.status === 401) {
        // Mid-upload session loss — kick to the grant flow. The XHR
        // doesn't have an apiFetch hook so we do this inline.
        const returnPath = window.location.pathname + window.location.search;
        window.location.href = platformGrantURL(returnPath);
        reject(new Error("Auth required"));
        return;
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const data = JSON.parse(xhr.responseText) as UploadResult;
          if (!data.attachments || data.attachments.length === 0) {
            reject(new Error("Upload succeeded but returned no attachment"));
            return;
          }
          resolve(data.attachments[0]);
        } catch {
          reject(new Error("Upload response was not valid JSON"));
        }
        return;
      }
      let message = "Upload failed";
      try {
        const data = JSON.parse(xhr.responseText) as { error?: string };
        if (data.error) message = data.error;
      } catch {
        // Default message wins.
      }
      reject(new Error(message));
    };
    xhr.onerror = () => reject(new Error("Network error during upload"));
    xhr.onabort = () =>
      reject(new DOMException("Upload aborted", "AbortError"));

    if (signal) {
      if (signal.aborted) {
        xhr.abort();
        reject(new DOMException("Upload aborted", "AbortError"));
        return;
      }
      signal.addEventListener("abort", () => xhr.abort(), { once: true });
    }

    xhr.send(form);
  });
}

// Fetches a previously-uploaded file as a blob URL the browser can use
// in <img src>. Caller revokes when the element unmounts.
export async function fetchInboxObjectURL({
  conversationId,
  filename,
  signal,
}: {
  conversationId: string;
  filename: string;
  signal?: AbortSignal;
}): Promise<string> {
  // eslint-disable-next-line no-restricted-syntax -- blob fetch for an <img> object URL; apiFetch's JSON wrapper is the wrong shape
  const res = await fetch(
    `/api/inbox/${encodeURIComponent(conversationId)}/${encodeURIComponent(filename)}`,
    { credentials: "include", signal },
  );
  if (!res.ok) {
    throw new Error(`Attachment fetch failed (${res.status})`);
  }
  const blob = await res.blob();
  return URL.createObjectURL(blob);
}

export function formatBytes(n: number): string {
  if (n <= 0) return "0 B";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(1)} GB`;
}

export function chipLabel(name: string, maxLength = 28): string {
  if (name.length <= maxLength) return name;
  const dot = name.lastIndexOf(".");
  if (dot < 0 || dot < maxLength - 6) {
    return name.slice(0, maxLength - 1) + "…";
  }
  const ext = name.slice(dot);
  const stem = name.slice(0, maxLength - ext.length - 1);
  return stem + "…" + ext;
}
