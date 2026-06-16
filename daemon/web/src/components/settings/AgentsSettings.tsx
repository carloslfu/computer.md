// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { apiFetch } from "../../api/fetch";

type AgentInfo = {
  name: string;
  command: string;
  version?: string;
  installed: boolean;
};

type SystemInfo = {
  agents?: AgentInfo[];
};

export function AgentsSettings() {
  const [agents, setAgents] = useState<AgentInfo[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await apiFetch("/api/system");
        if (!res.ok) {
          if (!cancelled) setError("Could not reach the computer.");
          return;
        }
        const data = (await res.json()) as SystemInfo;
        if (!cancelled) setAgents(data.agents ?? []);
      } catch {
        if (!cancelled) setError("Could not reach the computer.");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div>
      <h3 className="font-poppins text-base font-semibold text-slate-950">
        Worker agents
      </h3>
      <p className="mt-1.5 text-sm leading-relaxed text-slate-500">
        Programs the manager can spawn to fan out parallel work. Claude Code
        and Codex are preinstalled. Anything else you install on the computer
        will show up here too.
      </p>

      {error && <p className="mt-6 text-sm text-slate-500">{error}</p>}

      {!error && agents === null && (
        <div className="mt-6 space-y-2">
          {[0, 1].map((i) => (
            <div
              key={i}
              className="h-14 animate-pulse rounded-xl border border-slate-200/60 bg-white/40"
            />
          ))}
        </div>
      )}

      {!error && agents && agents.length === 0 && (
        <p className="mt-6 text-sm text-slate-500">
          No agents detected. The preinstalled workers (Claude Code and Codex)
          arrive automatically on the next updater tick.
        </p>
      )}

      {!error && agents && agents.length > 0 && (
        <ul className="mt-6 space-y-2">
          {agents.map((a) => (
            <li
              key={a.command}
              className="flex items-center justify-between rounded-xl border border-slate-200/60 bg-white/50 px-4 py-3"
            >
              <div>
                <p className="text-sm font-medium text-slate-950">{a.name}</p>
                <p className="mt-0.5 text-xs text-slate-500">
                  {a.installed
                    ? a.version || "installed"
                    : "Not installed yet"}
                </p>
              </div>
              <span
                className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-[11px] font-medium ${
                  a.installed
                    ? "bg-emerald-50 text-emerald-700"
                    : "bg-slate-100 text-slate-500"
                }`}
              >
                <span
                  className={`h-1.5 w-1.5 rounded-full ${
                    a.installed ? "bg-emerald-500" : "bg-slate-400"
                  }`}
                  aria-hidden="true"
                />
                {a.installed ? "Ready" : "Missing"}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
