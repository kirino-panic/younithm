import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const api = "http://localhost:2015";

export default defineConfig(({ command }) => ({
  // Relative asset URLs in the build, so one image works at any base path:
  // the server adds <base href> to index.html.
  base: command === "build" ? "./" : "/",
  plugins: [react()],
  server: {
    port: 5173,
    proxy: { "/api": api, "/1": api, "/healthz": api },
  },
  build: { outDir: "dist", emptyOutDir: true, chunkSizeWarningLimit: 900 },
}));
