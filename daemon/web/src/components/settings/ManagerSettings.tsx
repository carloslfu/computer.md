// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { CheckCircle2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";
import { ManagerKeySetupCard } from "../ManagerKeySetupCard";

type ManagerStatus = {
  mode?: string;
  configured?: boolean;
  setup_required?: boolean;
  can_configure?: boolean;
};

function modeLabel(mode?: string): string {
  if (mode === "platform") return "Included with this managed computer";
  if (mode === "operator") return "Operator-owned key";
  if (mode === "relay") return "Managed relay";
  return "Not configured";
}

export function ManagerSettings({ readOnly }: { readOnly: boolean }) {
  const [status, setStatus] = useState<ManagerStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    apiFetch("/api/manager-key")
      .then((res) => {
        if (!res.ok) throw new Error("status");
        return res.json() as Promise<ManagerStatus>;
      })
      .then((data) => {
        if (!cancelled) setStatus(data);
      })
      .catch(() => {
        if (!cancelled) setError("Could not load manager settings.");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div>
      <h3 className="font-poppins text-base font-semibold text-slate-950">
        Manager
      </h3>
      <p className="mt-1.5 text-sm leading-relaxed text-slate-500">
        The manager is the AI that operates this computer. Managed computers use
        VibeCraft credits. Connected computers use your OpenAI key on the machine.
      </p>

      {error && <p className="mt-6 text-sm text-slate-500">{error}</p>}

      {!error && !status && (
        <div className="mt-6 h-24 animate-pulse rounded-xl border border-slate-200/60 bg-white/40" />
      )}

      {!error && status && status.configured && (
        <div className="mt-6 rounded-xl border border-slate-200/60 bg-white/50 p-4">
          <div className="flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-widest text-emerald-700">
            <CheckCircle2 className="h-3 w-3" aria-hidden="true" />
            Ready
          </div>
          <p className="mt-1.5 font-poppins text-[15px] font-medium text-slate-950">
            {modeLabel(status.mode)}
          </p>
          <p className="mt-2 text-sm leading-relaxed text-slate-500">
            {status.mode === "operator"
              ? "The key is stored on this computer and is not shown to workers, apps, chat, or browser code."
              : "No key setup is needed on this computer."}
          </p>
        </div>
      )}

      {!error && status && !status.configured && status.can_configure === false && (
        <div className="mt-6 rounded-xl border border-slate-200/60 bg-white/50 p-4">
          <p className="font-poppins text-[15px] font-medium text-slate-950">
            Manager setup is pending
          </p>
          <p className="mt-2 text-sm leading-relaxed text-slate-500">
            This managed computer needs VibeCraft setup before it can run tasks.
            No local key is needed here.
          </p>
        </div>
      )}

      {!error && status && !status.configured && status.can_configure !== false && (
        <div className="mt-6">
          <ManagerKeySetupCard surface="settings" readOnly={readOnly} />
        </div>
      )}
    </div>
  );
}
