// SPDX-License-Identifier: Apache-2.0

import { Component, type ReactNode } from "react";

type Props = { children: ReactNode };
type State = { error: Error | null };

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error) {
    console.error("[ErrorBoundary]", error);
  }

  render() {
    if (this.state.error) {
      return (
        <main className="flex min-h-screen items-center justify-center bg-[#f4f3ee] p-6">
          <div className="max-w-md rounded-2xl border border-slate-200/60 bg-white/70 p-8 shadow-sm">
            <h1 className="font-poppins text-2xl font-semibold tracking-tight text-slate-950">
              Something went wrong
            </h1>
            <p className="mt-3 text-sm text-slate-600">
              The chat surface hit an error and stopped. Reload to recover; if
              this keeps happening, head back to the lobby.
            </p>
            <pre className="mt-4 max-h-32 overflow-auto rounded border border-slate-200/60 bg-slate-50 p-3 text-xs text-slate-600">
              {this.state.error.message}
            </pre>
            <div className="mt-6 flex gap-3">
              <button
                type="button"
                onClick={() => window.location.reload()}
                className="rounded-full bg-slate-950 px-5 py-2 text-sm font-medium text-white transition hover:-translate-y-0.5 hover:shadow-lg hover:shadow-slate-950/20"
              >
                Reload
              </button>
              <a
                href="https://www.vibecraft.so/dashboard"
                className="rounded-full border border-slate-200 px-5 py-2 text-sm font-medium text-slate-700 transition hover:border-slate-300"
              >
                Back to all machines
              </a>
            </div>
          </div>
        </main>
      );
    }
    return this.props.children;
  }
}
