import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  root: "web",
  plugins: [react(), tailwindcss()],
  define: {
    // Keep the entry chunk distinct across deployments, including no-source-change image rebuilds.
    __DISPATCH_BUILD__: JSON.stringify(
      process.env.GITHUB_SHA ?? process.env.DISPATCH_BUILD ?? Date.now().toString()
    ),
  },
  resolve: {
    alias: {
      "@legion/contracts/repo": fileURLToPath(new URL("../../contracts/src/repo.ts", import.meta.url)),
      "@legion/contracts": fileURLToPath(new URL("../../contracts/src/index.ts", import.meta.url)),
    },
  },
  build: {
    outDir: "dist",
    // @sjawhar/proof-editor (Milkdown + mermaid) is one ~2.8 MB lazy chunk loaded only on the document tab.
    chunkSizeWarningLimit: 3000,
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8766",
      "/auth": "http://localhost:8766",
      "/ws": {
        target: "ws://localhost:8766",
        ws: true,
      },
    },
  },
});
