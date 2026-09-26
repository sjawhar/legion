/**
 * The pinned dependency must carry the Dark Reader fix and agree with the upstream type regions
 * this package re-exports. Both otherwise fail silently.
 *
 * Dark Reader rewrites inline decoration styles inside the contenteditable, which ProseMirror
 * observes as mutations and redraws indefinitely. The source pin carries the upstream fix, so
 * the guard reads those editor modules directly.
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

test("src/upstream-types.ts still describes the files it was copied from", () => {
  const copied = readFileSync(join(import.meta.dir, "..", "src", "upstream-types.ts"), "utf8");
  const regions = [
    ...copied.matchAll(
      /\/\* --- copied from proof-sdk src\/(\S+) @ 9140b699 --- \*\/([\s\S]*?)\/\* --- end copy --- \*\//g
    ),
  ];
  const named: Record<string, string[]> = {};
  for (const [, path, body] of regions) {
    const declarations = (body ?? "")
      .split(/\n(?=export (?:type|interface) )/)
      .slice(1)
      .map((block) => block.trimEnd());
    named[path ?? ""] = declarations.map((block) => block.split("\n")[0] ?? "");
    const pinned = readFileSync(join(upstreamSrc, path ?? ""), "utf8");
    for (const declaration of declarations) {
      expect(pinned).toContain(declaration);
    }
  }
  // Losing a region's markers would silently stop checking it, so the set is pinned by name.
  expect(named).toEqual({
    "editor/plugins/heatmap-decorations.ts": [
      "export type HeatMapMode = 'hidden' | 'subtle' | 'background' | 'full';",
    ],
    "formats/marks.ts": [
      "export type MarkKind =",
      "export type SuggestionStatus = 'pending' | 'accepted' | 'rejected';",
      "export interface MarkRange {",
      "export interface CommentReply {",
      "export interface StoredMark {",
    ],
  });
});
