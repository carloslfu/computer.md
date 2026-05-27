// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  CheckCircle2,
  XCircle,
  AlertCircle,
  ChevronDown,
  Loader2,
} from "lucide-react";
import { Streamdown } from "streamdown";
import "streamdown/styles.css";
import { apiFetch } from "../../api/fetch";
import { subscribeTaskEvents } from "../../lib/task-events";

// Single "History" surface — combines what used to be Activity and
// Audit Log. The non-technical user lands on the Tasks filter and sees
// a calm task narrative; audit rows are reachable but never in their
// face by default.
//
// Data sources:
//   GET /api/memory?category=activity → readable task narratives
//   GET /api/audit                    → structured privileged-operation log
//   GET /api/conversations            → titles for the "in chat: ..." chip

type ActivityItem = {
  id: string;
  category: string;
  key: string;
  value: string;
  metadata?: string | null;
  created_at: string;
  updated_at: string;
};

type AuditEntry = {
  id: string;
  timestamp: string;
  action: string;
  category: string;
  task_id?: string;
  details?: string;
  risk_level: string;
};

type Conversation = {
  id: string;
  title: string;
};

type FilterKey = "tasks" | "vault" | "guardrails" | "system" | "all";

// The /audit endpoint defaults to limit=100; on a busy machine that
// pushes real vault/guardrail rows out of the window. 1000 is the same
// limit the old dashboard used.
const AUDIT_FETCH_LIMIT = 1000;
const EARLIER_COLLAPSE_THRESHOLD = 20;
const PAGE_SIZE = 30;

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: "tasks", label: "Tasks" },
  { key: "vault", label: "Vault" },
  { key: "guardrails", label: "Guardrails" },
  { key: "system", label: "System" },
  { key: "all", label: "All" },
];

// Audit categories deliberately excluded from the UI (they're kept in
// the DB for forensics): `security` (one row per HTTP request) and
// `agent` (one row per tool call — up to 40 per task). Without this
// filter, the System / All views would be dominated by ~800 rows of
// jwt_access / tool_executed noise.
const AUDIT_CATEGORY_FILTER: Record<
  string,
  Exclude<FilterKey, "tasks" | "all">
> = {
  vault: "vault",
  guardrail: "guardrails",
  system: "system",
  routes: "system",
  chat: "system",
  memory: "system",
  task: "system",
};

const EMPTY_COPY: Record<FilterKey, string> = {
  tasks: "No tasks yet. Your agent's history will appear here as it completes tasks.",
  vault: "No vault operations yet. Credentials stored via the secure form will appear here.",
  guardrails:
    "No guardrail decisions yet. Allow, confirm, and block decisions on tool calls will appear here.",
  system:
    "No system events yet. Daemon startup, updates, and machine-level events will appear here.",
  all: "Nothing here yet.",
};

type ParsedMeta = {
  status?: string;
  duration_ms?: number;
  step_count?: number;
  conversation_id?: string;
};

type ParsedMarkdown = {
  task: string;
  status: string;
  duration: string;
  resultLine: string;
  whatIDid: string;
  notes: string;
  stepCount: number | null;
};

function parseMetadata(raw: string | null | undefined): ParsedMeta {
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object") return parsed as ParsedMeta;
  } catch {
    // malformed — drop silently
  }
  return {};
}

function parseMarkdown(raw: string): ParsedMarkdown {
  let task = "";
  let status = "";
  let duration = "";
  let resultLine = "";
  let stepCount: number | null = null;

  const stepMatch = raw.match(/<!--\s*steps:(\d+)\s*-->/);
  if (stepMatch) stepCount = Number.parseInt(stepMatch[1], 10);
  const stripped = raw.replace(/<!--\s*steps:\d+\s*-->\s*/g, "");

  const lines = stripped.split("\n");
  let inWhatIDid = false;
  let inNotes = false;
  const whatLines: string[] = [];
  const notesLines: string[] = [];

  for (const line of lines) {
    const trimmed = line.trim();
    if (trimmed.startsWith("Task:")) {
      task = trimmed.slice(5).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Status:")) {
      status = trimmed.slice(7).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Duration:")) {
      duration = trimmed.slice(9).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.startsWith("Result:")) {
      resultLine = trimmed.slice(7).trim();
      inWhatIDid = false;
      inNotes = false;
    } else if (trimmed.toLowerCase().startsWith("what i did")) {
      inWhatIDid = true;
      inNotes = false;
    } else if (trimmed.startsWith("Notes:")) {
      inWhatIDid = false;
      inNotes = true;
      const rest = trimmed.slice(6).trim();
      if (rest) notesLines.push(rest);
    } else if (inWhatIDid) {
      whatLines.push(line);
    } else if (inNotes) {
      notesLines.push(line);
    }
  }

  return {
    task,
    status,
    duration,
    resultLine,
    whatIDid: whatLines.join("\n").trim(),
    notes: notesLines.join("\n").trim(),
    stepCount,
  };
}

