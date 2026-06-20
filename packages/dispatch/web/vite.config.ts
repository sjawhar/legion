import { defineConfig } from "vite";

export default defineConfig({
  root: "web",
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8766",
      "/auth": "http://localhost:8766",
    },
  },
});
