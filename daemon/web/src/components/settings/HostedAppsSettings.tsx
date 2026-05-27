// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { ExternalLink, Trash2 } from "lucide-react";
import { apiFetch } from "../../api/fetch";

type HostedApp = {
  name: string;
  port: number;
  url: string;
  sso_enabled: boolean;
  created_at: string;
};

export function HostedAppsSettings({ readOnly }: { readOnly: boolean }) {
  const [apps, setApps] = useState<HostedApp[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    apiFetch("/api/hosted-apps", { signal: controller.signal })
      .then(async (res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = (await res.json()) as { routes?: HostedApp[] };
        setApps(data.routes ?? []);
      })
      .catch((err) => {
        if (err.name !== "AbortError") {
          setError("Couldn't load hosted apps.");
          setApps([]);
        }
      });
    return () => controller.abort();
  }, []);

  async function toggleSSO(name: string, next: boolean) {
    setBusy(name);
    try {
      const res = await apiFetch(`/api/hosted-apps/${encodeURIComponent(name)}`, {
        method: "PATCH",
        body: JSON.stringify({ sso_enabled: next }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setApps((prev) =>
        prev
          ? prev.map((a) =>
              a.name === name ? { ...a, sso_enabled: next } : a,
            )
          : prev,
      );
    } catch {
      setError("Couldn't update SSO setting.");
    } finally {
      setBusy(null);
    }
  }

  async function remove(name: string) {
    setBusy(name);
    try {
      const res = await apiFetch(
        `/api/hosted-apps/${encodeURIComponent(name)}`,
        { method: "DELETE" },
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setApps((prev) => (prev ? prev.filter((a) => a.name !== name) : prev));
    } catch {
      setError("Couldn't delete app.");
    } finally {
      setBusy(null);
      setConfirmDelete(null);
    }
  }

  return (
    <div className="space-y-4">
      <h3 className="font-poppins text-sm font-semibold text-slate-950">
        Hosted apps
      </h3>
      <p className="text-xs leading-relaxed text-slate-500">
        Apps the manager has set up on this computer. Each gets its own
        subdomain with HTTPS.{" "}
        <strong className="font-medium text-slate-700">Private</strong>{" "}
        requires sign-in (only people with access to this computer can open
        the app — anyone else lands on the platform sign-in page).{" "}
        <strong className="font-medium text-slate-700">Public</strong>{" "}
        means anyone with the URL can open it. Apps can still read who you
        are via a same-origin{" "}
        <code className="rounded bg-slate-100 px-1 py-0.5 text-[10px]">
          /__auth/whoami
        </code>{" "}
        endpoint in either mode.
      </p>

      {error && (
        <p className="text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      {apps === null && (
        <div className="space-y-2">
          {[0, 1].map((i) => (
            <div
              key={i}
              className="h-12 animate-pulse rounded-xl border border-slate-200/60 bg-white/40"
            />
          ))}
        </div>
      )}

      {apps !== null && apps.length === 0 && (
        <p className="py-6 text-center text-xs text-slate-400">
          No hosted apps yet. The manager creates them automatically when a
          task needs one.
        </p>
      )}

      {apps !== null && apps.length > 0 && (
        <ul className="space-y-1.5">
          {apps.map((app) => (
            <li
              key={app.name}
              className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-slate-200/60 bg-white/50 px-4 py-3"
            >
              <div className="min-w-0 flex-1">
                <a
                  href={app.url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 font-medium text-slate-950 transition-colors hover:text-slate-600"
                >
                  <span className="font-mono text-sm">{app.name}</span>
                  <ExternalLink
                    className="h-3 w-3 text-slate-400"
                    aria-hidden="true"
                  />
                </a>
                <div className="mt-0.5 truncate font-mono text-[11px] text-slate-400">
                  {app.url} · port {app.port}
                </div>
              </div>
              <div className="flex items-center gap-3">
                <label
                  className="flex items-center gap-2 text-xs text-slate-600"
                  title={
                    app.sso_enabled
                      ? "Private — only people with access to this computer can open this app"
                      : "Public — anyone with the URL can open this app"
                  }
                >
                  <input
                    type="checkbox"
                    checked={app.sso_enabled}
                    onChange={(e) => toggleSSO(app.name, e.target.checked)}
                    disabled={readOnly || busy === app.name}
                    className="h-3.5 w-3.5 cursor-pointer accent-slate-950 disabled:cursor-not-allowed disabled:opacity-50"
                  />
                  Private
                </label>
                {!readOnly &&
                  (confirmDelete === app.name ? (
                    <div className="flex items-center gap-2">
                      <button
                        onClick={() => remove(app.name)}
                        disabled={busy === app.name}
                        className="text-[11px] font-medium text-red-500 hover:text-red-600 disabled:opacity-50"
                      >
                        Confirm
                      </button>
                      <button
                        onClick={() => setConfirmDelete(null)}
                        className="text-[11px] text-slate-400 hover:text-slate-500"
                      >
                        Cancel
                      </button>
                    </div>
                  ) : (
                    <button
                      onClick={() => setConfirmDelete(app.name)}
                      aria-label={`Delete hosted app ${app.name}`}
                      className="text-slate-300 transition hover:text-red-400"
                    >
                      <Trash2 className="h-3.5 w-3.5" aria-hidden="true" />
                    </button>
                  ))}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