function statusIcon(status: string) {
  const lower = status.toLowerCase();
  if (lower === "completed") {
    return (
      <CheckCircle2
        className="h-3.5 w-3.5 text-emerald-600"
        aria-hidden="true"
      />
    );
  }
  if (lower === "failed") {
    return <XCircle className="h-3.5 w-3.5 text-red-500" aria-hidden="true" />;
  }
  if (lower === "running") {
    return (
      <Loader2
        className="h-3.5 w-3.5 animate-spin text-blue-500"
        aria-hidden="true"
      />
    );
  }
  return (
    <AlertCircle className="h-3.5 w-3.5 text-amber-500" aria-hidden="true" />
  );
}

function statusLabel(status: string) {
  const lower = status.toLowerCase();
  if (lower === "completed") return "Success";
  if (lower === "failed") return "Failed";
  if (lower === "cancelled") return "Cancelled";
  if (lower === "running") return "Running…";
  return status || "Success";
}

function dayBucket(ts: number, today: number): string {
  const yesterday = today - 24 * 60 * 60 * 1000;
  const sevenDays = today - 7 * 24 * 60 * 60 * 1000;
  if (ts >= today) return "Today";
  if (ts >= yesterday) return "Yesterday";
  if (ts >= sevenDays) return "Last week";
  return "Earlier";
}

function groupActivityByDay(
  items: ActivityItem[],
): Array<{ label: string; items: ActivityItem[] }> {
  const now = new Date();
  const today = new Date(
    now.getFullYear(),
    now.getMonth(),
    now.getDate(),
  ).getTime();
  const buckets: Record<string, ActivityItem[]> = {
    Today: [],
    Yesterday: [],
    "Last week": [],
    Earlier: [],
  };
  for (const item of items) {
    const ts = new Date(item.updated_at).getTime();
    if (!Number.isFinite(ts)) {
      buckets.Earlier.push(item);
      continue;
    }
    buckets[dayBucket(ts, today)].push(item);
  }
  return Object.entries(buckets)
    .filter(([, arr]) => arr.length > 0)
    .map(([label, arr]) => ({ label, items: arr }));
}

const RISK_COLORS: Record<string, string> = {
  low: "text-green-500",
  medium: "text-yellow-500",
  high: "text-red-500",
};

function TaskRow({
  item,
  expanded,
  onToggle,
  chatTitle,
}: {
  item: ActivityItem;
  expanded: boolean;
  onToggle: () => void;
  chatTitle?: string;
}) {
  const meta = parseMetadata(item.metadata);
  const parsed = parseMarkdown(item.value);
  const time = new Date(item.updated_at).toLocaleTimeString("en-US", {
    hour: "numeric",
    minute: "2-digit",
  });
  const status = parsed.status || meta.status || "";
  const duration = parsed.duration;
  const stepCount =
    parsed.stepCount ??
    (typeof meta.step_count === "number" ? meta.step_count : null);

  return (
    <button
      onClick={onToggle}
      aria-expanded={expanded}
      className="block w-full rounded-lg border border-transparent px-3 py-2 text-left transition hover:bg-white/30"
    >
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="truncate text-xs font-medium text-slate-700">
            {parsed.task || parsed.resultLine || "Task"}
          </p>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-slate-400">
            <span>{time}</span>
            <span aria-hidden="true">·</span>
            <span className="inline-flex items-center gap-1">
              {statusIcon(status)}
              {statusLabel(status)}
            </span>
            {duration && (
              <>
                <span aria-hidden="true">·</span>
                <span>{duration}</span>
              </>
            )}
            {stepCount !== null && stepCount > 0 && (
              <>
                <span aria-hidden="true">·</span>
                <span>{stepCount} steps</span>
              </>
            )}
          </div>
          {chatTitle && (
            <p className="mt-0.5 truncate text-[11px] text-slate-400">
              in{" "}
              <span className="text-slate-500">&ldquo;{chatTitle}&rdquo;</span>
            </p>
          )}
        </div>
        <ChevronDown
          className={`h-3.5 w-3.5 flex-shrink-0 text-slate-400 transition-transform ${
            expanded ? "rotate-180" : ""
          }`}
          aria-hidden="true"
        />
      </div>
      {expanded && (
        <div className="mt-3 space-y-2 border-t border-slate-200/60 pt-2">
          {parsed.resultLine && (
            <p className="text-xs text-slate-700">{parsed.resultLine}</p>
          )}
          {parsed.whatIDid && (
            <div className="chat-markdown text-[11px] text-slate-600">
              <Streamdown>{parsed.whatIDid}</Streamdown>
            </div>
          )}
          {parsed.notes && (
            <p className="text-[11px] italic text-slate-500">
              <span className="font-medium">Notes: </span>
              {parsed.notes}
            </p>
          )}
        </div>
      )}
    </button>
  );
}

