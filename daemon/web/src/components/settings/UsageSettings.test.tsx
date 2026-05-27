// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { UsageSettings } from "./UsageSettings";

// fetchMock matches the per-call URL prefix and returns the configured
// response. Cleaner than a single big handler — each test states what
// the platform plan + daemon usage look like.
type MockResponses = {
  plan?: {
    ok?: boolean;
    status?: number;
    body?: object;
  };
  usage?: {
    ok?: boolean;
    status?: number;
    body?: object;
  };
};

function installFetchMock(responses: MockResponses) {
  const planRes = responses.plan ?? { ok: true, body: {} };
  const usageRes = responses.usage ?? { ok: true, body: {} };

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.includes("/api/plan")) {
      return new Response(JSON.stringify(planRes.body ?? {}), {
        status: planRes.status ?? (planRes.ok === false ? 500 : 200),
      });
    }
    if (url.includes("/api/usage")) {
      return new Response(JSON.stringify(usageRes.body ?? {}), {
        status: usageRes.status ?? (usageRes.ok === false ? 500 : 200),
      });
    }
    return new Response("{}", { status: 404 });
  }) as unknown as typeof fetch;
}

const fullPlan = {
  plan_name: "Production",
  ai_budget_usd: 200,
  period_start: "2026-05-01",
  period_end: "2026-05-31",
  period_source: "subscription" as const,
};

const calendarPlan = {
  plan_name: "Starter",
  ai_budget_usd: 50,
  period_start: "2026-05-01",
  period_end: "2026-05-31",
  period_source: "calendar_month" as const,
};

const enterprisePlan = {
  plan_name: "Enterprise",
  ai_budget_usd: -1,
  period_start: "2026-05-01",
  period_end: "2026-05-31",
  period_source: "subscription" as const,
};

const sampleSummary = {
  period: { start: "2026-05-01", end: "2026-05-31" },
  total_cost_usd: 23.45,
  by_model: [
    {
      model: "gpt-5.4-mini",
      input_tokens: 1_200_000,
      output_tokens: 80_000,
      cache_read_tokens: 500_000,
      cache_create_tokens: 100_000,
      cost_usd: 21.55,
    },
    {
      model: "gpt-5.4",
      input_tokens: 200_000,
      output_tokens: 30_000,
      cache_read_tokens: 0,
      cache_create_tokens: 0,
      cost_usd: 1.90,
    },
  ],
  by_day: [
    { day: "2026-05-15", cost_usd: 0.5 },
    { day: "2026-05-16", cost_usd: 2.3 },
    { day: "2026-05-17", cost_usd: 1.1 },
  ],
  top_conversations: [
    {
      conversation_id: "c-1",
      title: "Markdown notes app",
      cost_usd: 12.0,
      input_tokens: 600_000,
      output_tokens: 40_000,
    },
    {
      conversation_id: "c-2",
      title: "URL shortener",
      cost_usd: 5.5,
      input_tokens: 200_000,
      output_tokens: 18_000,
    },
  ],
};

const emptySummary = {
  period: { start: "2026-05-01", end: "2026-05-31" },
  total_cost_usd: 0,
  by_model: [],
  by_day: [],
  top_conversations: [],
};

