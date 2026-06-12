// SPDX-License-Identifier: Apache-2.0

import { useEffect, useMemo, useState } from "react";
import { apiFetch } from "../../api/fetch";
import { PLATFORM_BASE } from "../../auth/types";

// Usage panel.
//
// Joins two sources of truth at render time:
//   - Daemon /api/usage: per-machine consumption, billed at cost. Real
//     tokens, real cost in USD. Source: this machine's usage_records.
//   - Platform /api/plan: user-scoped plan + usage-credit budget +
//     billing period boundaries (from the Stripe subscription when
//     available; calendar month otherwise).
//
// The platform sources the budget and the period; the daemon sources
// the spend. Joined here so the user sees "$X.XX against $Y available"
// against the period that's actually being billed.
//
// Why the join lives in the SPA rather than on the platform: keeping
// usage on the daemon means the customer never has to trust that
// VibeCraft is accurately reading their consumption — it's stored on
// their machine, in the same encrypted DB as their chat history. The
// "bills at cost" promise becomes inspectable.

type ModelBreakdown = {
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  cost_usd: number;
};

type DayBreakdown = {
  day: string;
  cost_usd: number;
};

type ConvoBreakdown = {
  conversation_id: string;
  title: string;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
};

// BudgetState is the daemon's authoritative enforcement verdict — the
// daemon is the enforcer, so the UI displays its verdict rather than
// recomputing paused-ness from plan + spend.
type BudgetState = {
  budget_usd: number;
  usage_budget_usd?: number;
  monthly_usage_cap_usd?: number;
  cap_reached_reason?: string | null;
  enforced: boolean;
  paused: boolean;
  spent_usd: number;
  resets_on: string;
};

type Summary = {
  period: { start: string; end: string };
  total_cost_usd: number;
  unpriced_models?: string[];
  by_model: ModelBreakdown[];
  by_day: DayBreakdown[];
  top_conversations: ConvoBreakdown[];
  budget_state?: BudgetState;
};

type Plan = {
  plan_name: string | null;
  ai_budget_usd?: number;
  usage_budget_usd?: number;
  remaining_usage_usd?: number;
  monthly_usage_cap_usd?: number;
  included_usage_credit_usd?: number;
  usage_credit_balance_usd?: number;
  committed_resource_usd?: number;
  cap_reached_reason?: string | null;
  period_start: string;
  period_end: string;
  period_source: "subscription" | "calendar_month";
};

const MODEL_LABEL: Record<string, string> = {
  "gpt-5.4-mini": "VibeCraft manager",
  "gpt-5.4-mini-2026-03-17": "VibeCraft manager",
  "gpt-5.4": "VibeCraft manager",
};

function modelLabel(model: string): string {
  return MODEL_LABEL[model] ?? model;
}

function formatUSD(n: number, decimals = 2): string {
  return n.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: decimals,
    maximumFractionDigits: decimals,
  });
}

function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return n.toLocaleString();
}

// Parse YYYY-MM-DD into a UTC Date so all arithmetic happens in a
// time-zone-agnostic frame. We display "days remaining" as a calendar
// figure, not "hours since the period flipped at midnight UTC."
function parseYMD(s: string): Date {
  const [y, m, d] = s.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d));
}

// Days between today (UTC) and the period end, inclusive of end.
// Returns 0 when end is today or past.
function daysRemaining(periodEnd: string): number {
  const end = parseYMD(periodEnd);
  const now = new Date();
  const today = new Date(
    Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()),
  );
  const diffDays = Math.floor(
    (end.getTime() - today.getTime()) / (24 * 60 * 60 * 1000),
  );
  return Math.max(0, diffDays);
}

// Total calendar days in the period (inclusive of both endpoints).
function periodLengthDays(start: string, end: string): number {
  const s = parseYMD(start);
  const e = parseYMD(end);
  return (
    Math.floor((e.getTime() - s.getTime()) / (24 * 60 * 60 * 1000)) + 1
  );
}

// Days elapsed so far in the period (1 = first day, capped at length).
function periodDaysElapsed(start: string): number {
  const s = parseYMD(start);
  const now = new Date();
  const today = new Date(
    Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()),
  );
  const diffDays =
    Math.floor((today.getTime() - s.getTime()) / (24 * 60 * 60 * 1000)) + 1;
  return Math.max(1, diffDays);
}

