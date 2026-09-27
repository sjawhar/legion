/**
 * The pin has to carry every fix the fork's `library` line carries, and `upstream/` — the
 * declarations tsc reads in its place (AGENTS.md § The upstream boundary) — has to be what the
 * pinned sources emit. Both otherwise fail silently: nothing in this repository reads those
 * editor modules until a browser renders a document with them, and a stale declaration is a
 * shape tsc believes and the runtime does not have.
 *
 * Each fix case below is one member of that line. The suggestion mark's attributes are checked
 * by rendering the mark from the schema the headless engine builds; the rest have no exported
 * seam a unit test could call, so they are read where they live. A cut that loses one passes
 * every other check in the repository.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createHeadlessProof } from "../src/lib-headless.js";

const upstreamSrc = join(import.meta.dir, "..", "node_modules", "proof-sdk-upstream", "src");

test("the pinned dependency carries the Dark Reader fix", () => {
  const marks = readFileSync(join(upstreamSrc, "editor/plugins/marks.ts"), "utf8");
  expect(marks).not.toContain("const STYLES =");
  expect(marks).not.toContain("style: STYLES.compose_anchor");
  expect(marks).not.toContain("span.style.cssText = STYLES.insert");
  expect(marks).toContain("class: [cssClass, glowClass].filter(Boolean).join(' '),");

  const cursors = readFileSync(join(upstreamSrc, "editor/plugins/collab-cursors.ts"), "utf8");
  expect(cursors).not.toContain("cursorWidget.style.setProperty");
  expect(cursors).not.toContain("'data-proof-collab-selection':");
  expect(cursors).toContain("function ensureCollabColorStyles");
  expect(cursors).toContain("proof-collab-selection--");
});

test("the pinned dependency renders replacement suggestions", async () => {
  // The suggestion mark's DOM attributes have to stay strings: an object among them reaches the
  // span as "[object Object]". And the replace-insert widget is keyed by its replacement, so a
  // changed replacement redraws instead of keeping the first content it rendered.
  const { schema } = await createHeadlessProof();
  const suggestion = schema.marks.proofSuggestion;
  if (suggestion === undefined) throw new Error("the editor schema has no proofSuggestion mark");
  const mark = suggestion.create({ by: "ai:tester", id: "suggestion-1", kind: "replace" });
  const rendered: unknown = suggestion.spec.toDOM?.(mark, true);
  if (!Array.isArray(rendered)) throw new Error("proofSuggestion renders no DOM output spec");
  expect(rendered[1]).toEqual({
    "data-by": "ai:tester",
    "data-id": "suggestion-1",
    "data-kind": "replace",
    "data-proof": "suggestion",
  });

  const marks = readFileSync(join(upstreamSrc, "editor/plugins/marks.ts"), "utf8");
  expect(marks).toMatch(/key: `replace-insert-\$\{mark\.id\}-\$\{replacementContent\}`/);
});

// The timeout is explicit because the case spawns tsc over the whole upstream closure — two
// TypeScript programs — and that is the work, not a hang. Bun's 5 s default is under the cost on
// a two-core runner, where the timeout would read exactly like a stale `upstream/`.
test("upstream/ is what the pinned sources emit", async () => {
  const generator =
    await Bun.$`bun ${join(import.meta.dir, "..", "scripts", "upstream-declarations.ts")} --check`
      .cwd(join(import.meta.dir, ".."))
      .nothrow()
      .quiet();
  const output = generator.stdout.toString() + generator.stderr.toString();
  expect(output).toContain("matches the pin");
  expect(generator.exitCode).toBe(0);
}, 120_000);
