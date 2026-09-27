/**
 * The pin has to carry every fix the fork's `library` line carries, and `upstream/` — the
 * declarations tsc reads in its place (AGENTS.md § The upstream boundary) — has to be what the
 * pinned sources emit. Both otherwise fail silently: nothing in this repository reads those
 * editor modules until a browser renders a document with them, and a stale declaration is a
 * shape tsc believes and the runtime does not have.
 *
 * Each fix case below is one member of that line, read where it lives, because none of them has
 * an exported seam a unit test could call. A cut that loses one — the first cut of the cleaned
 * line lost the cursor label — passes every other check in the repository.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

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

test("the pinned dependency keeps the collaboration cursor label inline", () => {
  // A block label lets a browser move a post-update text selection into the cursor decoration,
  // which interrupts local typing after a remote edit.
  const cursors = readFileSync(join(upstreamSrc, "editor/plugins/collab-cursors.ts"), "utf8");
  expect(cursors).toContain("const label = document.createElement('span');");
  expect(cursors).toContain("label.contentEditable = 'false';");
  expect(cursors).not.toContain("const label = document.createElement('div');");
});

test("the pinned dependency renders replacement suggestions", () => {
  // The suggestion mark's DOM attributes have to stay primitive: spreading the ctx attrs put
  // "[object Object]" on the span. And the replace-insert widget is keyed by its replacement,
  // so a changed replacement redraws instead of keeping the first content it rendered.
  const proofMarks = readFileSync(join(upstreamSrc, "editor/schema/proof-marks.ts"), "utf8");
  expect(proofMarks).not.toContain("const attrs = ctx.get(proofSuggestionAttr.key)(mark);");

  const marks = readFileSync(join(upstreamSrc, "editor/plugins/marks.ts"), "utf8");
  expect(marks).toMatch(/key: `replace-insert-\$\{mark\.id\}-\$\{replacementContent\}`/);
});

test("upstream/ is what the pinned sources emit", async () => {
  const generator =
    await Bun.$`bun ${join(import.meta.dir, "..", "scripts", "upstream-declarations.ts")} --check`
      .cwd(join(import.meta.dir, ".."))
      .nothrow()
      .quiet();
  const output = generator.stdout.toString() + generator.stderr.toString();
  expect(output).toContain("matches the pin");
  expect(generator.exitCode).toBe(0);
});
