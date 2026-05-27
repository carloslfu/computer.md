// SPDX-License-Identifier: Apache-2.0

import { useMemo, useState } from "react";
import { Eye, EyeOff, Lock, CheckCircle2, CircleSlash } from "lucide-react";
import { apiFetch } from "../api/fetch";
import type {
  CredentialRequestPayload,
  CredentialSpec,
} from "../lib/rehydrate";

type FieldValue = { value: string; reveal: boolean };

function emptyValue(): FieldValue {
  return { value: "", reveal: false };
}

function inputTypeFor(
  spec: CredentialSpec,
  revealed: boolean,
): "text" | "password" | "url" {
  const t = spec.type ?? "text";
  if (t === "password" || t === "token") return revealed ? "text" : "password";
  if (t === "url") return "url";
  return "text";
}

function placeholderFor(spec: CredentialSpec): string {
  const t = spec.type ?? "text";
  switch (t) {
    case "password":
      return "Enter password";
    case "token":
      return "Paste token";
    case "url":
      return "https://";
    default:
      return "";
  }
}

function StoredNameChip({ name }: { name: string }) {
  return (
    <span className="inline-flex items-center rounded-md border border-slate-200/70 bg-slate-50 px-1.5 py-0.5 font-mono text-[11px] text-slate-700">
      <span className="text-slate-400">$</span>
      {name}
    </span>
  );
}

function ExpiredReceipt({
  title,
  reason,
  at,
  fieldNames,
}: {
  title: string;
  reason: string;
  at: string;
  fieldNames: string[];
}) {
  const timeLabel = useMemo(() => {
    if (!at) return "";
    const d = new Date(at);
    if (Number.isNaN(d.getTime())) return "";
    return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
  }, [at]);

  return (
    <div
      className="rounded-2xl rounded-bl-md border border-slate-200/80 bg-white/60 p-4 shadow-sm"
      aria-live="polite"
    >
      <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
        <CircleSlash className="h-3 w-3" aria-hidden="true" />
        Card expired
      </div>
      <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-700">
        {title}
      </p>
      {reason && (
        <p className="mt-1 text-[13px] text-slate-500">{reason}</p>
      )}
      {fieldNames.length > 0 && (
        <div className="mt-2 flex flex-wrap items-center gap-1.5">
          {fieldNames.map((n) => (
            <StoredNameChip key={n} name={n} />
          ))}
          {timeLabel && (
            <span className="text-[10px] text-slate-400">· {timeLabel}</span>
          )}
        </div>
      )}
      <p className="mt-2 text-[12px] text-slate-500">
        Nothing was stored. Start a new message to continue.
      </p>
    </div>
  );
}

function StoredReceipt({
  title,
  names,
  at,
  isUpdate,
}: {
  title: string;
  names: string[];
  at: string;
  isUpdate: boolean;
}) {
  const timeLabel = useMemo(() => {
    if (!at) return "";
    const d = new Date(at);
    if (Number.isNaN(d.getTime())) return "";
    return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit" });
  }, [at]);

  return (
    <div
      className="rounded-2xl rounded-bl-md border border-slate-200/80 bg-white/80 p-4 shadow-sm"
      aria-live="polite"
    >
      <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-emerald-700">
        <CheckCircle2 className="h-3 w-3" aria-hidden="true" />
        {isUpdate ? "Updated in vault" : "Stored in vault"}
      </div>
      <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-950">
        {title}
      </p>
      <div className="mt-2 flex flex-wrap items-center gap-1.5">
        {names.map((n) => (
          <StoredNameChip key={n} name={n} />
        ))}
        {timeLabel && (
          <span className="text-[10px] text-slate-400">· {timeLabel}</span>
        )}
      </div>
      <p className="mt-2 text-[12px] text-slate-500">
        Values are encrypted on your machine. I can reference them by name but
        never see the raw values.
      </p>
    </div>
  );
}

