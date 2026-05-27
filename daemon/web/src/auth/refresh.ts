// SPDX-License-Identifier: Apache-2.0

import { machineSlug, PLATFORM_BASE } from "./types";

// Silent-refresh singleton. The daemon sets `X-Vc-Refresh: 1` on
// responses when the session cookie has <1h left. Multiple concurrent
// responses may carry the header — debounce them so we only fire one
// platform round-trip per cookie window.

let backgroundInFlight: Promise<void> | null = null;
let syncInFlight: Promise<boolean> | null = null;

export function maybeRefresh(response: Response): void {
  if (response.headers.get("X-Vc-Refresh") !== "1") return;
  if (backgroundInFlight) return;
  backgroundInFlight = refreshNow()
    .catch(() => {
      // Failure is silent. Next 401 triggers the explicit redirect flow.
    })
    .finally(() => {
      backgroundInFlight = null;
    });
}

// Synchronous refresh used by `apiFetch` after a 401. Resolves true if
// the cookie was successfully renewed (caller should retry the
// original request), false otherwise (caller should fall through to
// the redirect-to-platform flow).
export function tryRefreshSync(): Promise<boolean> {
  if (syncInFlight) return syncInFlight;
  syncInFlight = refreshNow()
    .then(() => true)
    .catch(() => false)
    .finally(() => {
      syncInFlight = null;
    });
  return syncInFlight;
}

async function refreshNow(): Promise<void> {
  // 1. Ask the platform for a fresh grant code. WorkOS cookie on
  //    .vibecraft.so is sent automatically (credentials: 'include').
  const grantURL = new URL(PLATFORM_BASE + "/auth/grant");
  grantURL.searchParams.set("machine", machineSlug());
  grantURL.searchParams.set("xhr", "1");

  // eslint-disable-next-line no-restricted-syntax -- this IS the refresh primitive; apiFetch depends on it, can't depend back
  const grantRes = await fetch(grantURL.toString(), {
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!grantRes.ok) {
    throw new Error(`grant HTTP ${grantRes.status}`);
  }
  const body = (await grantRes.json()) as { code?: string };
  if (!body.code) {
    throw new Error("grant returned no code");
  }

  // 2. Exchange the code for a fresh cookie on this machine. The
  //    daemon reads `code` from the URL query (per
  //    daemon/auth_cookie.go:141); body is ignored. Accept JSON so the
  //    daemon returns 200 instead of a 302 the SPA would have to chase.
  const cbURL = new URL("/auth/callback", window.location.origin);
  cbURL.searchParams.set("code", body.code);
  // eslint-disable-next-line no-restricted-syntax -- refresh primitive; routing through apiFetch would recurse on 401
  const cbRes = await fetch(cbURL.toString(), {
    method: "POST",
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!cbRes.ok) {
    throw new Error(`callback HTTP ${cbRes.status}`);
  }
}
