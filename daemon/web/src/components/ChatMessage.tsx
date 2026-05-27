// SPDX-License-Identifier: Apache-2.0

import { useState } from "react";
import { Streamdown } from "streamdown";
import "streamdown/styles.css";
import { ApprovalCard } from "./ApprovalCard";
import { CredentialCard } from "./CredentialCard";
import { ManagerKeySetupCard } from "./ManagerKeySetupCard";
import { AttachmentImage } from "./AttachmentImage";
import {
  CheckCircle2,
  XCircle,
  AlertCircle,
  ChevronRight,
  FileText,
  Camera,
  Loader2,
} from "lucide-react";
import {
  chipLabel,
  formatBytes,
  isImageMime,
  type Attachment,
} from "../lib/attachments";
import { sanitizeAgentMarkdown } from "../lib/sanitize";
import type { ChatMessage as ChatMessageType } from "../lib/rehydrate";

export function ChatMessage({
  message,
  onApproval,
  conversationId,
  vaultNames,
  snapshotCount,
}: {
  message: ChatMessageType;
  onApproval: (messageId: string, action: string) => void;
  conversationId: string | null;
  vaultNames: string[];
  snapshotCount?: number;
}) {
  const [previewAttachment, setPreviewAttachment] = useState<string | null>(
    null,
  );

  const isUser = message.role === "user";
  const isSystem =
    message.role === "system" || message.type === "system_notice";
  const parsed = new Date(message.timestamp);
  const time =
    message.timestamp && !isNaN(parsed.getTime())
      ? parsed.toLocaleTimeString("en-US", {
          hour: "numeric",
          minute: "2-digit",
        })
      : "";

  if (isSystem) {
    return (
      <div className="flex justify-center" role="status">
        <div className="flex items-center gap-2 text-[11px] text-slate-400">
          <span className="h-px w-8 bg-slate-200/80" aria-hidden="true" />
          <span>{message.content}</span>
          {time && (
            <>
              <span aria-hidden="true">·</span>
              <span>{time}</span>
            </>
          )}
          <span className="h-px w-8 bg-slate-200/80" aria-hidden="true" />
        </div>
      </div>
    );
  }

  if (isUser) {
    const attachments = message.attachments ?? [];
    const hideText =
      attachments.length === 1 &&
      (message.content === attachments[0].original ||
        message.content === attachments[0].name);
    return (
      <div className="flex justify-end">
        <div className="max-w-[80%] space-y-2">
          {attachments.length > 0 && (
            <UserAttachments
              attachments={attachments}
              conversationId={conversationId}
              onPreview={(name) => setPreviewAttachment(name)}
            />
          )}
          {message.content && !hideText && (
            <div className="rounded-2xl rounded-br-md bg-slate-950 px-4 py-2.5 text-sm text-white">
              {message.content}
            </div>
          )}
          <div className="text-right text-[10px] text-slate-400">{time}</div>
        </div>

        {previewAttachment && conversationId && (
          <AttachmentPreviewModal
            conversationId={conversationId}
            filename={previewAttachment}
            onClose={() => setPreviewAttachment(null)}
          />
        )}
      </div>
    );
  }

  return (
    <div className="flex justify-start">
      <div className="max-w-[85%]">
        {message.type === "text" && message.content && (
          <div className="chat-markdown rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/50 px-4 py-2.5 text-sm text-slate-700">
            <Streamdown>{sanitizeAgentMarkdown(message.content)}</Streamdown>
          </div>
        )}

        {message.type === "approval" && (
          <ApprovalCard
            title={message.approvalTitle}
            reason={message.approvalReason}
            command={message.approvalCommand}
            description={message.description || message.content}
            imageUrl={message.imageUrl}
            actions={message.actions || []}
            onAction={(action) => onApproval(message.id, action)}
            severity={message.approvalSeverity}
            friction={message.approvalFriction}
            typeTarget={message.approvalTypeTarget}
            resolution={message.approvalResolution}
            expiredReason={message.approvalExpiredReason}
          />
        )}

        {message.type === "auto_approved" && (
          <AutoApprovedPill
            title={message.content}
            reason={message.autoApproveReason}
            command={message.autoApproveCommand}
          />
        )}

        {message.type === "credential_request" && message.credentialRequest && (
          <CredentialCard
            taskId={
              message.credentialRequest.task_id || message.taskId || ""
            }
            payload={message.credentialRequest}
            isUpdate={message.credentialRequest.fields.some((f) =>
              vaultNames.includes(f.name),
            )}
          />
        )}

        {message.type === "setup_request" && message.setupRequest && (
          <ManagerKeySetupCard payload={message.setupRequest} surface="chat" />
        )}

        {message.type === "activity_summary" && (
          <ActivitySummaryPill
            content={message.content}
            snapshotCount={snapshotCount}
          />
        )}

        <div className="mt-1 text-[10px] text-slate-400">{time}</div>
      </div>
    </div>
  );
}

