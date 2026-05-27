// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from "react";
import { FileText, Loader2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";

// COMPUTER.md is the per-machine config file shared between the
// customer and the manager (the AI agent on this machine). It lives
// on disk at /home/vibecraft/COMPUTER.md and is editable by both
// sides — the manager updates it via str_replace when the customer
// expresses a durable preference, and the customer can edit it
// directly here.
//
// Design intent: this panel is the *check + control* surface the
// customer described. Reading is as important as editing — they
// should be able to land here and immediately understand what their
// machine is set to do. So the editor is the first thing they see,
// the path + last-saved indicator sits quietly above it, and the
// only chrome below is Save + Discard.

type ComputerMdResponse = {
  content: string;
  path: string;
};

function relativeTime(d: Date): string {
  const seconds = Math.round((Date.now() - d.getTime()) / 1000);
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

export function ComputerMdSettings({ readOnly }: { readOnly: boolean }) {
  const [content, setContent] = useState("");
  const [savedContent, setSavedContent] = useState("");
  const [path, setPath] = useState("/home/vibecraft/COMPUTER.md");
  const [loadState, setLoadState] = useState<"loading" | "ready" | "error">(
    "loading",
  );
  const [saveState, setSaveState] = useState<"idle" | "saving" | "saved">(
    "idle",
  );
  const [error, setError] = useState<string | null>(null);
  const [lastSavedAt, setLastSavedAt] = useState<Date | null>(null);
  // tick: re-render once a minute so the "saved Nm ago" indicator stays current
  // without doing a full data refetch.
  const [, setTick] = useState(0);

  const dirty = content !== savedContent;

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/computer-md", { signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error("Failed to load COMPUTER.md");
        return res.json() as Promise<ComputerMdResponse>;
      })
      .then((data) => {
        setContent(data.content);
        setSavedContent(data.content);
        if (data.path) setPath(data.path);
        setLoadState("ready");
      })
      .catch((err) => {
        if (err?.name === "AbortError") return;
        setError(err?.message ?? "Failed to load COMPUTER.md");
        setLoadState("error");
      });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!lastSavedAt) return;
    const id = window.setInterval(() => setTick((t) => t + 1), 30_000);
    return () => window.clearInterval(id);
  }, [lastSavedAt]);

  const lastSavedLabel = useMemo(() => {
    if (!lastSavedAt) return null;
    return `Saved ${relativeTime(lastSavedAt)}`;
  }, [lastSavedAt]);

  async function handleSave() {
    setSaveState("saving");
    setError(null);
    try {
      const res = await apiFetch("/api/computer-md", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content }),
      });
      if (!res.ok) {
        let message = "Failed to save COMPUTER.md";
        try {
          const body = await res.json();
          if (body?.error) message = String(body.error);
        } catch {
          // ignore — keep generic message
        }
        throw new Error(message);
      }
      setSavedContent(content);
      setLastSavedAt(new Date());
      setSaveState("saved");
      // Snap back to idle so the Save button label doesn't linger.
      window.setTimeout(() => {
        setSaveState((s) => (s === "saved" ? "idle" : s));
      }, 1500);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSaveState("idle");
    }
  }

  function handleDiscard() {
    setContent(savedContent);
    setError(null);
  }

  if (loadState === "loading") {
    return (
      <div className="flex items-center gap-2 py-8 text-sm text-slate-400">
        <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
        Loading machine config…
      </div>
    );
  }

  if (loadState === "error") {
    return (
      <p className="text-sm text-red-500" role="alert">
        {error ?? "Couldn't load COMPUTER.md from this machine."}
      </p>
    );
  }

  const saveDisabled = !dirty || saveState === "saving" || readOnly;

  return (
    <div className="flex h-full flex-col gap-4">
      <div className="space-y-1.5">
        <div className="flex items-center gap-2">
          <FileText
            className="h-4 w-4 text-slate-400"
            aria-hidden="true"
          />
          <h3 className="font-poppins text-sm font-semibold text-slate-950">
            COMPUTER.md
          </h3>
        </div>
        <p className="text-xs leading-relaxed text-slate-500">
          Per-machine config shared between you and the manager. The agent
          reads this at the start of every conversation. It writes here when
          you state a durable preference (&ldquo;always prefer Codex&rdquo;,
          &ldquo;remember I&rsquo;m in Helsinki time&rdquo;). You can edit it
          directly — what&rsquo;s here is what the agent sees.
        </p>
      </div>

      <div className="flex items-center justify-between text-[10px] text-slate-400">
        <code className="font-mono">{path}</code>
        {lastSavedLabel && <span aria-live="polite">{lastSavedLabel}</span>}
      </div>

      <textarea
        value={content}
        onChange={(e) => setContent(e.target.value)}
        disabled={readOnly || saveState === "saving"}
        spellCheck={false}
        aria-label="COMPUTER.md content"
        className="min-h-[480px] flex-1 resize-y rounded-lg border border-slate-200/80 bg-white/70 p-4 font-mono text-xs leading-relaxed text-slate-800 shadow-inner outline-none transition focus:border-slate-400 focus:bg-white disabled:bg-slate-50 disabled:text-slate-400"
      />

      {readOnly && (
        <p className="text-xs text-slate-500">
          You have view-only access on this machine. Ask an operator with
          control access to make changes.
        </p>
      )}

      {error && (
        <p className="text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      <div className="flex items-center justify-between">
        <p className="text-[10px] text-slate-400">
          {dirty
            ? "Unsaved changes — the agent uses the saved version."
            : "In sync with the agent."}
        </p>
        <div className="flex items-center gap-2">
          {dirty && (
            <button
              type="button"
              onClick={handleDiscard}
              disabled={saveState === "saving"}
              className="rounded-full px-3 py-1.5 text-xs text-slate-500 transition hover:bg-slate-100 hover:text-slate-700 disabled:opacity-50"
            >
              Discard
            </button>
          )}
          <button
            type="button"
            onClick={handleSave}
            disabled={saveDisabled}
            className="flex items-center gap-1.5 rounded-full bg-slate-950 px-4 py-1.5 text-xs font-medium text-white transition hover:bg-slate-800 disabled:cursor-not-allowed disabled:bg-slate-300"
          >
            {saveState === "saving" && (
              <Loader2 className="h-3 w-3 animate-spin" aria-hidden="true" />
            )}
            {saveState === "saving"
              ? "Saving"
              : saveState === "saved"
                ? "Saved"
                : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}