// Projected end-of-period spend at the current pace. Conservative —
// only meaningful after a few days of data, so callers gate on
// elapsed >= 3 before surfacing.
function projectedPeriodTotal(
  totalCost: number,
  start: string,
  end: string,
): number {
  const elapsed = periodDaysElapsed(start);
  const length = periodLengthDays(start, end);
  if (elapsed <= 0) return totalCost;
  return (totalCost / elapsed) * length;
}

function formatPeriodLabel(p: Plan): string {
  // "May 1 – May 31, 2026" — readable, not the YYYY-MM-DD form. We
  // need to localize this through the browser to handle hyphen vs
  // en-dash + month abbreviation conventions, but stay timezone-agnostic
  // by using UTC parse + UTC formatter.
  const start = parseYMD(p.period_start);
  const end = parseYMD(p.period_end);
  const fmt: Intl.DateTimeFormatOptions = {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  };
  const startStr = start.toLocaleDateString("en-US", fmt);
  const endStr = end.toLocaleDateString("en-US", {
    ...fmt,
    year: "numeric",
  });
  return `${startStr} – ${endStr}`;
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="mb-2 text-[10px] font-medium uppercase tracking-widest text-slate-400">
      {children}
    </div>
  );
}

function DailyBars({
  data,
  max,
  periodStart,
  periodEnd,
}: {
  data: DayBreakdown[];
  max: number;
  periodStart: string;
  periodEnd: string;
}) {
  // Inline SVG bar chart — no chart library. The chart is laid out
  // against the full period grid (May 2 – Jun 2 = 32 day-slots), with
  // bars rendered only on days that have data. This is the right
  // shape: a single day of $0.61 spend on day 20 should take 1/32 of
  // the chart width at the day-20 position, not stretch across the
  // whole canvas like a "total bar."
  //
  // Doing the layout this way also makes the chart instantly readable:
  // the user sees WHERE in the cycle their spend happened, not just
  // the (n=1) bar shape that "data.length = 1" would produce.
  if (data.length === 0) return null;

  const periodLen = periodLengthDays(periodStart, periodEnd);
  if (periodLen <= 0) return null;

  const w = 480;
  const h = 80;
  const padTop = 4;
  const padBottom = 14;
  const innerH = h - padTop - padBottom;
  const slot = w / periodLen;
  const barW = Math.max(2, slot - 2);

  // Map day-of-period (0..periodLen-1) → bar from data, if any.
  const startMs = parseYMD(periodStart).getTime();

  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      preserveAspectRatio="none"
      className="h-20 w-full"
      role="img"
      aria-label="Daily spend chart"
    >
      {data.map((d) => {
        const idx = Math.round(
          (parseYMD(d.day).getTime() - startMs) / (24 * 60 * 60 * 1000),
        );
        if (idx < 0 || idx >= periodLen) return null; // defensive
        const ratio = max > 0 ? d.cost_usd / max : 0;
        const barH = Math.max(1, ratio * innerH);
        const x = slot * idx + 1;
        const y = padTop + innerH - barH;
        return (
          <rect
            key={d.day}
            x={x}
            y={y}
            width={barW}
            height={barH}
            rx={1}
            className="fill-slate-700"
          >
            <title>
              {d.day}: {formatUSD(d.cost_usd, 4)}
            </title>
          </rect>
        );
      })}
    </svg>
  );
}

// formatPercent renders the budget consumed as an honest percent label
// for the hero. Any non-zero spend that rounds to 0% becomes "<1%" so
// the customer never sees "$0.61 of $200 — 0% used" (a fact that
// arithmetically rounds correctly but reads as broken).
function formatPercent(p: number): string {
  if (p <= 0) return "0%";
  if (p < 1) return "<1%";
  return `${Math.round(p)}%`;
}

