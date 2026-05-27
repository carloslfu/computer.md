// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiFetch } from "./fetch";

// Workstream G "Done when": refresh failure falls through to the
// redirect flow; happy 401 → refresh → retry-once succeeds. Plus the
// JSON-content-type CSRF half from D.1.6.

function headers(obj: Record<string, string> = {}): Headers {
  return new Headers(obj);
}

describe("apiFetch", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // jsdom location.href assignment throws "navigation not implemented";
    // stub the whole object so we can assert on the redirect target.
    const loc = {
      href: "",
      pathname: "/c/abc",
      search: "",
      host: "vc-test123.vc.vibecraft.so",
      origin: "https://vc-test123.vc.vibecraft.so",
    };
    Object.defineProperty(window, "location", {
      value: loc,
      writable: true,
      configurable: true,
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it("adds Content-Type: application/json on a mutating request with a body", async () => {
    let seenInit: RequestInit | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        seenInit = init;
        return { ok: true, status: 200, headers: headers() } as unknown as Response;
      }),
    );
    await apiFetch("/api/task", { method: "POST", body: JSON.stringify({ x: 1 }) });
    const h = new Headers(seenInit?.headers);
    expect(h.get("Content-Type")).toBe("application/json");
    expect(seenInit?.credentials).toBe("include");
  });

  it("adds Content-Type: application/json on a bodyless mutating request", async () => {
    let seenInit: RequestInit | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        seenInit = init;
        return { ok: true, status: 200, headers: headers() } as unknown as Response;
      }),
    );
    await apiFetch("/api/task/t-1/cancel", { method: "POST" });
    const h = new Headers(seenInit?.headers);
    expect(h.get("Content-Type")).toBe("application/json");
    expect(seenInit?.credentials).toBe("include");
  });

  it("does NOT clobber a caller-set Content-Type (multipart upload)", async () => {
    let seenInit: RequestInit | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        seenInit = init;
        return { ok: true, status: 200, headers: headers() } as unknown as Response;
      }),
    );
    await apiFetch("/api/upload", {
      method: "POST",
      body: "x",
      headers: { "Content-Type": "multipart/form-data; boundary=z" },
    });
    const h = new Headers(seenInit?.headers);
    expect(h.get("Content-Type")).toContain("multipart/form-data");
  });

  it("does not set Content-Type for FormData bodies", async () => {
    let seenInit: RequestInit | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        seenInit = init;
        return { ok: true, status: 200, headers: headers() } as unknown as Response;
      }),
    );
    const form = new FormData();
    form.set("file", new Blob(["x"]), "x.txt");
    await apiFetch("/api/upload", { method: "POST", body: form });
    const h = new Headers(seenInit?.headers);
    expect(h.has("Content-Type")).toBe(false);
  });

  it("on 401 with successful refresh: retries once and returns the retry response", async () => {
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        // refresh.ts round-trip
        if (url.includes("/auth/grant")) {
          return {
            ok: true,
            status: 200,
            json: async () => ({ code: "c" }),
            headers: headers(),
          } as unknown as Response;
        }
        if (url.includes("/auth/callback")) {
          return { ok: true, status: 200, headers: headers() } as unknown as Response;
        }
        // The actual API call: 401 first, 200 on retry.
        call += 1;
        return {
          ok: call > 1,
          status: call > 1 ? 200 : 401,
          headers: headers(),
        } as unknown as Response;
      }),
    );
    const res = await apiFetch("/api/conversations");
    expect(res.status).toBe(200);
    expect(call).toBe(2); // original + one retry
    expect(window.location.href).toBe(""); // no redirect needed
  });

  it("on 401 with failed refresh: redirects to the platform grant flow", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.includes("/auth/grant")) {
          // refresh fails → tryRefreshSync resolves false
          return { ok: false, status: 401, headers: headers() } as unknown as Response;
        }
        return { ok: false, status: 401, headers: headers() } as unknown as Response;
      }),
    );
    await expect(apiFetch("/api/conversations")).rejects.toThrow(
      /Auth required/,
    );
    expect(window.location.href).toContain(
      "https://www.vibecraft.so/auth/grant",
    );
    expect(window.location.href).toContain("machine=vc-test123");
    expect(window.location.href).toContain("return=");
  });

  it("skipAuthRedirect bypasses the redirect (used by the bootstrap whoami call)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        return { ok: false, status: 401, headers: headers() } as unknown as Response;
      }),
    );
    const res = await apiFetch("/api/auth/whoami", { skipAuthRedirect: true });
    expect(res.status).toBe(401);
    expect(window.location.href).toBe(""); // no redirect, caller handles it
  });
});
