import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// What a reader opening a document waits for is the entry chunk: the browser downloads it,
// compiles it and evaluates every static import of it in one main-thread task before anything
// of the page's own runs (dispatch://INFRA-341, R11). The five parts below are not on the screen
// when a document's first block is, and the issue page's own first render is the largest commit
// the app makes, so neither belongs in that task. Both rules are shapes of `app.tsx`, which is
// what this reads - as `e2e/__tests__/two-builds-routes.test.ts` reads its router.
const source = (path: string) =>
  readFileSync(fileURLToPath(new URL(path, import.meta.url)), "utf8");

const app = source("../app.tsx");
const deferred = source("../features/shell/deferred-shell.ts");

/** Each `export const X = lazy(() => import("…"))` of `deferred-shell.ts`, by name. */
const deferredParts = new Map(
  [...deferred.matchAll(/export const (\w+) = lazy\(\(\) =>\s*import\("([^"]+)"\)/g)].map(
    ([, name, module]) => [name as string, module as string]
  )
);

/** The shell's parts that a document route does not draw first, each with the module it lives
 *  in, which `app.tsx` must not reach by a static import of its own. */
const parts: readonly { readonly name: string; readonly module: string }[] = [
  { module: "../inbox/InboxDrawer", name: "InboxDrawer" },
  { module: "../issue/CreateIssueDialog", name: "CreateIssueDialog" },
  { module: "../margin/Margin", name: "Margin" },
  { module: "../refs/RefPreview", name: "RefPreviewHost" },
  { module: "../refs/agent-registry", name: "AgentRegistryPoller" },
];

/** Every module `app.tsx` imports statically, as it writes the specifier. */
const staticImports = [...app.matchAll(/^import\s[^;]*?from "([^"]+)";$/gms)].map(
  ([, specifier]) => specifier as string
);

describe("the shell's deferred parts", () => {
  test("app.tsx declares static imports this test can read", () => {
    expect(staticImports).toContain("./features/shell/deferred-shell");
  });

  test("each loads by a dynamic import of its own module", () => {
    expect([...deferredParts].sort()).toEqual(
      parts.map((part) => [part.name, part.module] as [string, string]).sort()
    );
  });

  test("app.tsx takes each from deferred-shell, never from the part's own module", () => {
    const fromTheirOwnModules = parts.filter((part) =>
      staticImports.includes(part.module.replace("../", "./features/"))
    );
    expect(fromTheirOwnModules.map((part) => part.name)).toEqual([]);
    for (const part of parts) {
      expect(app).toContain(`  ${part.name},\n`);
    }
  });

  test("the margin's default width is its own module, so the shell reads it without the margin", () => {
    expect(staticImports).toContain("./features/margin/margin-width");
    expect(staticImports).not.toContain("./features/margin/Margin");
  });

  test("the reference trigger every page renders is apart from the card's host", () => {
    const preview = source("../features/refs/RefPreview.tsx");
    const state = source("../features/refs/ref-preview-state.ts");
    expect(state).toContain("export function referenceTriggerProps(");
    expect(state).toContain("export function closeRefPreview(");
    expect(preview).toContain("export function RefPreviewHost(");
    expect(preview).not.toContain("export function referenceTriggerProps(");
  });
});

describe("the issue page's first render", () => {
  test("is the route's element, and that element renders the page in a transition", () => {
    expect(app).toContain('<Route element={<IssuePageInATransition />} path="/issues/:key/*" />');
    const gate = app.slice(app.indexOf("function IssuePageInATransition("));
    expect(gate).toContain("startTransition(() => setRenderThePage(true));");
    // Until the transition commits the reader has the page's own skeleton, which is what the
    // Suspense boundary above shows while its chunk arrives: nothing new appears in between.
    expect(gate.slice(0, gate.indexOf("\n}\n"))).toContain(
      "renderThePage ? <IssuePage /> : <IssuePageFallback />"
    );
  });
});