function AuditRow({
  entry,
  expanded,
  onToggle,
}: {
  entry: AuditEntry;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <div>
      <button
        onClick={onToggle}
        aria-expanded={expanded}
        className="flex w-full items-center justify-between rounded-lg px-3 py-2 text-left transition hover:bg-white/30"
      >
        <div className="flex-1">
          <div className="flex items-center gap-2">
            <span className="text-xs font-medium text-slate-600">
              {entry.action}
            </span>
            <span className="text-[10px] text-slate-400">{entry.category}</span>
            <span
              className={`text-[10px] ${
                RISK_COLORS[entry.risk_level] || "text-slate-400"
              }`}
            >
              {entry.risk_level}
            </span>
          </div>
          <p className="text-[10px] text-slate-400">
            {new Date(entry.timestamp).toLocaleString()}
          </p>
        </div>
        <ChevronDown
          className={`h-3.5 w-3.5 text-slate-400 transition-transform ${
            expanded ? "rotate-180" : ""
          }`}
          aria-hidden="true"
        />
      </button>
      {expanded && (entry.details || entry.task_id) && (
        <div className="mb-2 ml-3 rounded-lg bg-white/30 p-3 text-xs">
          {entry.details && <p className="text-slate-500">{entry.details}</p>}
          {entry.task_id && (
            <p className="mt-1 text-slate-400">Task: {entry.task_id}</p>
          )}
        </div>
      )}
    </div>
  );
}

function PaginatedList<T>({
  items,
  visibleCount,
  onShowMore,
  renderItem,
  keyFor,
}: {
  items: T[];
  visibleCount: number;
  onShowMore: () => void;
  renderItem: (item: T) => React.ReactNode;
  keyFor: (item: T) => string;
}) {
  const visible = items.slice(0, visibleCount);
  const remaining = items.length - visible.length;
  return (
    <div className="space-y-1">
      {visible.map((item, idx) => (
        <div
          key={keyFor(item)}
          className={
            idx >= visibleCount - PAGE_SIZE && visibleCount > PAGE_SIZE
              ? "animate-fade-in"
              : ""
          }
        >
          {renderItem(item)}
        </div>
      ))}
      {remaining > 0 && (
        <button
          type="button"
          onClick={onShowMore}
          className="mt-2 w-full rounded-lg border border-dashed border-slate-200/80 px-3 py-2.5 text-center text-xs text-slate-500 transition hover:border-slate-300 hover:bg-white/40 hover:text-slate-700"
        >
          Show {Math.min(remaining, PAGE_SIZE)} more
          {remaining > PAGE_SIZE && (
            <span className="ml-1 text-[10px] text-slate-400">
              · {remaining} total
            </span>
          )}
        </button>
      )}
    </div>
  );
}

