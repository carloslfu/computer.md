// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { maybeRefresh, tryRefreshSync } from "./refresh";

// Workstream G "Done when": SPA-side tests for silent-refresh debounce
// under concurrent requests + refresh-failure fall-through.
//
// refreshNow() does two fetches: GET platform /auth/grant?xhr=1 then
// POST /auth/callback. We mock global.fetch and assert on call count.

function headers(obj: Record<string, string>): Headers {
  return new Headers(obj);
}

function okGrant(): Response {
  return {
    ok: true,
    status: 200,
    json: async () => ({ code: "grant-code-xyz" }),
    headers: headers({}),
  } as unknown as Response;
}

function okCallback(): Response {
  return {
    ok: true,
    status: 200,
    json: async () => ({ ok: true }),
    headers: headers({}),
  } as unknown as Response;
}

describe("maybeRefresh — background singleton", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("no-ops when the response lacks X-Vc-Refresh", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    maybeRefresh({ headers: headers({}) } as unknown as Response);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("debounces N concurrent X-Vc-Refresh responses into ONE round-trip", async () => {
    let grantCalls = 0;
    let callbackCalls = 0;
    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/auth/grant")) {
        grantCalls += 1;
        return okGrant();
      }
      callbackCalls += 1;
      return okCallback();
    });
    vi.stubGlobal("fetch", fetchMock);

    // Fire 5 responses all carrying the refresh hint, synchronously.
    const r = { headers: headers({ "X-Vc-Refresh": "1" }) } as unknown as Response;
    maybeRefresh(r);
    maybeRefresh(r);
    maybeRefresh(r);
    maybeRefresh(r);
    maybeRefresh(r);

    // Let the in-flight promise settle.
    await vi.waitFor(() => expect(callbackCalls).toBe(1));

    // Exactly one grant + one callback despite 5 hints — the singleton
    // coalesced the rest. This is the race the plan called out.
    expect(grantCalls).toBe(1);
    expect(callbackCalls).toBe(1);
  });

  it("allows a fresh refresh after the previous one settles", async () => {
    let grantCalls = 0;
    let callbackCalls = 0;
    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/auth/grant")) {
        grantCalls += 1;
        return okGrant();
      }
      callbackCalls += 1;
      return okCallback();
    });
    vi.stubGlobal("fetch", fetchMock);

    const r = { headers: headers({ "X-Vc-Refresh": "1" }) } as unknown as Response;
    maybeRefresh(r);
    // Wait for the FULL round-trip (grant + callback) AND the .finally()
    // that nulls the in-flight singleton. A macrotask flush guarantees
    // the finally chain has run before we fire the next hint.
    await vi.waitFor(() => expect(callbackCalls).toBe(1));
    await new Promise((res) => setTimeout(res, 0));
    // New cookie window later — singleton is clear, so this fires again.
    maybeRefresh(r);
    await vi.waitFor(() => expect(grantCalls).toBe(2));
  });
});

describe("tryRefreshSync — shared in-flight + result contract", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it("returns true on successful renew, and concurrent callers share one round-trip", async () => {
    let grantCalls = 0;
    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/auth/grant")) {
        grantCalls += 1;
        return okGrant();
      }
      return okCallback();
    });
    vi.stubGlobal("fetch", fetchMock);

    const [a, b, c] = await Promise.all([
      tryRefreshSync(),
      tryRefreshSync(),
      tryRefreshSync(),
    ]);
    expect(a).toBe(true);
    expect(b).toBe(true);
    expect(c).toBe(true);
    expect(grantCalls).toBe(1); // coalesced
  });

  it("returns false when the platform grant fails (fall-through contract)", async () => {
    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/auth/grant")) {
        return { ok: false, status: 401, headers: headers({}) } as unknown as Response;
      }
      return okCallback();
    });
    vi.stubGlobal("fetch", fetchMock);

    const result = await tryRefreshSync();
    expect(result).toBe(false);
  });

  it("returns false when the daemon callback rejects the fresh code", async () => {
    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/auth/grant")) return okGrant();
      return { ok: false, status: 401, headers: headers({}) } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetchMock);

    expect(await tryRefreshSync()).toBe(false);
  });
});
