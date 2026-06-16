// SPDX-License-Identifier: Apache-2.0

import { maybeRefresh, tryRefreshSync } from "../auth/refresh";
import { platformGrantURL } from "../auth/types";

// Every SPA fetch goes through this wrapper. Three jobs:
//   1. credentials: 'include' so the vc_session cookie rides along.
//   2. JSON Content-Type for mutations (the CSRF half SameSite=Lax
//      doesn't cover).
//   3. Silent-refresh observation (background X-Vc-Refresh hint) +
//      block-and-retry on 401 with a single redirect fallback.
//
// Lint enforces this: the no-restricted-syntax rule in eslint.config.js
// bans raw `fetch(` everywhere except this file (and a few annotated
// exceptions), so the wrapper can't be silently bypassed.

const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

export type FetchOptions = RequestInit & {
  // Set to true to skip the auth fallback flow (used by the bootstrap
  // /api/auth/whoami call which needs to handle 401 explicitly).
  skipAuthRedirect?: boolean;
};

export async function apiFetch(
  path: string,
  init: FetchOptions = {},
): Promise<Response> {
  return apiFetchInternal(path, init, false);
}

async function apiFetchInternal(
  path: string,
  init: FetchOptions,
  isRetry: boolean,
): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);

  // Mutations need an explicit Content-Type to defeat cross-origin
  // form-submit CSRF. Browsers block setting application/json from a
  // cross-origin form, and SameSite=Lax blocks the preflight. Skip if
  // the caller already set a type (multipart for uploads, etc.).
  const hasFormDataBody =
    typeof FormData !== "undefined" && init.body instanceof FormData;
  if (MUTATING.has(method) && !headers.has("Content-Type") && !hasFormDataBody) {
    headers.set("Content-Type", "application/json");
  }
  if (!headers.has("Accept")) {
    headers.set("Accept", "application/json");
  }

  const response = await fetch(path, {
    ...init,
    method,
    headers,
    credentials: "include",
  });

  maybeRefresh(response);

  if (response.status === 401 && !init.skipAuthRedirect && !isRetry) {
    const refreshed = await tryRefreshSync();
    if (refreshed) {
      return apiFetchInternal(path, init, true);
    }
    const returnPath = window.location.pathname + window.location.search;
    window.location.href = platformGrantURL(returnPath);
    throw new Error("Auth required — redirecting");
  }

  return response;
}

export async function apiJSON<T = unknown>(
  path: string,
  init: FetchOptions = {},
): Promise<T> {
  const res = await apiFetch(path, init);
  if (!res.ok) {
    let detail = "";
    try {
      const body = await res.json();
      detail = typeof body === "object" && body && "error" in body
        ? String((body as { error: unknown }).error)
        : JSON.stringify(body);
    } catch {
      try {
        detail = await res.text();
      } catch {
        detail = "";
      }
    }
    throw new APIError(res.status, detail || `HTTP ${res.status}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export class APIError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "APIError";
    this.status = status;
  }
}
