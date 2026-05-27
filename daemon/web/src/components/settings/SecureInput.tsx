// SPDX-License-Identifier: Apache-2.0

import { useState } from "react";
import { X, Eye, EyeOff } from "lucide-react";
import { apiFetch } from "../../api/fetch";

export function SecureInput({
  suggestedName,
  onClose,
  onSaved,
}: {
  suggestedName?: string;
  onClose: () => void;
  onSaved: (name: string) => void;
}) {
  const [name, setName] = useState(suggestedName || "");
  const [value, setValue] = useState("");
  const [showValue, setShowValue] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSave() {
    if (!name.trim() || !value.trim()) return;
    setSaving(true);
    setError(null);

    try {
      const res = await apiFetch("/api/vault", {
        method: "POST",
        body: JSON.stringify({ name: name.trim(), value: value.trim() }),
      });

      if (res.ok) {
        onSaved(name.trim());
        onClose();
      } else {
        setError("Failed to save secret. Try again.");
      }
    } catch {
      setError("Network error. Check your connection.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="rounded-2xl border border-slate-200/60 bg-white p-4">
      <div className="flex items-center justify-between">
        <h4 className="font-poppins text-sm font-semibold text-slate-950">
          Add secret
        </h4>
        <button
          onClick={onClose}
          aria-label="Close"
          className="text-slate-400 hover:text-slate-600"
        >
          <X className="h-4 w-4" aria-hidden="true" />
        </button>
      </div>

      <div className="mt-3 space-y-3">
        <div>
          <label htmlFor="secret-name" className="text-xs text-slate-500">
            Name
          </label>
          <input
            id="secret-name"
            type="text"
            value={name}
            onChange={(e) =>
              setName(e.target.value.toUpperCase().replace(/[^A-Z0-9_]/g, ""))
            }
            placeholder="HUBSPOT_API_KEY"
            className="mt-1 w-full rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 text-sm text-slate-700 outline-none focus:border-slate-300"
          />
        </div>
        <div>
          <label htmlFor="secret-value" className="text-xs text-slate-500">
            Value
          </label>
          <div className="relative mt-1">
            <input
              id="secret-value"
              type={showValue ? "text" : "password"}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              placeholder="Enter secret value"
              className="w-full rounded-lg border border-slate-200/60 bg-slate-50 px-3 py-2 pr-10 text-sm text-slate-700 outline-none focus:border-slate-300"
            />
            <button
              onClick={() => setShowValue(!showValue)}
              aria-label={showValue ? "Hide value" : "Show value"}
              className="absolute right-2 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600"
            >
              {showValue ? (
                <EyeOff className="h-4 w-4" aria-hidden="true" />
              ) : (
                <Eye className="h-4 w-4" aria-hidden="true" />
              )}
            </button>
          </div>
        </div>
      </div>

      {error && (
        <p className="mt-2 text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      <div className="mt-4 flex justify-end gap-2">
        <button
          onClick={onClose}
          className="rounded-full px-4 py-2 text-xs text-slate-500 hover:text-slate-600"
        >
          Cancel
        </button>
        <button
          onClick={handleSave}
          disabled={saving || !name.trim() || !value.trim()}
          aria-busy={saving}
          className="rounded-full bg-slate-950 px-4 py-2 text-xs font-medium text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20 disabled:opacity-50 disabled:hover:translate-y-0"
        >
          {saving ? "Saving..." : "Save secret"}
        </button>
      </div>
    </div>
  );
}
