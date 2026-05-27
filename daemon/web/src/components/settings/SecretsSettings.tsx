// SPDX-License-Identifier: Apache-2.0

import { useState, useEffect } from "react";
import { Plus, Trash2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";
import { SecureInput } from "./SecureInput";

type Secret = {
  name: string;
  label?: string;
  created_at?: string;
  updated_at?: string;
};

function formatAddedAt(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function SecretsSettings({ readOnly }: { readOnly: boolean }) {
  const [secrets, setSecrets] = useState<Secret[]>([]);
  const [showAdd, setShowAdd] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/vault", { signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error("Failed to load secrets");
        return res.json();
      })
      .then((data) => {
        if (Array.isArray(data)) setSecrets(data);
      })
      .catch((err) => {
        if (err.name !== "AbortError") setError("Failed to load secrets");
      });
    return () => controller.abort();
  }, []);

  async function deleteSecret(name: string) {
    try {
      const res = await apiFetch(`/api/vault/${encodeURIComponent(name)}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error();
      setSecrets((prev) => prev.filter((s) => s.name !== name));
    } catch {
      setError("Failed to delete secret");
    } finally {
      setConfirmDelete(null);
    }
  }

  if (readOnly) {
    return (
      <p className="text-sm text-slate-500">
        Only operators with control access can manage secrets.
      </p>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h3 className="font-poppins text-sm font-semibold text-slate-950">
          Secrets &amp; environment
        </h3>
        <button
          onClick={() => setShowAdd(true)}
          className="flex items-center gap-1 rounded-full bg-slate-100 px-3 py-1.5 text-xs text-slate-600 transition hover:bg-slate-200"
        >
          <Plus className="h-3 w-3" aria-hidden="true" />
          Add
        </button>
      </div>

      {error && (
        <p className="text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      {showAdd && (
        <SecureInput
          onClose={() => setShowAdd(false)}
          onSaved={(name) => {
            const now = new Date().toISOString();
            setSecrets((prev) => [
              ...prev.filter((s) => s.name !== name),
              { name, created_at: now, updated_at: now },
            ]);
            setShowAdd(false);
          }}
        />
      )}

      {secrets.length === 0 && !showAdd && (
        <p className="py-6 text-center text-xs text-slate-400">
          No secrets stored yet.
        </p>
      )}

      <div className="space-y-1">
        {secrets.map((secret) => {
          const added = formatAddedAt(secret.updated_at ?? secret.created_at);
          return (
            <div
              key={secret.name}
              className="flex items-center justify-between rounded-lg px-3 py-2 transition hover:bg-white/30"
            >
              <div>
                <span className="font-mono text-xs text-slate-700">
                  {secret.name}
                </span>
                <div className="flex items-center gap-2 text-[10px] text-slate-400">
                  <span className="inline-flex items-center gap-1 text-green-600">
                    <span
                      className="h-1.5 w-1.5 rounded-full bg-green-500"
                      aria-hidden="true"
                    />
                    Set
                  </span>
                  {secret.label && secret.label !== secret.name && (
                    <span className="text-slate-500">· {secret.label}</span>
                  )}
                  {added && <span>· Added {added}</span>}
                </div>
              </div>
              {confirmDelete === secret.name ? (
                <div className="flex items-center gap-2">
                  <button
                    onClick={() => deleteSecret(secret.name)}
                    className="text-[10px] font-medium text-red-500 hover:text-red-600"
                  >
                    Confirm
                  </button>
                  <button
                    onClick={() => setConfirmDelete(null)}
                    className="text-[10px] text-slate-400 hover:text-slate-500"
                  >
                    Cancel
                  </button>
                </div>
              ) : (
                <button
                  onClick={() => setConfirmDelete(secret.name)}
                  aria-label={`Delete secret ${secret.name}`}
                  className="text-slate-300 transition hover:text-red-400"
                >
                  <Trash2 className="h-3.5 w-3.5" aria-hidden="true" />
                </button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