beforeEach(() => {
  // Pin "today" to mid-period so pace projection and days-remaining
  // arithmetic are deterministic across CI machines and time zones.
  // toFake:['Date'] only — leave setTimeout/clearTimeout real so
  // React's effect scheduling + testing-library's waitFor still work.
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-05-15T12:00:00Z"));
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("UsageSettings", () => {
  it("renders the hero in dollars against the plan budget", async () => {
    installFetchMock({ plan: { body: fullPlan }, usage: { body: sampleSummary } });
    render(<UsageSettings />);

    // Wait for both fetches to resolve.
    await waitFor(() => screen.getByText("$23.45"));

    // Hero shows the consumed amount + the included budget.
    expect(screen.getByText("$23.45")).toBeTruthy();
    expect(screen.getByText("of $200")).toBeTruthy();

    // Progress bar: 23.45 / 200 = 11.725% → rounded to 12% used.
    expect(screen.getByText(/12% used/)).toBeTruthy();
  });

  it('shows "<1% used" when total > 0 but rounds to 0%', async () => {
    // $0.61 / $200 = 0.305%. Rounds to 0% which would read as "no
    // usage" even though there IS spend on screen. The hero must
    // render "<1% used" instead.
    installFetchMock({
      plan: { body: fullPlan },
      usage: {
        body: {
          ...sampleSummary,
          total_cost_usd: 0.61,
          by_model: [
            {
              model: "gpt-5.4-mini",
              input_tokens: 3900,
              output_tokens: 1300,
              cache_read_tokens: 228_500,
              cache_create_tokens: 0,
              cost_usd: 0.6,
            },
          ],
          by_day: [{ day: "2026-05-15", cost_usd: 0.61 }],
          top_conversations: [
            {
              conversation_id: "c-1",
              title: "Two file checks",
              cost_usd: 0.61,
              input_tokens: 7200,
              output_tokens: 1900,
            },
          ],
        },
      },
    });
    render(<UsageSettings />);
    await waitFor(() => screen.getByText("<1% used"));
    expect(screen.getByText("<1% used")).toBeTruthy();
    expect(screen.queryByText("0% used")).toBeNull();
  });

  it("shows a pace estimate after 3+ days of subscription period have elapsed", async () => {
    installFetchMock({ plan: { body: fullPlan }, usage: { body: sampleSummary } });
    render(<UsageSettings />);

    // 14 days elapsed of a 31-day cycle, $23.45 burned → ~$51.93 projected.
    await waitFor(() => screen.getByText(/At this pace/));
    // Projection is loose — assert just the framing + the dollar shape.
    expect(screen.getByText(/At this pace/)).toBeTruthy();
    expect(screen.getByText(/this cycle/)).toBeTruthy();
  });

  it("hides pace estimate when there's nothing to project from yet", async () => {
    // Subscription period + elapsed days are real, but total is $0.
    // "At this pace, $0.00 this cycle" would be useless copy — the
    // empty-state card carries the message.
    installFetchMock({
      plan: { body: fullPlan },
      usage: { body: emptySummary },
    });
    render(<UsageSettings />);
    await waitFor(() => screen.getByText("No AI consumption yet this period."));
    expect(screen.queryByText(/At this pace/)).toBeNull();
  });

  it("hides pace estimate on calendar-month fallback even with elapsed days", async () => {
    installFetchMock({
      plan: { body: calendarPlan },
      usage: { body: sampleSummary },
    });
    render(<UsageSettings />);

    await waitFor(() => screen.getByText("$23.45"));

    // calendar_month period is a fallback, not a real billing cycle —
    // projecting from it would mislead. The pace line must be absent.
    expect(screen.queryByText(/At this pace/)).toBeNull();
  });

  it("renders Custom plan framing for Enterprise (aiBudget = -1)", async () => {
    installFetchMock({
      plan: { body: enterprisePlan },
      usage: { body: sampleSummary },
    });
    render(<UsageSettings />);

    await waitFor(() => screen.getByText("$23.45"));

    expect(screen.getByText("Custom plan")).toBeTruthy();
    // No progress bar copy when unmetered.
    expect(screen.queryByText(/% used/)).toBeNull();
    expect(screen.queryByText(/of \$/)).toBeNull();
    expect(screen.getByText(/don't have a per-month cap/)).toBeTruthy();
  });

  it("shows empty state when no consumption yet this period", async () => {
    installFetchMock({ plan: { body: fullPlan }, usage: { body: emptySummary } });
    render(<UsageSettings />);

    await waitFor(() => screen.getByText("No AI consumption yet this period."));

    // No by-model card, no top-chats card.
    expect(screen.queryByText("By model")).toBeNull();
    expect(screen.queryByText("Top chats")).toBeNull();
  });

  it("renders the by-model and top-chats breakdowns when there is usage", async () => {
    installFetchMock({ plan: { body: fullPlan }, usage: { body: sampleSummary } });
    render(<UsageSettings />);

    await waitFor(() => screen.getAllByText("VibeCraft manager"));
    expect(screen.getAllByText("VibeCraft manager").length).toBeGreaterThan(0);
    expect(screen.getByText("$21.55")).toBeTruthy();
    expect(screen.getByText("$1.90")).toBeTruthy();

    expect(screen.getByText("Markdown notes app")).toBeTruthy();
    expect(screen.getByText("URL shortener")).toBeTruthy();
    expect(screen.getByText("$12.00")).toBeTruthy();
  });

  it("surfaces unpriced models as a warning banner", async () => {
    installFetchMock({
      plan: { body: fullPlan },
      usage: {
        body: {
          ...sampleSummary,
          unpriced_models: ["gpt-someday-future-7"],
        },
      },
    });
    render(<UsageSettings />);

    await waitFor(() => screen.getByRole("alert"));
    expect(screen.getByRole("alert").textContent).toContain(
      "gpt-someday-future-7",
    );
  });

  it("renders the budget-reached banner when the daemon reports paused", async () => {
    installFetchMock({
      plan: { body: fullPlan },
      usage: {
        body: {
          ...sampleSummary,
          budget_state: {
            budget_usd: 200,
            enforced: true,
            paused: true,
            spent_usd: 201.4,
            resets_on: "2026-06-02",
          },
        },
      },
    });
    render(<UsageSettings />);

    await waitFor(() => screen.getByText("Monthly AI budget reached"));
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Monthly AI budget reached");
    expect(alert.textContent).toContain("2026-06-02");
    expect(alert.textContent).toContain("paused");
  });

  it("does NOT render the paused banner when the daemon is not paused", async () => {
    installFetchMock({
      plan: { body: fullPlan },
      usage: {
        body: {
          ...sampleSummary,
          budget_state: {
            budget_usd: 200,
            enforced: true,
            paused: false,
            spent_usd: 23.45,
            resets_on: "2026-06-02",
          },
        },
      },
    });
    render(<UsageSettings />);
    await waitFor(() => screen.getByText("$23.45"));
    expect(screen.queryByText("Monthly AI budget reached")).toBeNull();
  });

  it("renders gracefully when /api/plan fails (degraded mode)", async () => {
    installFetchMock({
      plan: { status: 500, body: { error: "boom" } },
      usage: { body: sampleSummary },
    });
    render(<UsageSettings />);

    // Hero still renders the consumed amount; budget framing falls
    // back to the "Plan unavailable" indicator.
    await waitFor(() => screen.getByText("$23.45"));
    expect(screen.getByText("Plan unavailable")).toBeTruthy();
  });

  it("surfaces a clear error when /api/usage fails", async () => {
    installFetchMock({
      plan: { body: fullPlan },
      usage: { status: 500 },
    });
    render(<UsageSettings />);

    await waitFor(() =>
      screen.getByText("Could not load usage from this computer."),
    );
  });

  it("calls /api/plan with credentials:'include' for cross-origin auth", async () => {
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL, init?: RequestInit) => {
        return new Response(JSON.stringify(fullPlan), { status: 200 });
      },
    );
    globalThis.fetch = fetchSpy as unknown as typeof fetch;

    // Override with usage path returning a 500 to short-circuit (we
    // only care about the plan fetch's init in this test).
    fetchSpy.mockImplementation(async (input, init) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/plan")) {
        return new Response(JSON.stringify(fullPlan), { status: 200 });
      }
      return new Response("{}", { status: 500 });
    });

    render(<UsageSettings />);
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());

    const planCall = fetchSpy.mock.calls.find((c) =>
      String(c[0]).includes("/api/plan"),
    );
    expect(planCall).toBeTruthy();
    expect(planCall?.[1]?.credentials).toBe("include");
  });

  it("passes the plan's period through to /api/usage as query params", async () => {
    const seenUrls: string[] = [];
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      seenUrls.push(url);
      if (url.includes("/api/plan")) {
        return new Response(JSON.stringify(fullPlan), { status: 200 });
      }
      if (url.includes("/api/usage")) {
        return new Response(JSON.stringify(sampleSummary), { status: 200 });
      }
      return new Response("{}", { status: 404 });
    }) as unknown as typeof fetch;

    render(<UsageSettings />);
    await waitFor(() => screen.getByText("$23.45"));

    const usageURL = seenUrls.find((u) => u.includes("/api/usage"));
    expect(usageURL).toBeTruthy();
    expect(usageURL).toContain("start=2026-05-01");
    expect(usageURL).toContain("end=2026-05-31");
  });
});
