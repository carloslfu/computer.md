// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the daemon-served SPA.
//
// In production the bundle is //go:embedded into the daemon binary
// and served from the same origin as the API. For local dev the Vite
// server proxies /api/* and /auth/* to a remote dev daemon (see
// VITE_API_BASE) so feature work doesn't require a local Linux env.
export default defineConfig(({ mode }) => {
  const target = process.env.VITE_API_BASE ?? "http://localhost:8420";
  return {
    plugins: [react()],
    build: {
      // Hashed asset filenames so the daemon can serve them with
      // immutable cache headers; index.html stays no-cache.
      outDir: "dist",
      emptyOutDir: true,
      sourcemap: mode !== "production",
    },
    server: {
      port: 5173,
      proxy: {
        "/api": {
          target,
          changeOrigin: true,
          secure: true,
        },
        "/auth": {
          target,
          changeOrigin: true,
          secure: true,
        },
        "/health": {
          target,
          changeOrigin: true,
          secure: true,
        },
      },
    },
  };
});
