// SPDX-License-Identifier: Apache-2.0

import { useState, useRef, useEffect, useCallback } from "react";
import {
  ArrowUp,
  Square,
  Paperclip,
  X,
  Image as ImageIcon,
  FileText,
} from "lucide-react";
import {
  MAX_COMPOSER_ATTACHMENTS,
  MAX_UPLOAD_FILE_BYTES,
  chipLabel,
  formatBytes,
  isImageMime,
  uploadFile,
  type Attachment,
  type ComposerAttachment,
} from "../lib/attachments";

export function ChatInput({
  onSend,
  onStop,
  disabled,
  isWorking,
  stopping,
  conversationId,
  ensureConversationId,
  prefill,
}: {
  onSend: (message: string, attachments: Attachment[]) => void;
  onStop?: () => void;
  disabled?: boolean;
  isWorking?: boolean;
  stopping?: boolean;
  conversationId: string | null;
  ensureConversationId: () => string;
  // A starter prompt the user tapped in the empty state. Bumping `nonce`
  // refills the composer (and refocuses) even when the text repeats. Tapping
  // a starter never sends — the user reviews and hits send themselves.
  prefill?: { text: string; nonce: number };
}) {
  const [message, setMessage] = useState("");
  const [attachments, setAttachments] = useState<ComposerAttachment[]>([]);
  const [dragDepth, setDragDepth] = useState(0);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const hasText = message.trim().length > 0;
  const hasAttachments = attachments.length > 0;
  const uploading = attachments.some((a) => a.status === "uploading");
  const hasErrors = attachments.some((a) => a.status === "error");
  const canStop = Boolean(isWorking && onStop && !stopping);
  const mode: "send" | "stop" =
    canStop && !hasText && !hasAttachments ? "stop" : "send";
  const willInterrupt = Boolean(isWorking && (hasText || hasAttachments));
  const canSend = (hasText || hasAttachments) && !uploading && !hasErrors;

  useEffect(() => {
    return () => {
      setAttachments((prev) => {
        for (const a of prev) {
          if (a.previewUrl) URL.revokeObjectURL(a.previewUrl);
        }
        return [];
      });
    };
  }, []);

  const startUploads = useCallback(
    (files: File[]) => {
      if (files.length === 0) return;
      const convID = conversationId ?? ensureConversationId();

      setAttachments((prev) => {
        const remaining = MAX_COMPOSER_ATTACHMENTS - prev.length;
        if (remaining <= 0) return prev;
        const accepted = files.slice(0, remaining);

        const newItems: ComposerAttachment[] = accepted.map((file) => {
          const localId = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
          const previewUrl = file.type.startsWith("image/")
            ? URL.createObjectURL(file)
            : undefined;

          if (file.size > MAX_UPLOAD_FILE_BYTES) {
            return {
              localId,
              file,
              previewUrl,
              status: "error",
              error: `Too large (max ${formatBytes(MAX_UPLOAD_FILE_BYTES)})`,
            };
          }

          uploadFile({
            conversationId: convID,
            file,
            onProgress: (fraction) => {
              setAttachments((cur) =>
                cur.map((a) =>
                  a.localId === localId ? { ...a, progress: fraction } : a,
                ),
              );
            },
          })
            .then((attachment) => {
              setAttachments((cur) =>
                cur.map((a) =>
                  a.localId === localId
                    ? { ...a, status: "ready", attachment, progress: 1 }
                    : a,
                ),
              );
            })
            .catch((err: Error) => {
              setAttachments((cur) =>
                cur.map((a) =>
                  a.localId === localId
                    ? {
                        ...a,
                        status: "error",
                        error: err.message || "Upload failed",
                      }
                    : a,
                ),
              );
            });

          return {
            localId,
            file,
            previewUrl,
            status: "uploading",
            progress: 0,
          };
        });

        return [...prev, ...newItems];
      });
    },
    [conversationId, ensureConversationId],
  );

  const removeAttachment = useCallback((localId: string) => {
    setAttachments((prev) => {
      const target = prev.find((a) => a.localId === localId);
      if (target?.previewUrl) URL.revokeObjectURL(target.previewUrl);
      return prev.filter((a) => a.localId !== localId);
    });
  }, []);

  function handleSubmit() {
    const trimmed = message.trim();
    if (!canSend || disabled) return;
    const readyAttachments = attachments
      .filter((a) => a.status === "ready" && a.attachment)
      .map((a) => a.attachment as Attachment);
    onSend(trimmed, readyAttachments);
    setMessage("");
    for (const a of attachments) {
      if (a.previewUrl) URL.revokeObjectURL(a.previewUrl);
    }
    setAttachments([]);
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
    }
  }

  function handleStop() {
    if (onStop && canStop) onStop();
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key === ".") {
      e.preventDefault();
      handleStop();
      return;
    }
    if (e.key === "Escape" && canStop && !hasText && !hasAttachments) {
      e.preventDefault();
      handleStop();
    }
  }

  function handlePaste(e: React.ClipboardEvent<HTMLTextAreaElement>) {
    if (!e.clipboardData || e.clipboardData.files.length === 0) return;
    const files: File[] = [];
    for (let i = 0; i < e.clipboardData.files.length; i++) {
      const f = e.clipboardData.files.item(i);
      if (f) files.push(f);
    }
    if (files.length > 0) {
      e.preventDefault();
      startUploads(files);
    }
  }

  function pickFromFiles(e: React.ChangeEvent<HTMLInputElement>) {
    const list = e.target.files;
    if (!list) return;
    const files: File[] = [];
    for (let i = 0; i < list.length; i++) {
      const f = list.item(i);
      if (f) files.push(f);
    }
    startUploads(files);
    e.target.value = "";
  }

  function onDragEnter(e: React.DragEvent) {
    if (!e.dataTransfer?.types?.includes("Files")) return;
    e.preventDefault();
    setDragDepth((d) => d + 1);
  }
  function onDragOver(e: React.DragEvent) {
    if (!e.dataTransfer?.types?.includes("Files")) return;
    e.preventDefault();
  }
  function onDragLeave(e: React.DragEvent) {
    if (!e.dataTransfer?.types?.includes("Files")) return;
    e.preventDefault();
    setDragDepth((d) => Math.max(0, d - 1));
  }
  function onDrop(e: React.DragEvent) {
    if (!e.dataTransfer?.files || e.dataTransfer.files.length === 0) return;
    e.preventDefault();
    setDragDepth(0);
    const files: File[] = [];
    for (let i = 0; i < e.dataTransfer.files.length; i++) {
      const f = e.dataTransfer.files.item(i);
      if (f) files.push(f);
    }
    startUploads(files);
  }

  useEffect(() => {
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height = `${Math.min(textareaRef.current.scrollHeight, 160)}px`;
    }
  }, [message]);

  // Fill (don't send) the composer when a starter prompt is tapped, then
  // focus and drop the cursor at the end so it's ready to edit or send.
  useEffect(() => {
    if (!prefill?.text) return;
    setMessage(prefill.text);
    requestAnimationFrame(() => {
      const el = textareaRef.current;
      if (el) {
        el.focus();
        el.setSelectionRange(el.value.length, el.value.length);
      }
    });
  }, [prefill]);

  useEffect(() => {
    if (!canStop) return;
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === ".") {
        e.preventDefault();
        handleStop();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canStop]);

  const sendDisabled = disabled || !canSend;
  const buttonLabel =
    mode === "stop"
      ? stopping
        ? "Stopping..."
        : "Stop task (Cmd/Ctrl + .)"
      : uploading
        ? "Waiting for upload..."
        : willInterrupt
          ? "Interrupt and send"
          : "Send message";

  const dragActive = dragDepth > 0;
  const attachDisabled = disabled;
  const attachmentsAtLimit = attachments.length >= MAX_COMPOSER_ATTACHMENTS;

  return (
    <div className="border-t border-slate-200/60 px-4 pb-3 pt-2 sm:px-6">
      <div className="mx-auto max-w-2xl">
        {willInterrupt && (
          <p
            className="mb-1.5 text-center text-[11px] text-slate-400"
            role="status"
          >
            Sending will interrupt the current task
          </p>
        )}
        <div className="flex items-end gap-2">
          <div
            className={`relative flex-1 rounded-2xl border bg-white/50 transition ${
              dragActive
                ? "border-slate-400 ring-2 ring-slate-300/50"
                : "border-slate-200/60"
            }`}
            onDragEnter={onDragEnter}
            onDragOver={onDragOver}
            onDragLeave={onDragLeave}
            onDrop={onDrop}
          >
            {hasAttachments && (
              <div className="flex flex-wrap gap-1.5 px-3 pt-2.5">
                {attachments.map((a) => (
                  <AttachmentChip
                    key={a.localId}
                    att={a}
                    onRemove={() => removeAttachment(a.localId)}
                  />
                ))}
              </div>
            )}

            {dragActive && !hasAttachments && (
              <div
                className="pointer-events-none absolute inset-0 flex items-center justify-center rounded-2xl text-xs font-medium text-slate-500"
                aria-hidden="true"
              >
                Drop to attach
              </div>
            )}

            <label htmlFor="chat-input" className="sr-only">
              Message
            </label>
            <textarea
              id="chat-input"
              ref={textareaRef}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              onKeyDown={handleKeyDown}
              onPaste={handlePaste}
              placeholder={
                disabled
                  ? "Session locked. Viewing only."
                  : dragActive
                    ? ""
                    : isWorking
                      ? "Interrupt with new instructions..."
                      : hasAttachments
                        ? "Add a message (or send as-is)…"
                        : "Type a message, paste an image, or drop files…"
              }
              disabled={disabled}
              rows={1}
              className="w-full resize-none bg-transparent px-4 py-3 pl-10 pr-4 text-sm text-slate-700 placeholder-slate-400 outline-none disabled:opacity-50"
              style={{ minHeight: "44px" }}
            />

            <input
              ref={fileInputRef}
              type="file"
              multiple
              hidden
              onChange={pickFromFiles}
            />
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              disabled={attachDisabled || attachmentsAtLimit}
              aria-label={
                attachmentsAtLimit
                  ? `Maximum ${MAX_COMPOSER_ATTACHMENTS} files`
                  : "Attach files"
              }
              title={
                attachmentsAtLimit
                  ? `Maximum ${MAX_COMPOSER_ATTACHMENTS} files`
                  : "Attach files"
              }
              className="absolute bottom-2 left-2 flex h-7 w-7 items-center justify-center rounded-full text-slate-400 transition hover:bg-slate-100 hover:text-slate-700 disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent"
            >
              <Paperclip className="h-4 w-4" aria-hidden="true" />
            </button>
          </div>

          {mode === "stop" ? (
            <button
              type="button"
              onClick={handleStop}
              disabled={stopping}
              aria-label={buttonLabel}
              title={buttonLabel}
              className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-slate-950 text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20 disabled:opacity-50 disabled:hover:translate-y-0 disabled:hover:shadow-none"
            >
              {stopping ? (
                <span
                  className="h-3 w-3 animate-spin rounded-full border-2 border-white/30 border-t-white"
                  aria-hidden="true"
                />
              ) : (
                <Square className="h-3 w-3 fill-current" aria-hidden="true" />
              )}
            </button>
          ) : (
            <button
              type="button"
              onClick={handleSubmit}
              disabled={sendDisabled}
              aria-label={buttonLabel}
              title={buttonLabel}
              className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-slate-950 text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20 disabled:opacity-30 disabled:hover:translate-y-0 disabled:hover:shadow-none"
            >
              {uploading ? (
                <span
                  className="h-3 w-3 animate-spin rounded-full border-2 border-white/40 border-t-white"
                  aria-hidden="true"
                />
              ) : (
                <ArrowUp className="h-4 w-4" aria-hidden="true" />
              )}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

function AttachmentChip({
  att,
  onRemove,
}: {
  att: ComposerAttachment;
  onRemove: () => void;
}) {
  const label = chipLabel(att.file.name);
  const isImage = isImageMime(att.file.type) && !!att.previewUrl;

  return (
    <div
      className={`group relative flex items-center gap-2 rounded-lg border bg-white/70 py-1 pl-1 pr-2 text-[11px] text-slate-600 transition ${
        att.status === "error"
          ? "border-red-200 text-red-600"
          : "border-slate-200/60"
      }`}
      role="listitem"
    >
      <div className="relative h-7 w-7 shrink-0 overflow-hidden rounded bg-slate-100">
        {isImage ? (
          <img
            src={att.previewUrl}
            alt=""
            className="h-full w-full object-cover"
            draggable={false}
          />
        ) : (
          <div className="flex h-full w-full items-center justify-center text-slate-400">
            {att.file.type.startsWith("image/") ? (
              <ImageIcon className="h-3.5 w-3.5" aria-hidden="true" />
            ) : (
              <FileText className="h-3.5 w-3.5" aria-hidden="true" />
            )}
          </div>
        )}
        {att.status === "uploading" && (
          <div
            className="absolute inset-0 flex items-center justify-center bg-white/70"
            aria-label={`Uploading ${att.file.name}`}
            role="status"
          >
            <span
              className="h-3 w-3 animate-spin rounded-full border-2 border-slate-300 border-t-slate-700"
              aria-hidden="true"
            />
          </div>
        )}
      </div>
      <div className="flex min-w-0 flex-col leading-tight">
        <span className="truncate">{label}</span>
        <span className="text-[10px] text-slate-400">
          {att.status === "error"
            ? att.error
            : att.status === "uploading"
              ? `Uploading${att.progress ? ` ${Math.round(att.progress * 100)}%` : ""}`
              : formatBytes(att.file.size)}
        </span>
      </div>
      <button
        type="button"
        onClick={onRemove}
        aria-label={`Remove ${att.file.name}`}
        className="ml-1 flex h-4 w-4 shrink-0 items-center justify-center rounded-full text-slate-400 transition hover:bg-slate-100 hover:text-slate-700"
      >
        <X className="h-3 w-3" aria-hidden="true" />
      </button>
    </div>
  );
}
