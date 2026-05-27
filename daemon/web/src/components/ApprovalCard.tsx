// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { ChevronRight, AlertTriangle, Check, Clock, X } from "lucide-react";

export type ApprovalAction = { label: string; action: "approve" | "edit" | "cancel" };
export type ApprovalSeverity = "escalate" | "soft_deny";
export type ApprovalFriction = "none" | "delay" | "type_to_confirm";
// "expired" is the terminal state when the card was closed without an
// answer — 24h timeout, parent task ended, or the customer redirected
// attention to another conversation. Rendered with a distinct pill so
// the user sees the card resolved, not stuck.
export type ApprovalResolution = "approved" | "denied" | "decided" | "expired";

const OVERRIDE_DELAY_SECONDS = 3;

export function ApprovalCard({
  title,
  reason,
  command,
  description,
  imageUrl,
  actions,
  onAction,
  severity = "escalate",
  friction = "none",
  typeTarget,
  resolution,
  expiredReason,
}: {
  title?: string;
  reason?: string;
  command?: string;
  description?: string;
  imageUrl?: string;
  actions: ApprovalAction[];
  onAction: (action: string) => void;
  severity?: ApprovalSeverity;
  friction?: ApprovalFriction;
  typeTarget?: string;
  resolution?: ApprovalResolution;
  expiredReason?: string;
}) {
  const [showCommand, setShowCommand] = useState(false);
  const [typed, setTyped] = useState("");
  const [secondsLeft, setSecondsLeft] = useState(
    friction === "delay" ? OVERRIDE_DELAY_SECONDS : 0,
  );

  const isSoftDeny = severity === "soft_deny";
  const hasStructured = Boolean(title);
  const isResolved = Boolean(resolution);

  useEffect(() => {
    if (isResolved) return;
    if (friction !== "delay" || secondsLeft <= 0) return;
    const t = setTimeout(() => setSecondsLeft((s) => s - 1), 1000);
    return () => clearTimeout(t);
  }, [friction, secondsLeft, isResolved]);

  if (isResolved) {
    const isApproved = resolution === "approved";
    const isDenied = resolution === "denied";
    const isExpired = resolution === "expired";
    const pillClasses = isApproved
      ? "bg-emerald-50 text-emerald-700 ring-emerald-200/60"
      : isDenied
      ? "bg-rose-50 text-rose-700 ring-rose-200/60"
      : isExpired
      ? "bg-amber-50 text-amber-700 ring-amber-200/60"
      : "bg-slate-100 text-slate-600 ring-slate-200/60";
    const PillIcon = isApproved
      ? Check
      : isDenied
      ? X
      : isExpired
      ? Clock
      : null;
    const pillLabel = isApproved
      ? "Approved"
      : isDenied
      ? "Denied"
      : isExpired
      ? "Closed"
      : "Resolved";
    const expiredCopy = isExpired ? humaniseExpiredReason(expiredReason) : "";

    return (
      <div className="rounded-2xl rounded-bl-md border border-slate-200/60 bg-white/50 px-4 py-3 shadow-sm">
        <div className="flex flex-wrap items-center gap-2">
          <span
            className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-widest ring-1 ${pillClasses}`}
          >
            {PillIcon ? <PillIcon className="h-2.5 w-2.5" aria-hidden="true" /> : null}
            {pillLabel}
          </span>
          {hasStructured ? (
            <p className="text-[13px] font-medium text-slate-700">{title}</p>
          ) : (
            <p className="text-[13px] text-slate-500">Approval</p>
          )}
        </div>
        {isExpired && expiredCopy && (
          <p className="mt-1.5 text-[12px] text-slate-500">{expiredCopy}</p>
        )}
        {hasStructured && command && (
          <div className="mt-2">
            <button
              type="button"
              onClick={() => setShowCommand((v) => !v)}
              className="inline-flex items-center gap-1 text-[11px] font-medium text-slate-400 transition-colors hover:text-slate-600"
              aria-expanded={showCommand}
            >
              <ChevronRight
                className={`h-3 w-3 transition-transform ${showCommand ? "rotate-90" : ""}`}
                aria-hidden="true"
              />
              {showCommand ? "Hide command" : "Show command"}
            </button>
            {showCommand && (
              <pre className="mt-1.5 overflow-x-auto rounded-lg border border-slate-200/60 bg-slate-50/80 px-3 py-2 font-mono text-[11px] leading-snug text-slate-600">
                {command}
              </pre>
            )}
          </div>
        )}
      </div>
    );
  }

  const overrideReady =
    friction === "none" ||
    (friction === "delay" && secondsLeft <= 0) ||
    (friction === "type_to_confirm" && Boolean(typeTarget) && typed === typeTarget);

  const primaryLabel = (a: ApprovalAction) => {
    if (a.action === "approve" && isSoftDeny) {
      if (friction === "delay" && secondsLeft > 0) return `Do it anyway (${secondsLeft})`;
      return "Do it anyway";
    }
    return a.label;
  };

  const containerClasses = isSoftDeny
    ? "rounded-2xl rounded-bl-md border border-red-200/80 bg-red-50/40 p-4 shadow-sm"
    : "rounded-2xl rounded-bl-md border border-slate-200/80 bg-white/80 p-4 shadow-sm";

  const labelClasses = isSoftDeny
    ? "text-[10px] font-medium uppercase tracking-widest text-red-700"
    : "text-[10px] font-medium uppercase tracking-widest text-slate-400";

  const titleClasses = isSoftDeny
    ? "mt-1.5 font-poppins text-[15px] font-medium text-red-950"
    : "mt-1.5 font-poppins text-[15px] font-medium text-slate-950";

  const reasonClasses = isSoftDeny
    ? "mt-1 text-[13px] text-red-900/80"
    : "mt-1 text-[13px] text-slate-500";

  const actionStyles = {
    approve: isSoftDeny
      ? "bg-red-600 text-white hover:-translate-y-0.5 hover:shadow-lg hover:shadow-red-600/20 disabled:opacity-40 disabled:translate-y-0 disabled:shadow-none disabled:cursor-not-allowed"
      : "bg-slate-950 text-white hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20",
    edit: "bg-slate-100 text-slate-700 hover:bg-slate-200",
    cancel: isSoftDeny
      ? "bg-white text-red-700 border border-red-200 hover:bg-red-50"
      : "bg-white text-slate-700 border border-slate-200 hover:bg-slate-50",
  };

  return (
    <div className={containerClasses}>
      <div className={labelClasses}>
        {isSoftDeny ? (
          <span className="inline-flex items-center gap-1.5">
            <AlertTriangle className="h-3 w-3" aria-hidden="true" />
            We recommend against this
          </span>
        ) : (
          "Approval needed"
        )}
      </div>

      {hasStructured ? (
        <>
          <p className={titleClasses}>{title}</p>
          {reason && <p className={reasonClasses}>{reason}</p>}
          {command && (
            <div className="mt-3">
              <button
                type="button"
                onClick={() => setShowCommand((v) => !v)}
                className={
                  isSoftDeny
                    ? "inline-flex items-center gap-1 text-xs font-medium text-red-700 transition-colors hover:text-red-900"
                    : "inline-flex items-center gap-1 text-xs font-medium text-slate-500 transition-colors hover:text-slate-700"
                }
                aria-expanded={showCommand}
              >
                <ChevronRight
                  className={`h-3 w-3 transition-transform ${showCommand ? "rotate-90" : ""}`}
                  aria-hidden="true"
                />
                {showCommand ? "Hide command" : "Show command"}
              </button>
              {showCommand && (
                <pre className="mt-2 overflow-x-auto rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 font-mono text-[12px] leading-snug text-slate-700">
                  {command}
                </pre>
              )}
            </div>
          )}
        </>
      ) : (
        <p className="mt-1.5 whitespace-pre-wrap text-sm text-slate-700">{description}</p>
      )}

      {imageUrl && (
        <div className="mt-3 overflow-hidden rounded-xl border border-slate-200/60">
          <img src={imageUrl} alt="Preview" className="max-h-48 w-auto" />
        </div>
      )}

      {isSoftDeny && friction === "type_to_confirm" && typeTarget && (
        <div className="mt-3">
          <label className="block text-[12px] text-red-900/80">
            To proceed, type <span className="font-mono text-red-950">{typeTarget}</span> below:
          </label>
          <input
            type="text"
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            spellCheck={false}
            autoComplete="off"
            autoCorrect="off"
            className="mt-1.5 w-full rounded-lg border border-red-200 bg-white px-3 py-1.5 font-mono text-[13px] text-slate-900 outline-none focus:border-red-400 focus:ring-1 focus:ring-red-200"
            placeholder={typeTarget}
            aria-label={`Type ${typeTarget} to confirm`}
          />
        </div>
      )}

      <div className="mt-4 flex flex-wrap gap-2">
        {actions.map((action) => {
          const isPrimary = action.action === "approve";
          const disabled = isPrimary && isSoftDeny && !overrideReady;
          return (
            <button
              key={action.action}
              onClick={() => !disabled && onAction(action.action)}
              disabled={disabled}
              className={`rounded-full px-4 py-1.5 text-xs font-medium transition ${
                actionStyles[action.action]
              }`}
            >
              {primaryLabel(action)}
            </button>
          );
        })}
      </div>
    </div>
  );
}

// humaniseExpiredReason turns the daemon's machine reason string into
// the one-line sentence shown under the "Closed" pill. The daemon side
// keeps the reasons terse and stable (markCredentialCardExpired /
// markApprovalCardExpired); this maps each known reason to user-facing
// copy and falls back to a neutral default for unrecognised values.
function humaniseExpiredReason(raw?: string): string {
  if (!raw) return "This approval is no longer active.";
  const lower = raw.toLowerCase();
  if (lower.includes("redirect")) {
    return "You started a new task in another conversation, so this approval was closed.";
  }
  if (lower.includes("24h") || lower.includes("24 h")) {
    return "Closed automatically after 24 hours without a response.";
  }
  if (lower.includes("task ended")) {
    return "The task this belonged to ended before you answered.";
  }
  return "This approval is no longer active.";
}
