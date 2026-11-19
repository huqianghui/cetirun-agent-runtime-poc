// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";
import { productionLicenses } from "./scripts/production-licenses.mjs";

export default defineConfig({
  plugins: [
    react(),
    productionLicenses(fileURLToPath(new URL("./package.json", import.meta.url))),
  ],
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
        // In production, OAuth2 Proxy sets these headers at the gateway.
        // During local dev (npm run dev + go run), inject them so the
        // backend middleware doesn't reject requests with 401.
        // Set DEV_USER to your alias (e.g. DEV_USER=alice npm run dev).
        headers: {
          "X-Auth-Request-Preferred-Username": `${process.env.DEV_USER ?? "dev"}@example.com`,
          "X-Auth-Request-Email": `${process.env.DEV_USER ?? "dev"}@example.com`,
          "X-Auth-Request-User": `local-test-${process.env.DEV_USER ?? "dev"}`,
        },
      },
    },
  },
  test: {
    environment: "jsdom",
    maxWorkers: 2,
    setupFiles: ["./src/test/setup.ts"],
    globals: true,
  },
});
