// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { CheckCircle2, Eye, EyeOff, KeyRound, Lock } from "lucide-react";
import { apiFetch } from "../api/fetch";
import type { SetupRequestPayload } from "../lib/rehydrate";

type Props = {
  payload?: SetupRequestPayload;
  surface?: "chat" | "settings";
  readOnly?: boolean;
};

type ManagerKeyStatus = {
  mode?: string;
  configured?: boolean;
  setup_required?: boolean;
  can_configure?: boolean;
};

function defaultPayload(): SetupRequestPayload {
  return {
    code: "operator_openai_key_required",
    title: "Connect this computer's manager",
    body: "This connected computer needs your OpenAI API key before the manager can run tasks. Add it once. It stays on this machine.",
    footnote:
      "Connected machines use your OpenAI account directly. VibeCraft hosted credits are not used for these manager calls.",
    primary_label: "Save key",
    settings_path: "/settings?tab=manager",
    fields: [
      {
        name: "openai_key",
        label: "OpenAI API key",
        type: "token",
        placeholder: "sk-...",
        hint: "Use a key from your OpenAI project.",
      },
    ],
  };
}

export function ManagerKeySetupCard({
  payload,
  surface = "chat",
  readOnly = false,
}: Props) {
  const navigate = useNavigate();
  const p = payload ?? defaultPayload();
  const field = p.fields?.[0] ?? defaultPayload().fields![0];
  const [value, setValue] = useState("");
  const [revealed, setRevealed] = useState(false);
  const [saving, setSaving] = useState(false);
  const [savedAt, setSavedAt] = useState<string | null>(p.stored?.at ?? null);
  const [error, setError] = useState<string | null>(null);
  const [status, setStatus] = useState<ManagerKeyStatus | null>(null);

  useEffect(() => {
    let cancelled = false;
    apiFetch("/api/manager-key")
      .then((res) => (res.ok ? res.json() : null))
      .then((data: ManagerKeyStatus | null) => {
        if (cancelled || !data) return;
        setStatus(data);
        if (data.configured) setSavedAt((prev) => prev ?? new Date().toISOString());
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  const isReady = Boolean(savedAt || status?.configured || p.stored);
  const canConfigure = status?.can_configure !== false;
  const shell =
    surface === "chat"
      ? "rounded-2xl rounded-bl-md border border-slate-200/80 bg-white/80 p-4 shadow-sm"
      : "rounded-xl border border-slate-200/60 bg-white/50 p-4";

  const timeLabel = useMemo(() => {
    if (!savedAt) return "";
    const d = new Date(savedAt);
    if (Number.isNaN(d.getTime())) return "";
    return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
  }, [savedAt]);

  async function save() {
    if (saving || !value.trim()) return;
    setSaving(true);
    setError(null);
    try {
      const res = await apiFetch("/api/manager-key", {
        method: "POST",
        body: JSON.stringify({
          openai_key: value.trim(),
          message_id: p.message_id,
        }),
      });
      const data = (await res.json().catch(() => ({}))) as {
        error?: string;
        configured_at?: string;
      };
      if (!res.ok) {
        throw new Error(data.error || "Could not save the key.");
      }
      setSavedAt(data.configured_at || new Date().toISOString());
      setValue("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not save the key.");
    } finally {
      setSaving(false);
    }
  }

  function openSettings() {
    navigate(p.settings_path || "/settings?tab=manager");
  }

  if (isReady) {
    return (
      <div className={shell} aria-live="polite">
        <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-emerald-700">
          <CheckCircle2 className="h-3 w-3" aria-hidden="true" />
          Manager ready
        </div>
        <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-950">
          This computer can run manager tasks now.
        </p>
        <p className="mt-2 text-[12px] text-slate-500">
          The key is stored on this computer and is never shown to the agent.
          {timeLabel ? ` Saved at ${timeLabel}.` : ""}
        </p>
      </div>
    );
  }

  return (
    <div className={shell}>
      <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
        <Lock className="h-3 w-3" aria-hidden="true" />
        Setup required
      </div>

      <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-950">
        {p.title}
      </p>
      <p className="mt-1 text-[13px] leading-relaxed text-slate-500">{p.body}</p>

      {canConfigure && !readOnly ? (
        <div className="mt-3">
          <label htmlFor="manager-openai-key" className="text-xs font-medium text-slate-600">
            {field.label}
          </label>
          <div className="relative mt-1">
            <input
              id="manager-openai-key"
              type={revealed ? "text" : "password"}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder={field.placeholder || "sk-..."}
              disabled={saving}
              autoComplete="off"
              spellCheck={false}
              className="w-full rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 pr-10 text-sm text-slate-700 outline-none transition focus:border-slate-300 disabled:opacity-60"
            />
            <button
              type="button"
              onClick={() => setRevealed((v) => !v)}
              aria-label={revealed ? "Hide value" : "Show value"}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600"
            >
              {revealed ? (
                <EyeOff className="h-4 w-4" aria-hidden="true" />
              ) : (
                <Eye className="h-4 w-4" aria-hidden="true" />
              )}
            </button>
          </div>
          {field.hint && <p className="mt-1 text-[11px] text-slate-400">{field.hint}</p>}
        </div>
      ) : (
        <p className="mt-3 rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 text-xs text-slate-500">
          Only an operator with control access can complete this setup.
        </p>
      )}

      {p.footnote && <p className="mt-3 text-[11px] text-slate-400">{p.footnote}</p>}
      {error && (
        <p className="mt-3 text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      <div className="mt-4 flex flex-wrap items-center justify-end gap-2">
        {surface === "chat" && (
          <button
            type="button"
            onClick={openSettings}
            className="rounded-full px-4 py-1.5 text-xs font-medium text-slate-500 hover:text-slate-700"
          >
            Open Settings
          </button>
        )}
        {canConfigure && !readOnly && (
          <button
            type="button"
            onClick={save}
            disabled={!value.trim() || saving}
            aria-busy={saving}
            className="inline-flex items-center gap-1.5 rounded-full bg-slate-950 px-4 py-1.5 text-xs font-medium text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20 disabled:opacity-50 disabled:hover:translate-y-0"
          >
            <KeyRound className="h-3 w-3" aria-hidden="true" />
            {saving ? "Saving..." : p.primary_label || "Save key"}
          </button>
        )}
      </div>
    </div>
  );
}