export function HistorySettings() {
  const [filter, setFilter] = useState<FilterKey>("tasks");
  const [activity, setActivity] = useState<ActivityItem[]>([]);
  const [audit, setAudit] = useState<AuditEntry[]>([]);
  const [earlierExpanded, setEarlierExpanded] = useState(false);
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const activeFetchRef = useRef<AbortController | null>(null);

  const refetch = useCallback(() => {
    activeFetchRef.current?.abort();
    const controller = new AbortController();
    activeFetchRef.current = controller;

    setLoading(true);
    Promise.allSettled([
      apiFetch("/api/memory?category=activity", {
        signal: controller.signal,
      }).then((r) =>
        r.ok ? r.json() : Promise.reject(new Error("activity")),
      ),
      apiFetch(`/api/audit?limit=${AUDIT_FETCH_LIMIT}`, {
        signal: controller.signal,
      }).then((r) => (r.ok ? r.json() : Promise.reject(new Error("audit")))),
      apiFetch("/api/conversations", {
        signal: controller.signal,
      }).then((r) =>
        r.ok ? r.json() : Promise.reject(new Error("conversations")),
      ),
    ])
      .then(([actRes, audRes, convRes]) => {
        if (controller.signal.aborted) return;
        if (actRes.status === "fulfilled" && Array.isArray(actRes.value)) {
          setActivity(actRes.value);
        }
        if (audRes.status === "fulfilled" && Array.isArray(audRes.value)) {
          setAudit(audRes.value);
        }
        if (convRes.status === "fulfilled" && Array.isArray(convRes.value)) {
          setConversations(convRes.value);
        }
        if (
          actRes.status === "rejected" &&
          audRes.status === "rejected" &&
          convRes.status === "rejected"
        ) {
          setError("Failed to load history");
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
  }, []);

  useEffect(() => {
    refetch();
    return () => {
      activeFetchRef.current?.abort();
    };
  }, [refetch]);

  // Refetch on task lifecycle SSE events, debounced so back-to-back
  // task:started + task:activity emissions don't double-refetch.
  useEffect(() => {
    let pending: ReturnType<typeof setTimeout> | null = null;
    const unsubscribe = subscribeTaskEvents(() => {
      if (pending) clearTimeout(pending);
      pending = setTimeout(() => {
        pending = null;
        refetch();
      }, 300);
    });
    return () => {
      unsubscribe();
      if (pending) clearTimeout(pending);
    };
  }, [refetch]);

  const visibleAudit = useMemo(
    () =>
      audit.filter((e) => AUDIT_CATEGORY_FILTER[e.category] !== undefined),
    [audit],
  );

  const filteredAudit = useMemo(() => {
    if (filter === "tasks") return [];
    if (filter === "all") return visibleAudit;
    return visibleAudit.filter(
      (e) => AUDIT_CATEGORY_FILTER[e.category] === filter,
    );
  }, [visibleAudit, filter]);

  const allItems = useMemo(() => {
    if (filter !== "all") return [];
    type Combined =
      | { kind: "task"; ts: number; item: ActivityItem }
      | { kind: "audit"; ts: number; entry: AuditEntry };
    const rows: Combined[] = [];
    for (const a of activity)
      rows.push({
        kind: "task",
        ts: new Date(a.updated_at).getTime(),
        item: a,
      });
    for (const e of visibleAudit)
      rows.push({
        kind: "audit",
        ts: new Date(e.timestamp).getTime(),
        entry: e,
      });
    return rows.sort((a, b) => b.ts - a.ts);
  }, [activity, visibleAudit, filter]);

  const toggleExpanded = (id: string) =>
    setExpanded((prev) => ({ ...prev, [id]: !prev[id] }));

  useEffect(() => {
    setEarlierExpanded(false);
    setVisibleCount(PAGE_SIZE);
  }, [filter]);

  const showMore = () => setVisibleCount((c) => c + PAGE_SIZE);

  const conversationsById = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of conversations) m.set(c.id, c.title);
    return m;
  }, [conversations]);
  const showChatChip = conversations.length > 1;

  const chatTitleFor = (item: ActivityItem): string | undefined => {
    if (!showChatChip) return undefined;
    const meta = parseMetadata(item.metadata);
    const cid = meta.conversation_id;
    if (!cid) return undefined;
    return conversationsById.get(cid);
  };

  const counts: Record<FilterKey, number> = {
    tasks: activity.length,
    vault: visibleAudit.filter(
      (e) => AUDIT_CATEGORY_FILTER[e.category] === "vault",
    ).length,
    guardrails: visibleAudit.filter(
      (e) => AUDIT_CATEGORY_FILTER[e.category] === "guardrails",
    ).length,
    system: visibleAudit.filter(
      (e) => AUDIT_CATEGORY_FILTER[e.category] === "system",
    ).length,
    all: activity.length + visibleAudit.length,
  };

  return (
    <div>
      <h3 className="font-poppins text-base font-semibold text-slate-950">
        History
      </h3>
      <p className="mt-1.5 text-sm leading-relaxed text-slate-500">
        Everything the agent has done on this computer — tasks, credential
        operations, guardrail decisions, and system events.
      </p>

      <div
        role="tablist"
        aria-label="History filter"
        className="mt-5 flex flex-wrap gap-1.5"
      >
        {FILTERS.map((f) => {
          const active = filter === f.key;
          const count = counts[f.key];
          return (
            <button
              key={f.key}
              type="button"
              role="tab"
              aria-selected={active}
              onClick={() => setFilter(f.key)}
              className={`inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium transition ${
                active
                  ? "bg-slate-950 text-white"
                  : "bg-slate-100 text-slate-600 hover:bg-slate-200"
              }`}
            >
              {f.label}
              {count > 0 && (
                <span
                  className={`text-[10px] ${
                    active ? "text-white/70" : "text-slate-400"
                  }`}
                >
                  {count}
                </span>
              )}
            </button>
          );
        })}
      </div>

      {error && (
        <p className="mt-4 text-xs text-red-500" role="alert">
          {error}
        </p>
      )}

      {loading && activity.length === 0 && audit.length === 0 && (
        <p className="mt-6 py-6 text-center text-xs text-slate-400">
          Loading history…
        </p>
      )}

      <div className="mt-4">
        {filter === "tasks" &&
          (activity.length === 0
            ? !loading && (
                <p className="py-6 text-center text-xs text-slate-400">
                  {EMPTY_COPY.tasks}
                </p>
              )
            : groupActivityByDay(activity).map(
                ({ label, items: groupItems }, idx) => {
                  const isEarlier = label === "Earlier";
                  const shouldCollapse =
                    isEarlier &&
                    !earlierExpanded &&
                    groupItems.length > EARLIER_COLLAPSE_THRESHOLD;
                  return (
                    <div key={label} className={idx > 0 ? "mt-5" : ""}>
                      <div className="mb-2 border-b border-slate-200/60 pb-1.5 text-xs font-medium uppercase tracking-widest text-slate-500">
                        {label}
                      </div>
                      {shouldCollapse ? (
                        <button
                          type="button"
                          onClick={() => setEarlierExpanded(true)}
                          className="w-full rounded-lg border border-dashed border-slate-200/80 px-3 py-3 text-center text-xs text-slate-500 transition hover:border-slate-300 hover:bg-white/40 hover:text-slate-700"
                        >
                          Show {groupItems.length} earlier task
                          {groupItems.length === 1 ? "" : "s"}
                        </button>
                      ) : (
                        <div className="space-y-1">
                          {groupItems.map((item) => (
                            <div
                              key={item.id}
                              className={
                                isEarlier && earlierExpanded
                                  ? "animate-fade-in"
                                  : ""
                              }
                            >
                              <TaskRow
                                item={item}
                                expanded={!!expanded[item.id]}
                                onToggle={() => toggleExpanded(item.id)}
                                chatTitle={chatTitleFor(item)}
                              />
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                },
              ))}

        {(filter === "vault" ||
          filter === "guardrails" ||
          filter === "system") &&
          (filteredAudit.length === 0
            ? !loading && (
                <p className="py-6 text-center text-xs text-slate-400">
                  {EMPTY_COPY[filter]}
                </p>
              )
            : (
                <PaginatedList
                  items={filteredAudit}
                  visibleCount={visibleCount}
                  onShowMore={showMore}
                  renderItem={(entry) => (
                    <AuditRow
                      entry={entry}
                      expanded={!!expanded[entry.id]}
                      onToggle={() => toggleExpanded(entry.id)}
                    />
                  )}
                  keyFor={(entry) => entry.id}
                />
              ))}

        {filter === "all" &&
          (allItems.length === 0
            ? !loading && (
                <p className="py-6 text-center text-xs text-slate-400">
                  {EMPTY_COPY.all}
                </p>
              )
            : (
                <PaginatedList
                  items={allItems}
                  visibleCount={visibleCount}
                  onShowMore={showMore}
                  renderItem={(row) =>
                    row.kind === "task" ? (
                      <TaskRow
                        item={row.item}
                        expanded={!!expanded[row.item.id]}
                        onToggle={() => toggleExpanded(row.item.id)}
                        chatTitle={chatTitleFor(row.item)}
                      />
                    ) : (
                      <AuditRow
                        entry={row.entry}
                        expanded={!!expanded[row.entry.id]}
                        onToggle={() => toggleExpanded(row.entry.id)}
                      />
                    )
                  }
                  keyFor={(row) =>
                    row.kind === "task" ? row.item.id : row.entry.id
                  }
                />
              ))}
      </div>
    </div>
  );
}
