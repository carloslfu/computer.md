// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from "react";
import { ExternalLink } from "lucide-react";
import { apiFetch } from "../api/fetch";

// SystemsList is the sidebar's at-a-glance "what's running on my
// computer" view. Designed for non-technical operators: no port
// numbers, no kebab-case identifiers, no "crontab" or "systemd" — the
// row shows a humanized name, a status dot, and either an "open" arrow
// (for hosted apps) or a last-touched timestamp (for everything else).
//
// Render rules (the section disappears entirely when empty — a fresh
// machine shouldn't show "Nothing running" as confusing first-run UI):
//   1. Empty list  → nothing renders, sidebar stays clean.
//   2. Loading     → skeleton rows, only on first load.
//   3. Populated   → header + rows, max-height capped so it can't
//                    crowd out the conversation list on small screens.
//
// Auto-refreshes on a slow tick (45s) so the operator's at-a-glance
// view stays honest without hammering the daemon. The probe latency
// on the daemon side is bounded by 1.5s × N parallel, so even a
// machine with 20 systems renders in well under 2s.

type SystemView = {
  name: string;
  display_name: string;
  kind: "app" | "scheduled" | "idle";
  url?: string;
  live?: "up" | "down";
  last_active?: string;
};

export function SystemsList({ onOpenSettings }: { onOpenSettings: () => void }) {
  const [systems, setSystems] = useState<SystemView[] | null>(null);

  useEffect(() => {
    let cancelled = false;

    async function refresh() {
      try {
        const res = await apiFetch("/api/dashboard/systems");
        if (!res.ok) return;
        const data = (await res.json()) as { systems?: SystemView[] };
        if (!cancelled) setSystems(data.systems ?? []);
      } catch {
        // Silent — a transient failure shouldn't blank the sidebar.
        // The next tick will retry.
      }
    }

    refresh();
    const interval = window.setInterval(refresh, 45_000);
    return () => {
      cancelled = true;
      window.clearInterval(interval);
    };
  }, []);

  // First-load skeleton. Two narrow placeholder rows, just enough to
  // hint "this area will populate" without being a loading-spinner
  // distraction on a fast connection.
  if (systems === null) {
    return (
      <div
        className="border-t border-slate-200/60 p-3"
        aria-label="Systems loading"
      >
        <div className="mb-1.5 px-2 text-[10px] font-medium uppercase tracking-widest text-slate-400">
          Systems
        </div>
        <div className="space-y-1">
          <div className="h-7 w-full animate-pulse rounded-lg bg-slate-200/40" />
          <div className="h-7 w-4/5 animate-pulse rounded-lg bg-slate-200/40" />
        </div>
      </div>
    );
  }

  // Restraint: nothing to show, nothing renders. A first-run user with
  // no systems sees a clean sidebar instead of an empty-state mystery.
  if (systems.length === 0) return null;

  return (
    <div className="border-t border-slate-200/60 p-3">
      <div className="mb-1.5 px-2 text-[10px] font-medium uppercase tracking-widest text-slate-400">
        Systems
      </div>
      <ul
        className="space-y-0.5 overflow-y-auto"
        style={{ maxHeight: "9.5rem" }}
        aria-label="Running systems"
      >
        {systems.map((sys) => (
          <SystemRow key={sys.name} sys={sys} onOpenSettings={onOpenSettings} />
        ))}
      </ul>
    </div>
  );
}

function SystemRow({
  sys,
  onOpenSettings,
}: {
  sys: SystemView;
  onOpenSettings: () => void;
}) {
  // For hosted apps the row is a link that opens the public URL in a
  // new tab — the most common operator action on this surface. For
  // scheduled/idle systems we point at Settings → Hosted apps (the
  // existing detail view; a dedicated Systems panel is a future
  // workstream and is intentionally out of scope here).
  const isApp = sys.kind === "app" && Boolean(sys.url);

  const dot = (
    <span
      aria-hidden="true"
      className={`h-1.5 w-1.5 shrink-0 rounded-full ${
        sys.kind === "app"
          ? sys.live === "up"
            ? "bg-emerald-500"
            : "bg-slate-300"
          : sys.kind === "scheduled"
            ? "bg-amber-400"
            : "bg-slate-300"
      }`}
    />
  );

  const trailing = isApp ? (
    <ExternalLink className="h-3 w-3 shrink-0 text-slate-400" aria-hidden="true" />
  ) : (
    <span className="shrink-0 text-[11px] tabular-nums text-slate-400">
      {relativeTime(sys.last_active)}
    </span>
  );

  const rowClasses =
    "group flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm text-slate-600 transition hover:bg-slate-950/[0.03] hover:text-slate-900";

  if (isApp) {
    return (
      <li>
        <a
          href={sys.url}
          target="_blank"
          rel="noreferrer"
          className={rowClasses}
          aria-label={`Open ${sys.display_name}${sys.live === "down" ? " (currently unreachable)" : ""}`}
        >
          {dot}
          <span className="line-clamp-1 flex-1">{sys.display_name}</span>
          {trailing}
        </a>
      </li>
    );
  }

  return (
    <li>
      <button
        type="button"
        onClick={onOpenSettings}
        className={rowClasses}
        aria-label={`View ${sys.display_name} in settings`}
      >
        {dot}
        <span className="line-clamp-1 flex-1">{sys.display_name}</span>
        {trailing}
      </button>
    </li>
  );
}

// relativeTime turns an RFC3339 timestamp into a tiny human label
// ("5m", "2h", "yesterday", "3d"). Returns an empty string for
// unparseable input — the row still renders, just without the trailing
// timestamp. Sub-minute precision isn't useful in this surface; we
// floor to the minute.
function relativeTime(ts?: string): string {
  if (!ts) return "";
  const t = Date.parse(ts);
  if (Number.isNaN(t)) return "";
  const diffMs = Date.now() - t;
  if (diffMs < 0) return "just now"; // clock skew safety
  const minutes = Math.floor(diffMs / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "yesterday";
  if (days < 7) return `${days}d`;
  return `${Math.floor(days / 7)}w`;
}
