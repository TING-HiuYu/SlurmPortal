import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The UI is served by the same Worker as the API (Workers Static Assets), so every call is
// same-origin. `npm run dev` proxies /api to API_ORIGIN (default: a local cmd/server on port 8080).
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", sourcemap: false, chunkSizeWarningLimit: 1500 },
  server: { proxy: { "/api": { target: process.env.API_ORIGIN || "http://127.0.0.1:8080", changeOrigin: true } } },
  test: { environment: "jsdom" },
});
