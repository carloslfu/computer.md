// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { Check, Pencil, X } from "lucide-react";
import { apiFetch } from "../../api/fetch";
import { PLATFORM_BASE } from "../../auth/types";

type SystemInfo = {
  cpu: { model: string; cores: number; threads: number };
  memory_gb: number;
  hostname: string;
  kernel: string;
  uptime_seconds: number;
  daemon_version: string;
};

type MachineIdentity = {
  id: string;
  name: string;
  host: string;
};

function formatCores(cores: number, threads: number): string {
  if (cores <= 0) return "—";
  if (threads > cores) return `${cores}c / ${threads}t`;
  return `${cores} core${cores === 1 ? "" : "s"}`;
}

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${Math.floor(seconds)}s`;
}

type Row = { label: string; value: string; sub?: string; mono?: boolean };

function SpecRow({ label, value, sub, mono }: Row) {
  return (
    <div className="flex items-start justify-between gap-4 py-2.5">
      <span className="text-sm text-slate-500">{label}</span>
      <span className="text-right">
        <span
          className={`text-sm text-slate-700 ${mono ? "font-mono text-xs" : ""}`}
        >
          {value}
        </span>
        {sub && (
          <span className="block text-[11px] text-slate-400">{sub}</span>
        )}
      </span>
    </div>
  );
}

export function MachineSettings({ readOnly = false }: { readOnly?: boolean }) {
  const [info, setInfo] = useState<SystemInfo | null>(null);
  const [identity, setIdentity] = useState<MachineIdentity | null>(null);
  const [editingName, setEditingName] = useState(false);
  const [draftName, setDraftName] = useState("");
  const [savingName, setSavingName] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nameError, setNameError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([
      apiFetch("/api/system", { signal: controller.signal }),
      apiFetch("/api/machine", { signal: controller.signal }),
    ])
      .then((res) => {
        if (!res[0].ok || !res[1].ok) {
          throw new Error("Failed to load machine info");
        }
        return Promise.all([
          res[0].json() as Promise<SystemInfo>,
          res[1].json() as Promise<MachineIdentity>,
        ]);
      })
      .then(([systemInfo, machineIdentity]) => {
        setInfo(systemInfo);
        setIdentity(machineIdentity);
        setDraftName(machineIdentity.name);
      })
      .catch((err) => {
        if (err.name !== "AbortError") setError("Could not reach the computer.");
      });
    return () => controller.abort();
  }, []);

  async function saveName() {
    if (!identity) return;
    const name = draftName.trim().replace(/\s+/g, " ");
    if (!name || name.length > 80) {
      setNameError("Use 1-80 characters.");
      return;
    }
    setSavingName(true);
    setNameError(null);
    try {
      // eslint-disable-next-line no-restricted-syntax -- cross-origin call to the platform source of truth; apiFetch is daemon-scoped
      const res = await fetch(
        `${PLATFORM_BASE}/api/machines/${encodeURIComponent(identity.id)}/name`,
        {
          method: "POST",
          credentials: "include",
          headers: {
            "Content-Type": "application/json",
            Accept: "application/json",
          },
          body: JSON.stringify({ name }),
        },
      );
      const body = (await res.json().catch(() => ({}))) as {
        name?: string;
        error?: string;
      };
      if (!res.ok || !body.name) {
        throw new Error(body.error || "Rename failed");
      }
      const next = { ...identity, name: body.name };
      setIdentity(next);
      setDraftName(body.name);
      setEditingName(false);
      window.dispatchEvent(
        new CustomEvent("vibecraft:machine-name-updated", { detail: next }),
      );
    } catch (err) {
      setNameError(err instanceof Error ? err.message : "Rename failed");
    } finally {
      setSavingName(false);
    }
  }

  return (
    <div>
      <h3 className="font-poppins text-base font-semibold text-slate-950">
        Machine
      </h3>
      <p className="mt-1.5 text-sm leading-relaxed text-slate-500">
        Hardware and runtime details, read live from the computer itself. The
        manager sees the same picture.
      </p>

      {error && <p className="mt-6 text-sm text-slate-500">{error}</p>}

      {!error && !info && (
        <div className="mt-6 space-y-2">
          {[0, 1, 2].map((i) => (
            <div
              key={i}
              className="h-14 animate-pulse rounded-xl border border-slate-200/60 bg-white/40"
            />
          ))}
        </div>
      )}

      {!error && info && (
        <div className="mt-6 space-y-6">
          <section>
            <div className="mb-1 text-[10px] font-medium uppercase tracking-widest text-slate-400">
              Specs
            </div>
            <div className="divide-y divide-slate-200/60 rounded-xl border border-slate-200/60 bg-white/50 px-4">
              <SpecRow
                label="CPU"
                value={formatCores(info.cpu.cores, info.cpu.threads)}
                sub={info.cpu.model || undefined}
              />
              <SpecRow label="Memory" value={`${info.memory_gb} GB`} />
            </div>
          </section>

          <section>
            <div className="mb-1 text-[10px] font-medium uppercase tracking-widest text-slate-400">
              Identity
            </div>
            <div className="divide-y divide-slate-200/60 rounded-xl border border-slate-200/60 bg-white/50 px-4">
              <div className="flex items-start justify-between gap-4 py-2.5">
                <span className="text-sm text-slate-500">Name</span>
                <div className="min-w-0 flex-1 text-right">
                  {editingName ? (
                    <div>
                      <div className="flex items-center justify-end gap-1.5">
                        <input
                          value={draftName}
                          onChange={(e) => setDraftName(e.target.value)}
                          maxLength={80}
                          disabled={savingName}
                          className="h-8 w-36 rounded-md border border-slate-200 bg-white px-2 text-sm text-slate-800 outline-none transition focus:border-slate-400 sm:w-56"
                          autoFocus
                        />
                        <button
                          type="button"
                          onClick={saveName}
                          disabled={savingName}
                          className="inline-flex h-8 w-8 items-center justify-center rounded-md border border-slate-200 bg-white text-slate-600 transition hover:border-slate-300 hover:text-slate-950 disabled:opacity-60"
                          aria-label="Save machine name"
                          title="Save name"
                        >
                          <Check className="h-4 w-4" aria-hidden="true" />
                        </button>
                        <button
                          type="button"
                          onClick={() => {
                            setEditingName(false);
                            setDraftName(identity?.name ?? "");
                            setNameError(null);
                          }}
                          disabled={savingName}
                          className="inline-flex h-8 w-8 items-center justify-center rounded-md border border-slate-200 bg-white text-slate-600 transition hover:border-slate-300 hover:text-slate-950 disabled:opacity-60"
                          aria-label="Cancel machine rename"
                          title="Cancel"
                        >
                          <X className="h-4 w-4" aria-hidden="true" />
                        </button>
                      </div>
                      {nameError && (
                        <span className="mt-1 block text-[11px] text-red-600">
                          {nameError}
                        </span>
                      )}
                    </div>
                  ) : (
                    <div className="flex items-center justify-end gap-2">
                      <span className="min-w-0 truncate text-sm text-slate-700">
                        {identity?.name || "—"}
                      </span>
                      {!readOnly && (
                        <button
                          type="button"
                          onClick={() => {
                            setDraftName(identity?.name ?? "");
                            setEditingName(true);
                            setNameError(null);
                          }}
                          disabled={!identity}
                          className="inline-flex h-7 w-7 items-center justify-center rounded-md border border-slate-200 bg-white text-slate-500 transition hover:border-slate-300 hover:text-slate-950 disabled:opacity-40"
                          aria-label="Rename machine"
                          title="Rename"
                        >
                          <Pencil className="h-3.5 w-3.5" aria-hidden="true" />
                        </button>
                      )}
                    </div>
                  )}
                </div>
              </div>
              <SpecRow label="Address" value={identity?.host || "—"} mono />
              <SpecRow label="Hostname" value={info.hostname || "—"} mono />
              <SpecRow label="Kernel" value={info.kernel || "—"} mono />
            </div>
          </section>

          <section>
            <div className="mb-1 text-[10px] font-medium uppercase tracking-widest text-slate-400">
              Runtime
            </div>
            <div className="divide-y divide-slate-200/60 rounded-xl border border-slate-200/60 bg-white/50 px-4">
              <SpecRow label="Uptime" value={formatUptime(info.uptime_seconds)} />
              <SpecRow
                label="Daemon"
                value={info.daemon_version || "—"}
                mono
              />
            </div>
          </section>
        </div>
      )}
    </div>
  );
}
