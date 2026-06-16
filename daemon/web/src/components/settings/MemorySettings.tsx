// SPDX-License-Identifier: Apache-2.0

import { useState, useEffect } from "react";
import { Trash2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";

type Memory = {
  id: string;
  category: string;
  key: string;
  value: string;
  created_at: string;
};

export function MemorySettings({ readOnly }: { readOnly: boolean }) {
  const [memories, setMemories] = useState<Memory[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/memory?excludeCategory=activity", {
      signal: controller.signal,
    })
      .then((res) => {
        if (!res.ok) throw new Error("Failed to load memories");
        return res.json();
      })
      .then((data) => {
        if (Array.isArray(data)) setMemories(data);
      })
      .catch((err) => {
        if (err.name !== "AbortError") setError("Failed to load memories");
      });
    return () => controller.abort();
  }, []);

  async function deleteMemory(id: string) {
    try {
      const res = await apiFetch(`/api/memory/${encodeURIComponent(id)}`, {
        method: "DELETE",
      });
      if (!res.ok) throw new Error();
      setMemories((prev) => prev.filter((m) => m.id !== id));
    } catch {
      setError("Failed to delete memory");
    } finally {
      setConfirmDelete(null);
    }
  }

  const categoryLabels: Record<string, string> = {
    preference: "Preferences",
    workflow: "Workflows",
    contact: "Contacts",
    learned: "Learned",
  };

  const grouped: Record<string, Memory[]> = {};
  for (const mem of memories) {
    const cat = mem.category || "other";
    if (!grouped[cat]) grouped[cat] = [];
    grouped[cat].push(mem);
  }

  return (
    <div className="space-y-4">
      <h3 className="font-poppins text-sm font-semibold text-slate-950">
        Agent memory
      </h3>
      <p className="text-xs text-slate-500">
        Facts your agent has learned about you and your workflows. These are
        used to personalize its behavior.
      </p>

      {error && (
        <p className="text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      {memories.length === 0 && (
        <p className="py-6 text-center text-xs text-slate-400">
          No memories stored yet. Your agent will learn as you work together.
        </p>
      )}

      {Object.entries(grouped).map(([category, mems]) => (
        <div key={category}>
          <div className="mb-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
            {categoryLabels[category] || category}
          </div>
          <div className="space-y-1">
            {mems.map((mem) => (
              <div
                key={mem.id}
                className="flex items-start justify-between rounded-lg px-3 py-2 transition hover:bg-white/30"
              >
                <div className="flex-1">
                  <span className="text-xs font-medium text-slate-600">
                    {mem.key}
                  </span>
                  <p className="text-xs text-slate-500">{mem.value}</p>
                </div>
                {!readOnly &&
                  (confirmDelete === mem.id ? (
                    <div className="ml-2 mt-0.5 flex items-center gap-2">
                      <button
                        onClick={() => deleteMemory(mem.id)}
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
                      onClick={() => setConfirmDelete(mem.id)}
                      aria-label={`Delete memory: ${mem.key}`}
                      className="ml-2 mt-0.5 text-slate-300 transition hover:text-red-400"
                    >
                      <Trash2 className="h-3.5 w-3.5" aria-hidden="true" />
                    </button>
                  ))}
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
