import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";
import {
  type GeneratedCode,
  packageRoot,
  thirdPartyNotices,
  viteBundleInputs,
} from "../../../scripts/third-party-notices";

const vite = packageRoot("vite", fileURLToPath(new URL(".", import.meta.url)));
/**
 * The code the build emits from no source file it records: Vite's own helpers (module preload,
 * and the CommonJS interop of the @rollup/plugin-commonjs build Vite 5 carries inside it), and
 * the wrappers that interop puts around a CommonJS module, whose own file the build records.
 */
const GENERATED_CODE: GeneratedCode[] = [
  { id: /^\0vite\//, packages: [vite] },
  { id: /^\0(commonjsHelpers\.js|commonjs-dynamic-modules)$/, packages: [vite] },
  { id: /^\0\/.*\?commonjs-(module|exports)$/, packages: [] },
];

/**
 * Writes dist/THIRD_PARTY_NOTICES.txt, which Dispatch serves at /THIRD_PARTY_NOTICES.txt as text:
 * the license of every third-party package the build copies into dist.
 */
function thirdPartyNoticesFile(): Plugin {
  let root = "";
  return {
    name: "third-party-notices",
    apply: "build",
    configResolved(config) {
      root = config.root;
    },
    async generateBundle(_options, bundle) {
      const inputs = await viteBundleInputs(this, bundle, root, GENERATED_CODE);
      this.emitFile({
        type: "asset",
        fileName: "THIRD_PARTY_NOTICES.txt",
        source: await thirdPartyNotices(inputs, root),
      });
    },
  };
}

export default defineConfig({
  root: "web",
  plugins: [react(), tailwindcss(), thirdPartyNoticesFile()],
  define: {
    // Keep the entry chunk distinct across deployments, including no-source-change image rebuilds.
    __DISPATCH_BUILD__: JSON.stringify(
      process.env.GITHUB_SHA ?? process.env.DISPATCH_BUILD ?? Date.now().toString()
    ),
  },
  resolve: {
    // The source-only editor and Dispatch both reach this constructor. Historical-version
    // rendering passes a Proof document to Dispatch's DOMSerializer, so Vite must emit one
    // module identity rather than one per workspace import path.
    dedupe: ["prosemirror-model"],
    alias: {
      "@legion/contracts/repo": fileURLToPath(
        new URL("../../contracts/src/repo.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-snippet": fileURLToPath(
        new URL("../../contracts/src/dispatch-snippet.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-tools": fileURLToPath(
        new URL("../../contracts/src/dispatch-tools.ts", import.meta.url)
      ),
      "@legion/contracts": fileURLToPath(new URL("../../contracts/src/index.ts", import.meta.url)),
      "@legion/proof-editor/headless": fileURLToPath(
        new URL("../../proof-editor/src/lib-headless.ts", import.meta.url)
      ),
      "@legion/proof-editor/style.css": fileURLToPath(
        new URL("../../proof-editor/src/lib.css", import.meta.url)
      ),
      "@legion/proof-editor": fileURLToPath(
        new URL("../../proof-editor/src/lib.ts", import.meta.url)
      ),
      // proof-sdk's package.json publishes only its built dist through `exports`, so the
      // editor's upstream imports name a path its own `exports` hides. Same mapping as
      // packages/proof-editor/tsconfig.json, which is what Bun and tsc read.
      "proof-sdk-upstream/src": fileURLToPath(
        new URL("../../proof-editor/node_modules/proof-sdk-upstream/src", import.meta.url)
      ),
    },
  },
  build: {
    outDir: "dist",
    // The editor (Milkdown + mermaid) is one ~2.8 MB lazy chunk loaded only on the document tab.
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
