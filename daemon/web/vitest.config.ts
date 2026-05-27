// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "jsdom",
    globals: true,
    // jsdom defaults window.location to http://localhost. machineSlug()
    // reads window.location.host so the auth helpers behave as if served
    // from a real machine host.
    environmentOptions: {
      jsdom: { url: "https://vc-test123.vc.vibecraft.so/c/abc" },
    },
  },
});