function UserAttachments({
  attachments,
  conversationId,
  onPreview,
}: {
  attachments: Attachment[];
  conversationId: string | null;
  onPreview: (filename: string) => void;
}) {
  const images = attachments.filter((a) => isImageMime(a.mime));
  const files = attachments.filter((a) => !isImageMime(a.mime));

  return (
    <div className="flex flex-col items-end gap-2">
      {images.length > 0 && conversationId && (
        <div className="flex flex-wrap justify-end gap-2">
          {images.map((att) => (
            <AttachmentImage
              key={att.name}
              conversationId={conversationId}
              filename={att.name}
              alt={att.original || att.name}
              onClick={() => onPreview(att.name)}
              className="h-28 w-40"
            />
          ))}
        </div>
      )}
      {files.length > 0 && (
        <div className="flex flex-wrap justify-end gap-1.5">
          {files.map((att) => (
            <div
              key={att.name}
              className="flex items-center gap-2 rounded-lg border border-slate-200/60 bg-white/70 py-1.5 pl-2 pr-3 text-[11px] text-slate-600 shadow-sm"
            >
              <div className="flex h-6 w-6 shrink-0 items-center justify-center rounded bg-slate-100 text-slate-400">
                <FileText className="h-3.5 w-3.5" aria-hidden="true" />
              </div>
              <div className="flex min-w-0 flex-col leading-tight">
                <span className="truncate font-medium text-slate-700">
                  {chipLabel(att.original || att.name, 28)}
                </span>
                <span className="text-[10px] text-slate-400">
                  {formatBytes(att.size)}
                </span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function AttachmentPreviewModal({
  conversationId,
  filename,
  onClose,
}: {
  conversationId: string;
  filename: string;
  onClose: () => void;
}) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-slate-950/70 p-4 animate-fade-in"
      role="dialog"
      aria-modal="true"
      aria-label="Attachment preview"
      onClick={onClose}
    >
      <div
        className="relative max-h-full max-w-5xl"
        onClick={(e) => e.stopPropagation()}
      >
        <AttachmentImage
          conversationId={conversationId}
          filename={filename}
          alt="Attachment full view"
          className="max-h-[85vh] max-w-full"
        />
        <button
          type="button"
          onClick={onClose}
          aria-label="Close preview"
          className="absolute -right-3 -top-3 flex h-8 w-8 items-center justify-center rounded-full bg-white text-slate-700 shadow-lg transition hover:-translate-y-0.5 hover:shadow-xl"
        >
          <XCircle className="h-5 w-5" aria-hidden="true" />
        </button>
      </div>
    </div>
  );
}

type ParsedActivity = {
  status: string;
  duration: string;
  resultLine: string;
  currentLine: string;
  lastActivity: string;
  whatIDid: string;
  notes: string;
  stepCount: number | null;
};

function parseActivityMarkdown(raw: string): ParsedActivity {
  let status = "";
  let duration = "";
  let resultLine = "";
  let currentLine = "";
  let lastActivity = "";
  let stepCount: number | null = null;

  const stepMatch = raw.match(/<!--\s*steps:(\d+)\s*-->/);
  if (stepMatch) {
    stepCount = Number.parseInt(stepMatch[1], 10);
  }
  const stripped = raw.replace(/<!--\s*steps:\d+\s*-->\s*/g, "");

  const lines = stripped.split("\n");
  let inWhatIDid = false;
  let inNotes = false;
  const whatLines: string[] = [];
  const notesLines: string[] = [];

  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed.startsWith("Status:")) {
      status = trimmed.slice(7).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Duration:")) {
      duration = trimmed.slice(9).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Result:")) {
      resultLine = trimmed.slice(7).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Current:")) {
      currentLine = trimmed.slice(8).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Last activity:")) {
      lastActivity = trimmed.slice(14).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.toLowerCase().startsWith("what i did")) {
      inWhatIDid = true;
      inNotes = false;
    } else if (trimmed.startsWith("Notes:")) {
      inWhatIDid = false;
      inNotes = true;
      const rest = trimmed.slice(6).trim();
      if (rest) notesLines.push(rest);
    } else if (inWhatIDid) {
      whatLines.push(line);
    } else if (inNotes) {
      notesLines.push(line);
    }
  }

  return {
    status,
    duration,
    resultLine,
    currentLine,
    lastActivity,
    whatIDid: whatLines.join("\n").trim(),
    notes: notesLines.join("\n").trim(),
    stepCount,
  };
}

function statusIcon(status: string) {
  const lower = status.toLowerCase();
  if (lower === "running") {
    return <Loader2 className="h-3.5 w-3.5 animate-spin text-slate-500" aria-hidden="true" />;
  }
  if (lower === "completed") {
    return <CheckCircle2 className="h-3.5 w-3.5 text-emerald-600" aria-hidden="true" />;
  }
  if (lower === "failed") {
    return <XCircle className="h-3.5 w-3.5 text-red-500" aria-hidden="true" />;
  }
  return <AlertCircle className="h-3.5 w-3.5 text-amber-500" aria-hidden="true" />;
}

function statusLabel(status: string) {
  const lower = status.toLowerCase();
  if (lower === "running") return "Working";
  if (lower === "completed") return "Task done";
  if (lower === "failed") return "Failed";
  if (lower === "cancelled") return "Cancelled";
  return status || "Task done";
}

function AutoApprovedPill({
  title,
  reason,
  command,
}: {
  title: string;
  reason?: string;
  command?: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const hasDetail = Boolean(reason || command);

  return (
    <div className="rounded-xl border border-slate-200/60 bg-white/40 px-3 py-1.5 text-[12px] text-slate-500">
      <button
        type="button"
        onClick={() => hasDetail && setExpanded((v) => !v)}
        className={`flex w-full items-center gap-1.5 text-left ${
          hasDetail ? "cursor-pointer hover:text-slate-700" : "cursor-default"
        }`}
        aria-expanded={hasDetail ? expanded : undefined}
        disabled={!hasDetail}
      >
        <CheckCircle2 className="h-3 w-3 shrink-0 text-emerald-500" aria-hidden="true" />
        <span className="font-medium text-slate-600">Auto-approved</span>
        <span className="text-slate-400" aria-hidden="true">·</span>
        <span className="truncate">{title}</span>
        {hasDetail && (
          <ChevronRight
            className={`ml-auto h-3 w-3 shrink-0 transition-transform ${expanded ? "rotate-90" : ""}`}
            aria-hidden="true"
          />
        )}
      </button>
      {expanded && (
        <div className="mt-2 space-y-2 border-t border-slate-200/60 pt-2">
          {reason && <p className="text-[11px] text-slate-500">{reason}</p>}
          {command && (
            <pre className="overflow-x-auto rounded-lg border border-slate-200/60 bg-slate-50 px-2.5 py-1.5 font-mono text-[11px] leading-snug text-slate-700">
              {command}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

function ActivitySummaryPill({
  content,
  snapshotCount,
}: {
  content: string;
  snapshotCount?: number;
}) {
  const [expanded, setExpanded] = useState(false);
  const parsed = parseActivityMarkdown(content);

  return (
    <div className="rounded-2xl border border-slate-200/60 bg-white/70 px-4 py-3 text-sm text-slate-700 shadow-sm">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-slate-500">
        <span className="inline-flex items-center gap-1 font-medium text-slate-700">
          {statusIcon(parsed.status)}
          {statusLabel(parsed.status)}
        </span>
        {parsed.duration && (
          <span className="text-slate-400" aria-hidden="true">·</span>
        )}
        {parsed.duration && <span>{parsed.duration}</span>}
        {parsed.stepCount !== null && parsed.stepCount > 0 && (
          <>
            <span className="text-slate-400" aria-hidden="true">·</span>
            <span>{parsed.stepCount} steps</span>
          </>
        )}
        {typeof snapshotCount === "number" && snapshotCount > 0 && (
          <>
            <span className="text-slate-400" aria-hidden="true">·</span>
            <span className="inline-flex items-center gap-1">
              <Camera className="h-3 w-3" aria-hidden="true" />
              {snapshotCount} snapshot{snapshotCount === 1 ? "" : "s"}
            </span>
          </>
        )}
      </div>
      {(parsed.whatIDid || parsed.notes) && (
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="mt-2 inline-flex items-center gap-1 text-xs font-medium text-slate-500 transition-colors hover:text-slate-700"
          aria-expanded={expanded}
        >
          <ChevronRight
            className={`h-3 w-3 transition-transform ${expanded ? "rotate-90" : ""}`}
            aria-hidden="true"
          />
          {expanded ? "Hide details" : "See what I did"}
        </button>
      )}
      {(parsed.currentLine || parsed.lastActivity) && (
        <div className="mt-3 space-y-1.5 border-t border-slate-200/60 pt-2">
          {parsed.currentLine && (
            <div className="text-xs leading-relaxed text-slate-700">
              <span className="font-medium text-slate-500">Current: </span>
              {parsed.currentLine}
            </div>
          )}
          {parsed.lastActivity && (
            <div className="text-[11px] leading-relaxed text-slate-500">
              <span className="font-medium">Evidence: </span>
              <code className="break-words rounded bg-slate-100 px-1 py-0.5 font-mono text-[10.5px] text-slate-600">
                {parsed.lastActivity}
              </code>
            </div>
          )}
        </div>
      )}
      {expanded && (
        <div className="mt-2 space-y-2 border-t border-slate-200/60 pt-2">
          {parsed.whatIDid && (
            <div className="chat-markdown text-xs text-slate-600">
              <Streamdown>{sanitizeAgentMarkdown(parsed.whatIDid)}</Streamdown>
            </div>
          )}
          {parsed.notes && (
            <div className="text-xs italic text-slate-500">
              <span className="font-medium">Notes: </span>
              {parsed.notes}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