function InputForm({
  payload,
  saving,
  error,
  onChange,
  onToggleReveal,
  onSubmit,
  onDismiss,
  values,
  isUpdate,
}: {
  payload: CredentialRequestPayload;
  saving: boolean;
  error: string | null;
  onChange: (name: string, value: string) => void;
  onToggleReveal: (name: string) => void;
  onSubmit: () => void;
  onDismiss: () => void;
  values: Record<string, FieldValue>;
  isUpdate: boolean;
}) {
  const allFilled = payload.fields.every(
    (f) => (values[f.name]?.value ?? "").trim() !== "",
  );

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter" && allFilled && !saving) {
      e.preventDefault();
      onSubmit();
    }
  }

  return (
    <div className="rounded-2xl rounded-bl-md border border-slate-200/80 bg-white/80 p-4 shadow-sm">
      <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
        <Lock className="h-3 w-3" aria-hidden="true" />
        {isUpdate ? "Update credentials" : "Secure input"}
      </div>

      <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-950">
        {payload.title}
      </p>
      {payload.reason && (
        <p className="mt-1 text-[13px] text-slate-500">{payload.reason}</p>
      )}

      <div className="mt-3 space-y-3">
        {payload.fields.map((spec) => {
          const v = values[spec.name] ?? emptyValue();
          const isSecret = spec.type === "password" || spec.type === "token";
          const inputId = `cred-${payload.tool_use_id ?? "x"}-${spec.name}`;
          return (
            <div key={spec.name}>
              <div className="flex items-baseline justify-between gap-2">
                <label
                  htmlFor={inputId}
                  className="text-xs font-medium text-slate-600"
                >
                  {spec.label}
                </label>
                <span className="font-mono text-[10px] text-slate-400">
                  ${spec.name}
                </span>
              </div>
              <div className="relative mt-1">
                <input
                  id={inputId}
                  type={inputTypeFor(spec, v.reveal)}
                  value={v.value}
                  onChange={(e) => onChange(spec.name, e.target.value)}
                  onKeyDown={onKeyDown}
                  placeholder={placeholderFor(spec)}
                  disabled={saving}
                  autoComplete="off"
                  spellCheck={false}
                  className="w-full rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 pr-10 text-sm text-slate-700 outline-none transition focus:border-slate-300 disabled:opacity-60"
                />
                {isSecret && (
                  <button
                    type="button"
                    onClick={() => onToggleReveal(spec.name)}
                    aria-label={v.reveal ? "Hide value" : "Show value"}
                    className="absolute right-2 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600"
                  >
                    {v.reveal ? (
                      <EyeOff className="h-4 w-4" aria-hidden="true" />
                    ) : (
                      <Eye className="h-4 w-4" aria-hidden="true" />
                    )}
                  </button>
                )}
              </div>
              {spec.hint && (
                <p className="mt-1 text-[11px] text-slate-400">{spec.hint}</p>
              )}
            </div>
          );
        })}
      </div>

      {error && (
        <p className="mt-3 text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
        <p className="text-[11px] text-slate-400">
          Values write directly to the encrypted vault. The agent never sees
          them.
        </p>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={onDismiss}
            disabled={saving}
            className="rounded-full px-4 py-1.5 text-xs font-medium text-slate-500 hover:text-slate-700 disabled:opacity-50"
          >
            Dismiss
          </button>
          <button
            type="button"
            onClick={onSubmit}
            disabled={!allFilled || saving}
            aria-busy={saving}
            className="rounded-full bg-slate-950 px-4 py-1.5 text-xs font-medium text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20 disabled:opacity-50 disabled:hover:translate-y-0"
          >
            {saving
              ? "Storing…"
              : isUpdate
                ? "Update in vault"
                : "Store in vault"}
          </button>
        </div>
      </div>
    </div>
  );
}

export function CredentialCard({
  taskId,
  payload,
  isUpdate,
}: {
  taskId: string;
  payload: CredentialRequestPayload;
  isUpdate: boolean;
}) {
  const [values, setValues] = useState<Record<string, FieldValue>>(() => {
    const init: Record<string, FieldValue> = {};
    for (const f of payload.fields) init[f.name] = emptyValue();
    return init;
  });
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [optimisticStored, setOptimisticStored] = useState<
    { names: string[]; at: string } | null
  >(null);

  const stored = payload.stored ?? optimisticStored;
  if (stored) {
    return (
      <StoredReceipt
        title={payload.title}
        names={stored.names}
        at={stored.at}
        isUpdate={isUpdate}
      />
    );
  }

  if (payload.expired_at) {
    return (
      <ExpiredReceipt
        title={payload.title}
        reason={
          payload.expired_reason === "no response within 24h"
            ? "The agent waited 24 hours and moved on."
            : "The task that asked for this ended."
        }
        at={payload.expired_at}
        fieldNames={payload.fields.map((f) => f.name)}
      />
    );
  }

  function onChange(name: string, value: string) {
    setValues((prev) => ({
      ...prev,
      [name]: { ...(prev[name] ?? emptyValue()), value },
    }));
  }

  function onToggleReveal(name: string) {
    setValues((prev) => ({
      ...prev,
      [name]: {
        ...(prev[name] ?? emptyValue()),
        reveal: !(prev[name]?.reveal ?? false),
      },
    }));
  }

  async function postSubmission(body: unknown) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 30_000);
    try {
      const res = await apiFetch(`/api/task/${taskId}/credentials`, {
        method: "POST",
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as {
          error?: string;
        };
        throw new Error(data.error || `Request failed (${res.status})`);
      }
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") {
        throw new Error("Request timed out. Check your connection and retry.");
      }
      throw e;
    } finally {
      clearTimeout(timeout);
    }
  }

  async function onSubmit() {
    if (saving) return;
    setSaving(true);
    setError(null);

    const credentials = payload.fields.map((f) => ({
      name: f.name,
      label: f.label,
      value: values[f.name]?.value.trim() ?? "",
    }));

    try {
      await postSubmission({ credentials });
      setOptimisticStored({
        names: credentials.map((c) => c.name),
        at: new Date().toISOString(),
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to store credentials.");
      setSaving(false);
    }
  }

  async function onDismiss() {
    if (saving) return;
    setSaving(true);
    setError(null);
    try {
      await postSubmission({ cancelled: true });
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to dismiss.");
      setSaving(false);
    }
  }

  return (
    <InputForm
      payload={payload}
      saving={saving}
      error={error}
      onChange={onChange}
      onToggleReveal={onToggleReveal}
      onSubmit={onSubmit}
      onDismiss={onDismiss}
      values={values}
      isUpdate={isUpdate}
    />
  );
}
