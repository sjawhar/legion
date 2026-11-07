import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";
import {
  type GeneratedCode,
  packageRoot,
  thirdPartyNotices,
  viteBundleInputs,
  viteRuntimeGeneratedCode,
} from "../../../scripts/third-party-notices";

/**
 * `decode-named-character-reference`'s DOM-free implementation, which micromark decodes every
 * named character reference through in every bundle this config builds. The package's `browser`
 * export condition is a module whose body is `document.createElement('i')`, and Vite resolves a
 * Web Worker's bundle with the client's conditions, so the parse worker
 * (features/refs/markdown-parse.worker.ts) would throw before its first message. Its default
 * condition answers from the `character-entities` table instead - the table Bun, Node, the unit
 * suites and the pmdoc fixture generator already run - so a body reads the same above and below
 * the parse worker's threshold, and the same as the generator that pins Go's fixtures
 * (sjawhar/legion#2519), for 11.6 KB gzipped on the page's bundle.
 *
 * The package publishes no `./index.js` subpath, so the file is resolved rather than named: from
 * the editor, which `micromark-core-commonmark` is a dependency of, and from that package, which
 * decodes references with it. `require`'s own conditions hold no `browser`, so this is `index.js`.
 */
const proofEditor = createRequire(
  fileURLToPath(new URL("../../proof-editor/src/lib-headless.ts", import.meta.url))
);
const characterReferenceDecoder = createRequire(
  proofEditor.resolve("micromark-core-commonmark")
).resolve("decode-named-character-reference");
const vite = packageRoot("vite", fileURLToPath(new URL(".", import.meta.url)));
/**
 * The code the build emits from no source file it records: Vite's own helpers (module preload,
 * the stand-in for an optional peer dependency that is not installed, and the CommonJS interop
 * of the @rollup/plugin-commonjs build Vite 5 carries inside it), and the wrappers that interop
 * puts around a CommonJS module, whose own file the build records.
 */
const GENERATED_CODE: GeneratedCode[] = [
  viteRuntimeGeneratedCode(vite),
  { id: /^\0(commonjsHelpers\.js|commonjs-dynamic-modules)$/, packages: [vite] },
  { id: /^\0\/.*\?commonjs-(module|exports)$/, packages: [] },
  // linkedom, which the diagram worker gives upstream Mermaid a DOM with, declares the native
  // `canvas` package as an optional peer dependency for its own `<canvas>`. Nothing installs it
  // and nothing needs it: the shim answers `getContext` from the worker's `OffscreenCanvas`
  // (`features/refs/mermaid-dom-shim.ts`). Vite emits a module of its own that throws if the
  // import is ever reached, so the code is Vite's.
  { id: /^\0?__vite-optional-peer-dep:/, packages: [vite] },
];

/**
 * Writes dist/THIRD_PARTY_NOTICES.txt, which Dispatch serves at /THIRD_PARTY_NOTICES.txt as text:
 * the license of every third-party package the build copies into dist.
 *
 * A Web Worker (`new Worker(new URL(…), …)`) is a bundle of its own, built while the page's build
 * transforms the module that creates it, so before the page's bundle is generated; the page's
 * build then copies the worker's files in as assets that name no source. So `worker`, the plugin
 * of each worker bundle, records that bundle's files and inputs, and `page` leaves those files
 * out of the page's bundle and adds those inputs to its own.
 */
function thirdPartyNoticesFile(): { page: Plugin; worker: () => Plugin } {
  let root = "";
  const workerFiles = new Set<string>();
  const workerInputs = new Set<string>();
  return {
    page: {
      name: "third-party-notices",
      apply: "build",
      configResolved(config) {
        root = config.root;
      },
      async generateBundle(_options, bundle) {
        const pageBundle = Object.fromEntries(
          Object.entries(bundle).filter(([fileName]) => !workerFiles.has(fileName))
        );
        const inputs = await viteBundleInputs(this, pageBundle, root, GENERATED_CODE);
        for (const input of workerInputs) inputs.add(input);
        this.emitFile({
          type: "asset",
          fileName: "THIRD_PARTY_NOTICES.txt",
          source: await thirdPartyNotices(inputs, root),
        });
      },
    },
    worker: () => ({
      name: "third-party-notices-worker",
      apply: "build",
      async generateBundle(_options, bundle) {
        for (const fileName of Object.keys(bundle)) workerFiles.add(fileName);
        for (const input of await viteBundleInputs(this, bundle, root, GENERATED_CODE)) {
          workerInputs.add(input);
        }
      },
    }),
  };
}

const notices = thirdPartyNoticesFile();

export default defineConfig({
  root: "web",
  plugins: [react(), tailwindcss(), notices.page],
  // The Mermaid worker (features/refs/mermaid-render.worker.ts) is created with
  // `{ type: "module" }`, so it is bundled as an ES module. Its entry file keeps Vite's own
  // name (`assets/mermaid-render.worker-<hash>.js`); the chunks Mermaid is code-split into are
  // named `assets/mermaid-chunk-<rollup name>-<hash>.js`, because two browser rows must say
  // whether a page fetched one and Rollup would otherwise call them `elk`, `mermaid.core`,
  // `cytoscape.esm`, `katex` and so on, names that move with the dependency tree. The path is
  // flat: `two-builds.multiprocess.e2e.ts`'s `readBuild` lists `dist/assets` without recursing.
  worker: {
    format: "es",
    plugins: () => [notices.worker()],
    rollupOptions: { output: { chunkFileNames: "assets/mermaid-chunk-[name]-[hash].js" } },
  },
  define: {
    // Keep the entry chunk distinct across deployments, including no-source-change image rebuilds.
    // DISPATCH_BUILD wins over GITHUB_SHA, which CI sets on every step, so one job can build two
    // distinct dashboards of one commit (`bun run e2e:multiprocess`'s second build).
    __DISPATCH_BUILD__: JSON.stringify(
      process.env.DISPATCH_BUILD ?? process.env.GITHUB_SHA ?? Date.now().toString()
    ),
  },
  resolve: {
    // The source-only editor and Dispatch both reach this constructor. Historical-version
    // rendering passes a Proof document to Dispatch's DOMSerializer, so Vite must emit one
    // module identity rather than one per workspace import path.
    dedupe: ["prosemirror-model"],
    alias: {
      "decode-named-character-reference": characterReferenceDecoder,
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
      "@legion/proof-editor/mermaid": fileURLToPath(
        new URL("../../proof-editor/src/mermaid-svg.ts", import.meta.url)
      ),
      "@legion/proof-editor/mermaid-render": fileURLToPath(
        new URL("../../proof-editor/src/mermaid-render.ts", import.meta.url)
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
    // The largest chunk is ECharts (an `index-*.js` of 1,134,772 bytes raw), which only the
    // Delivery page loads, and the entry is 781,007 bytes, both past Vite's 500 kB default.
    // beautiful-mermaid, with the ELK it bundles, lives only in the Mermaid worker's bundle
    // (features/refs/mermaid-render.worker.ts, 1,568,116 bytes), fetched once a body or a
    // document holds a mermaid block. Upstream Mermaid is reached from that worker with
    // `await import(…)`, so it is 63 code-split chunks of its own (the largest, its own ELK's,
    // 1,513,480 bytes), named `assets/mermaid-chunk-<rollup name>-<hash>.js` by the `worker`
    // options above and fetched only by a source that needs them. Vite emits each of these as
    // an asset and holds it to no chunk limit. The editor's lazy chunk (`lib-*.js`, 204,492
    // bytes) carries neither renderer.
    chunkSizeWarningLimit: 1200,
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