export function UsageSettings() {
  const [plan, setPlan] = useState<Plan | null>(null);
  const [planError, setPlanError] = useState(false);
  const [summary, setSummary] = useState<Summary | null>(null);
  const [usageError, setUsageError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // Fetch plan first so we can pass its period to /api/usage. Falls
  // back to calendar-month on the daemon side if the plan call fails,
  // so the panel still renders something useful (just without budget
  // framing).
  useEffect(() => {
    let cancelled = false;
    (async () => {
      let p: Plan | null = null;
      try {
        // eslint-disable-next-line no-restricted-syntax -- cross-origin call to the platform; apiFetch is daemon-scoped and would attach the wrong credentials
        const res = await fetch(PLATFORM_BASE + "/api/plan", {
          credentials: "include",
        });
        if (res.ok) {
          p = (await res.json()) as Plan;
          if (!cancelled) setPlan(p);
        } else {
          if (!cancelled) setPlanError(true);
        }
      } catch {
        if (!cancelled) setPlanError(true);
      }

      // Now usage. Use the plan's period when we got one; otherwise
      // let the daemon default to calendar month.
      try {
        const qs =
          p && p.period_start && p.period_end
            ? `?start=${encodeURIComponent(p.period_start)}&end=${encodeURIComponent(p.period_end)}`
            : "";
        const res = await apiFetch("/api/usage" + qs);
        if (!res.ok) {
          if (!cancelled) {
            setUsageError(
              res.status === 503
                ? "Usage tracking isn't available on this daemon yet — upgrade required."
                : "Could not load usage from this computer.",
            );
            setLoading(false);
          }
          return;
        }
        const data = (await res.json()) as Summary;
        if (!cancelled) {
          setSummary(data);
          setLoading(false);
        }
      } catch {
        if (!cancelled) {
          setUsageError("Could not load usage from this computer.");
          setLoading(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const maxDay = useMemo(() => {
    if (!summary) return 0;
    return summary.by_day.reduce((m, d) => (d.cost_usd > m ? d.cost_usd : m), 0);
  }, [summary]);

  const hasUsage = summary !== null && summary.total_cost_usd > 0;
  const budget =
    plan?.usage_budget_usd ?? plan?.remaining_usage_usd ?? plan?.ai_budget_usd ?? 0;
  const monthlyCap = plan?.monthly_usage_cap_usd ?? 0;
  const hasCustomTerms = budget === -1;
  const pctUsed =
    hasUsage && budget > 0
      ? Math.min(100, (summary!.total_cost_usd / budget) * 100)
      : 0;

  // Pace projection only when we have a real subscription period, at
  // least 3 elapsed days of data, ≥2 days remaining to project into,
  // AND a non-zero total to project from. "At this pace, about $0.00
  // this cycle" is mathematically correct but useless copy — gate it
  // out so the empty state card carries the whole "no usage yet"
  // message without a self-defeating projection above it.
  const showPace =
    plan?.period_source === "subscription" &&
    summary !== null &&
    summary.total_cost_usd > 0 &&
    periodDaysElapsed(plan.period_start) >= 3 &&
    daysRemaining(plan.period_end) >= 2;

  return (
    <div>
      <h3 className="font-poppins text-base font-semibold text-slate-950">
        Usage
      </h3>
      <p className="mt-1.5 text-sm leading-relaxed text-slate-500">
        Per-machine AI consumption from the manager and hosted tools when they
        use VibeCraft-metered routes. Usage credit is billed at cost.
      </p>

      {loading && (
        <div className="mt-8 space-y-3">
          {[0, 1, 2].map((i) => (
            <div
              key={i}
              className="h-16 animate-pulse rounded-xl border border-slate-200/60 bg-white/40"
            />
          ))}
        </div>
      )}

      {!loading && usageError && (
        <div className="mt-8 rounded-xl border border-slate-200/60 bg-white/50 p-4 text-sm text-slate-500">
          {usageError}
        </div>
      )}

      {!loading && !usageError && summary && (
        <div className="mt-8 space-y-8">
          {/* ── Budget-reached banner ───────────────────────────────────
              Authoritative — driven by the daemon's BudgetState, not a
              client-side recompute. The daemon is the enforcer; when it
              says paused, new tasks are genuinely being held. */}
          {summary.budget_state?.paused && (
            <div
              role="alert"
              className="rounded-2xl border border-amber-300/80 bg-amber-50 p-5"
            >
              <p className="font-poppins text-sm font-semibold text-amber-900">
                Usage credit reached
              </p>
              <p className="mt-1.5 text-sm leading-relaxed text-amber-800">
                You&apos;ve used your available{" "}
                {formatUSD(summary.budget_state.usage_budget_usd ?? summary.budget_state.budget_usd, 0)} VibeCraft usage credit. New tasks are
                paused until your billing cycle renews on{" "}
                <span className="font-medium">
                  {summary.budget_state.resets_on}
                </span>
                , or until the account is topped up. Anything already running
                finishes normally.
              </p>
            </div>
          )}

          {/* ── Hero: this period ───────────────────────────────────── */}
          <section>
            <div className="rounded-2xl border border-slate-200/60 bg-white/50 p-6">
              <div className="flex items-baseline justify-between text-xs">
                <span className="text-slate-500">
                  {plan?.period_source === "subscription"
                    ? "This billing cycle"
                    : "This month"}
                  {plan && (
                    <span className="ml-2 text-slate-400">
                      {formatPeriodLabel(plan)}
                    </span>
                  )}
                </span>
                <span className="text-slate-500">
                  {planError
                    ? "Plan unavailable"
                    : hasCustomTerms
                      ? "Custom plan"
                      : plan
                        ? `${formatUSD(budget, 0)} available`
                        : "—"}
                </span>
              </div>

              <div className="mt-3 flex items-baseline gap-2">
                <span className="font-poppins text-3xl font-semibold text-slate-950">
                  {formatUSD(summary.total_cost_usd)}
                </span>
                {!hasCustomTerms && plan && budget > 0 && (
                  <span className="text-sm text-slate-500">
                    against {formatUSD(budget, 0)}
                  </span>
                )}
              </div>

              {!hasCustomTerms && plan && budget > 0 && (
                <div className="mt-4">
                  <div
                    className="h-1.5 overflow-hidden rounded-full bg-slate-100"
                    role="progressbar"
                    aria-valuenow={Math.round(pctUsed)}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-label="Budget consumed"
                  >
                    <div
                      className={`h-full rounded-full transition-all duration-300 ${
                        pctUsed >= 90
                          ? "bg-red-500"
                          : pctUsed >= 70
                            ? "bg-amber-500"
                            : "bg-emerald-500"
                      }`}
                      style={{ width: `${pctUsed}%` }}
                    />
                  </div>
                  <div className="mt-2 flex items-center justify-between text-[11px] text-slate-400">
                    <span>{formatPercent(pctUsed)} used</span>
                    {plan.period_source === "subscription" && (
                      <span>
                        {daysRemaining(plan.period_end)} day
                        {daysRemaining(plan.period_end) === 1 ? "" : "s"} left
                      </span>
                    )}
                  </div>
                  {showPace && (
                    <p className="mt-3 text-xs text-slate-500">
                      At this pace, about{" "}
                      <span className="font-medium text-slate-700">
                        {formatUSD(
                          projectedPeriodTotal(
                            summary.total_cost_usd,
                            plan.period_start,
                            plan.period_end,
                          ),
                        )}
                      </span>{" "}
                      this {plan.period_source === "subscription" ? "cycle" : "month"}.
                    </p>
                  )}
                  {monthlyCap > 0 && (
                    <p className="mt-2 text-xs text-slate-500">
                      Account cap:{" "}
                      <span className="font-medium text-slate-700">
                        {formatUSD(monthlyCap, 0)}
                      </span>{" "}
                      this cycle.
                    </p>
                  )}
                </div>
              )}

              {hasCustomTerms && (
                <p className="mt-3 text-xs text-slate-500">
                  This account uses custom metered terms. Spend is tracked and
                  reconciled separately.
                </p>
              )}
            </div>
          </section>

          {/* ── Empty state when nothing has been used yet ──────────── */}
          {!hasUsage && (
            <div className="rounded-xl border border-dashed border-slate-200/80 bg-white/30 p-6 text-center">
              <p className="text-sm text-slate-500">
                No metered AI consumption yet this period.
              </p>
              <p className="mt-1.5 text-xs text-slate-400">
                Send the manager a task to get started. Token spend will
                appear here in real time.
              </p>
            </div>
          )}

          {/* ── Unpriced model warning ──────────────────────────────── */}
          {summary.unpriced_models && summary.unpriced_models.length > 0 && (
            <div
              role="alert"
              className="rounded-xl border border-amber-200/80 bg-amber-50/60 p-3 text-xs text-amber-900"
            >
              Some recent calls used a model without pricing in this build:{" "}
              <span className="font-mono">
                {summary.unpriced_models.join(", ")}
              </span>
              . The amount shown excludes their cost. Update the daemon to
              get a complete total.
            </div>
          )}

          {/* ── By-model breakdown ──────────────────────────────────── */}
          {hasUsage && summary.by_model.length > 0 && (
            <section>
              <SectionLabel>By model</SectionLabel>
              <div className="divide-y divide-slate-200/60 rounded-xl border border-slate-200/60 bg-white/50">
                {summary.by_model.map((m) => (
                  <div key={m.model} className="flex items-start justify-between gap-4 px-4 py-3">
                    <div className="min-w-0">
                      <p className="text-sm font-medium text-slate-700">
                        {modelLabel(m.model)}
                      </p>
                      <p className="mt-0.5 text-[11px] text-slate-400">
                        {formatTokens(m.input_tokens)} input ·{" "}
                        {formatTokens(m.output_tokens)} output
                        {m.cache_read_tokens > 0 && (
                          <> · {formatTokens(m.cache_read_tokens)} cached</>
                        )}
                      </p>
                    </div>
                    <span className="text-sm font-medium text-slate-700">
                      {formatUSD(m.cost_usd)}
                    </span>
                  </div>
                ))}
              </div>
            </section>
          )}

          {/* ── Top conversations ───────────────────────────────────── */}
          {hasUsage && summary.top_conversations.length > 0 && (
            <section>
              <SectionLabel>Top chats</SectionLabel>
              <div className="divide-y divide-slate-200/60 rounded-xl border border-slate-200/60 bg-white/50">
                {summary.top_conversations.map((c) => (
                  <div
                    key={c.conversation_id}
                    className="flex items-start justify-between gap-4 px-4 py-3"
                  >
                    <div className="min-w-0">
                      <p className="truncate text-sm font-medium text-slate-700">
                        {c.title || "Untitled chat"}
                      </p>
                      <p className="mt-0.5 text-[11px] text-slate-400">
                        {formatTokens(c.input_tokens)} in ·{" "}
                        {formatTokens(c.output_tokens)} out
                      </p>
                    </div>
                    <span className="text-sm font-medium text-slate-700">
                      {formatUSD(c.cost_usd)}
                    </span>
                  </div>
                ))}
              </div>
            </section>
          )}

          {/* ── Daily trend ─────────────────────────────────────────── */}
          {hasUsage && summary.by_day.length > 0 && (
            <section>
              <SectionLabel>Daily</SectionLabel>
              <div className="rounded-xl border border-slate-200/60 bg-white/50 p-4">
                <DailyBars
                  data={summary.by_day}
                  max={maxDay}
                  periodStart={summary.period.start}
                  periodEnd={summary.period.end}
                />
                <div className="mt-2 flex justify-between text-[10px] text-slate-400">
                  <span>{summary.period.start}</span>
                  <span>{summary.period.end}</span>
                </div>
              </div>
            </section>
          )}

          {/* ── Footer — restate the trust framing ──────────────────── */}
          <p className="text-[11px] text-slate-400">
            VibeCraft-metered AI is billed at cost. OpenAI publishes current API rates at{" "}
            <a
              href="https://developers.openai.com/api/docs/pricing"
              target="_blank"
              rel="noopener noreferrer"
              className="text-slate-500 underline decoration-slate-300 underline-offset-2 hover:text-slate-700"
            >
              developers.openai.com/api/docs/pricing
            </a>
            . Managed infrastructure reservations and account-wide credit live
            in the billing dashboard. BYOM own-key manager calls and workers
            (Claude Code, Codex) don&apos;t appear here.
          </p>
        </div>
      )}
    </div>
  );
}
