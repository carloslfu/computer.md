// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { ChevronLeft, ExternalLink } from "lucide-react";
import { type WhoAmI, PLATFORM_BASE, machineSlug } from "../auth/types";
import { SecretsSettings } from "../components/settings/SecretsSettings";
import { MemorySettings } from "../components/settings/MemorySettings";
import { RulesSettings } from "../components/settings/RulesSettings";
import { AgentsSettings } from "../components/settings/AgentsSettings";
import { HostedAppsSettings } from "../components/settings/HostedAppsSettings";
import { ComputerMdSettings } from "../components/settings/ComputerMdSettings";
import { MachineSettings } from "../components/settings/MachineSettings";
import { HistorySettings } from "../components/settings/HistorySettings";
import { UsageSettings } from "../components/settings/UsageSettings";
import { ManagerSettings } from "../components/settings/ManagerSettings";

type Tab = {
  id: string;
  label: string;
  panel: () => JSX.Element;
};

type TabGroup = { label: string; tabs: Tab[] };

export function SettingsRoute({ who }: { who: WhoAmI }) {
  const location = useLocation();
  const readOnly = who.access !== "control";

  const groups = useMemo<TabGroup[]>(
    () => [
      {
        label: "Computer",
        tabs: [
          {
            id: "computer-md",
            label: "COMPUTER.md",
            panel: () => <ComputerMdSettings readOnly={readOnly} />,
          },
          {
            id: "machine",
            label: "Machine",
            panel: () => <MachineSettings readOnly={readOnly} />,
          },
          { id: "agents", label: "Agents", panel: () => <AgentsSettings /> },
          {
            id: "hosted-apps",
            label: "Hosted apps",
            panel: () => <HostedAppsSettings readOnly={readOnly} />,
          },
        ],
      },
      {
        label: "Agent",
        tabs: [
          {
            id: "manager",
            label: "Manager",
            panel: () => <ManagerSettings readOnly={readOnly} />,
          },
          {
            id: "memory",
            label: "Memory",
            panel: () => <MemorySettings readOnly={readOnly} />,
          },
          {
            id: "rules",
            label: "Rules",
            panel: () => <RulesSettings readOnly={readOnly} />,
          },
          {
            id: "secrets",
            label: "Secrets",
            panel: () => <SecretsSettings readOnly={readOnly} />,
          },
        ],
      },
      {
        label: "Activity",
        tabs: [
          {
            id: "history",
            label: "History",
            panel: () => <HistorySettings />,
          },
          {
            id: "usage",
            label: "Usage",
            panel: () => <UsageSettings />,
          },
        ],
      },
    ],
    [readOnly],
  );

  const allTabs = useMemo(() => groups.flatMap((g) => g.tabs), [groups]);
  const requestedTab = new URLSearchParams(location.search).get("tab") ?? "";
  const initialTab = allTabs.some((t) => t.id === requestedTab)
    ? requestedTab
    : allTabs[0]?.id ?? "agents";
  const [activeId, setActiveId] = useState<string>(initialTab);
  useEffect(() => {
    if (requestedTab && allTabs.some((t) => t.id === requestedTab)) {
      setActiveId(requestedTab);
    }
  }, [requestedTab, allTabs]);
  const active = allTabs.find((t) => t.id === activeId) ?? allTabs[0];

  return (
    <div className="min-h-screen bg-[#f4f3ee]">
      <header className="flex items-center justify-between border-b border-slate-200/60 px-4 py-3 sm:px-6">
        <Link
          to="/"
          className="flex items-center gap-0.5 font-poppins text-sm font-semibold text-slate-950 transition-colors hover:text-slate-600"
        >
          <ChevronLeft className="h-4 w-4 text-slate-400" aria-hidden="true" />
          Back to chat
        </Link>
        <div className="text-xs text-slate-500">{who.email}</div>
      </header>

      <div className="mx-auto flex max-w-5xl gap-6 px-4 py-8 sm:px-6">
        <nav
          className="w-48 shrink-0"
          role="tablist"
          aria-orientation="vertical"
          aria-label="Settings sections"
        >
          <h1 className="px-3 pb-3 font-poppins text-lg font-semibold tracking-tight text-slate-950">
            Settings
          </h1>
          {groups.map((group, idx) => (
            <div key={group.label} className={idx > 0 ? "mt-5" : ""}>
              <div className="px-3 pb-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
                {group.label}
              </div>
              <div className="space-y-0.5">
                {group.tabs.map((tab) => (
                  <button
                    key={tab.id}
                    onClick={() => setActiveId(tab.id)}
                    role="tab"
                    aria-selected={activeId === tab.id}
                    className={`w-full rounded-md px-3 py-1.5 text-left text-xs font-medium transition ${
                      activeId === tab.id
                        ? "bg-slate-100 text-slate-950"
                        : "text-slate-500 hover:bg-slate-50 hover:text-slate-700"
                    }`}
                  >
                    {tab.label}
                  </button>
                ))}
              </div>
            </div>
          ))}

          <div className="mt-5 px-3 pb-1.5 text-[10px] font-medium uppercase tracking-widest text-slate-400">
            Account
          </div>
          <a
            href={PLATFORM_BASE + "/dashboard"}
            className="flex w-full items-center justify-between rounded-md px-3 py-1.5 text-left text-xs font-medium text-slate-500 transition hover:bg-slate-50 hover:text-slate-700"
          >
            <span>Plan & Billing</span>
            <ExternalLink className="h-3 w-3 text-slate-400" aria-hidden="true" />
          </a>
          <a
            href={
              PLATFORM_BASE +
              "/dashboard/account/api-keys?machine=" +
              machineSlug()
            }
            className="flex w-full items-center justify-between rounded-md px-3 py-1.5 text-left text-xs font-medium text-slate-500 transition hover:bg-slate-50 hover:text-slate-700"
          >
            <span>API keys</span>
            <ExternalLink className="h-3 w-3 text-slate-400" aria-hidden="true" />
          </a>
          <a
            href={PLATFORM_BASE + "/dashboard/account/cli"}
            className="flex w-full items-center justify-between rounded-md px-3 py-1.5 text-left text-xs font-medium text-slate-500 transition hover:bg-slate-50 hover:text-slate-700"
          >
            <span>Connect via CLI</span>
            <ExternalLink className="h-3 w-3 text-slate-400" aria-hidden="true" />
          </a>
          <a
            href={PLATFORM_BASE + "/dashboard/account/team"}
            className="flex w-full items-center justify-between rounded-md px-3 py-1.5 text-left text-xs font-medium text-slate-500 transition hover:bg-slate-50 hover:text-slate-700"
          >
            <span>Team</span>
            <ExternalLink className="h-3 w-3 text-slate-400" aria-hidden="true" />
          </a>
        </nav>

        <main
          className="min-h-[60vh] flex-1 rounded-2xl border border-slate-200/60 bg-white/50 px-6 py-6 shadow-sm"
          role="tabpanel"
          aria-label={active?.label}
        >
          {active ? active.panel() : null}
        </main>
      </div>
    </div>
  );
}
