// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState, type ReactNode } from "react";
import { Route, Routes, useLocation } from "react-router-dom";
import { apiFetch } from "./api/fetch";
import { ErrorBoundary } from "./components/ErrorBoundary";
import {
  PLATFORM_BASE,
  platformGrantURL,
  type WhoAmI,
} from "./auth/types";

import { ChatRoute } from "./routes/ChatRoute";
import { SettingsRoute } from "./routes/SettingsRoute";

type AuthState =
  | { status: "loading" }
  | { status: "authed"; who: WhoAmI }
  | { status: "error"; message: string };

export function App() {
  return (
    <ErrorBoundary>
      <AuthGate>
        {(who) => (
          <Routes>
            <Route path="/" element={<ChatRoute who={who} />} />
            <Route path="/c/:conversationId" element={<ChatRoute who={who} />} />
            <Route path="/settings/*" element={<SettingsRoute who={who} />} />
            <Route path="*" element={<ChatRoute who={who} />} />
          </Routes>
        )}
      </AuthGate>
    </ErrorBoundary>
  );
}

function AuthGate({ children }: { children: (who: WhoAmI) => ReactNode }) {
  const [state, setState] = useState<AuthState>({ status: "loading" });
  const location = useLocation();

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await apiFetch("/api/auth/whoami", {
          skipAuthRedirect: true,
        });
        if (res.status === 401) {
          window.location.href = platformGrantURL(
            location.pathname + location.search,
          );
          return;
        }
        if (!res.ok) {
          throw new Error(`whoami HTTP ${res.status}`);
        }
        const who = (await res.json()) as WhoAmI;
        if (!cancelled) setState({ status: "authed", who });
      } catch (err) {
        if (!cancelled)
          setState({ status: "error", message: (err as Error).message });
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (state.status === "loading") {
    return (
      <main className="flex min-h-screen items-center justify-center bg-[#f4f3ee] p-6">
        <p className="text-sm text-slate-500">Connecting to your machine…</p>
      </main>
    );
  }

  if (state.status === "error") {
    return (
      <main className="flex min-h-screen items-center justify-center bg-[#f4f3ee] p-6">
        <div className="max-w-md rounded-2xl border border-slate-200/60 bg-white/70 p-8 shadow-sm">
          <h1 className="font-poppins text-2xl font-semibold tracking-tight text-slate-950">
            Couldn’t reach the machine
          </h1>
          <p className="mt-3 text-sm text-slate-600">{state.message}</p>
          <p className="mt-6">
            <a
              href={PLATFORM_BASE + "/dashboard"}
              className="text-sm font-medium text-slate-700 underline-offset-4 hover:underline"
            >
              ← Back to all machines
            </a>
          </p>
        </div>
      </main>
    );
  }

  return <>{children(state.who)}</>;
}
