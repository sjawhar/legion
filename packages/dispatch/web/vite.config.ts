import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  root: "web",
  plugins: [react(), tailwindcss()],
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
