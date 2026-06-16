// SPDX-License-Identifier: Apache-2.0

import { useState, useEffect } from "react";
import { Plus, Trash2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";

type Rule = {
  id: string;
  name: string;
  pattern: string;
  action: string;
  description: string;
  enabled: boolean;
  priority: number;
};

export function RulesSettings({ readOnly }: { readOnly: boolean }) {
  const [rules, setRules] = useState<Rule[]>([]);
  const [showAdd, setShowAdd] = useState(false);
  const [newRule, setNewRule] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/rules", { signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error("Failed to load rules");
        return res.json();
      })
      .then((data) => {
        if (Array.isArray(data)) setRules(data);
      })
      .catch((err) => {
        if (err.name !== "AbortError") setError("Failed to load rules");
      });
    return () => controller.abort();
  }, []);

  async function addRule() {
    if (!newRule.trim()) return;
    try {
      const res = await apiFetch("/api/rules", {
        method: "POST",
        body: JSON.stringify({
          name: newRule.trim(),
          pattern: newRule.trim(),
          action: "confirm",
          description: newRule.trim(),
          enabled: true,
        }),
      });
      if (!res.ok) throw new Error();
      const rule = await res.json();
      setRules((prev) => [...prev, rule]);
      setNewRule("");
      setShowAdd(false);
      setError(null);
    } catch {
      setError("Failed to add rule");
    }
  }

  async function deleteRule(id: string) {
    try {
      const res = await apiFetch(`/api/rules/${encodeURIComponent(id)}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error();
      setRules((prev) => prev.filter((r) => r.id !== id));
    } catch {
      setError("Failed to delete rule");
    } finally {
      setConfirmDelete(null);
    }
  }

  if (readOnly) {
    return (
      <p className="text-sm text-slate-500">
        Only operators with control access can manage guardrail policies.
      </p>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h3 className="font-poppins text-sm font-semibold text-slate-950">
          Guardrail policies
        </h3>
        <button
          onClick={() => setShowAdd(true)}
          className="flex items-center gap-1 rounded-full bg-slate-100 px-3 py-1.5 text-xs text-slate-600 transition hover:bg-slate-200"
        >
          <Plus className="h-3 w-3" aria-hidden="true" />
          Add rule
        </button>
      </div>

      <p className="text-xs text-slate-500">
        Rules that control what your agent can do. Default policies are always
        active.
      </p>

      {error && (
        <p className="text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      {showAdd && (
        <div className="flex gap-2">
          <label htmlFor="new-rule" className="sr-only">
            New guardrail rule
          </label>
          <input
            id="new-rule"
            type="text"
            value={newRule}
            onChange={(e) => setNewRule(e.target.value)}
            placeholder='e.g., "Never touch the production database"'
            className="flex-1 rounded-lg border border-slate-200/60 bg-white/50 px-3 py-2 text-xs text-slate-700 outline-none focus:border-slate-300"
            onKeyDown={(e) => e.key === "Enter" && addRule()}
          />
          <button
            onClick={addRule}
            className="rounded-full bg-slate-950 px-3 py-2 text-xs font-medium text-white"
          >
            Add
          </button>
        </div>
      )}

      {rules.length === 0 && (
        <p className="py-6 text-center text-xs text-slate-400">
          Default policies are active. Add custom rules to restrict agent
          behavior.
        </p>
      )}

      <div className="space-y-1">
        {rules.map((rule) => (
          <div
            key={rule.id}
            className="flex items-center justify-between rounded-lg px-3 py-2 transition hover:bg-white/30"
          >
            <div>
              <span className="text-xs text-slate-600">{rule.name}</span>
              <span className="ml-2 text-[10px] text-slate-400">
                {rule.action}
              </span>
            </div>
            {confirmDelete === rule.id ? (
              <div className="flex items-center gap-2">
                <button
                  onClick={() => deleteRule(rule.id)}
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
                onClick={() => setConfirmDelete(rule.id)}
                aria-label={`Delete rule: ${rule.name}`}
                className="text-slate-300 transition hover:text-red-400"
              >
                <Trash2 className="h-3.5 w-3.5" aria-hidden="true" />
              </button>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
