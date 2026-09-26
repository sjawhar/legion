/**
 * The pinned dependency has to arrive patched, and its types have to agree with the copy this
 * package re-exports on its public surface. Both fail silently otherwise.
 *
 * `patchedDependencies` keys a patch on `<name>@<version>`, and a git dependency's version is its
 * ref string. Bun applies nothing when the key stops matching — no warning, exit 0 — so a pin
 * bump that misses the root `package.json` key (LEGION-287 step 2 moves the pin) puts peer-cursor
 * colours and mark decorations back into inline `style` attributes, which is the Dark Reader
 * redraw loop legion #1234 fixed. Nothing else observes that before a browser tab wedges: the
 * decorations are built inside the mark plugin's own view, with no exported seam, so the patched
 * lines are read where they live. Each string below is one hunk of
 * patches/proof-sdk-upstream@24a5fc94.patch.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const upstreamSrc = join(import.meta.dir, "..", "node_modules", "proof-sdk-upstream", "src");

test("the pinned dependency arrives with the Dark Reader patch applied", () => {
  const marks = readFileSync(join(upstreamSrc, "editor/plugins/marks.ts"), "utf8");
  expect(marks).not.toContain("style: STYLES.compose_anchor");
  expect(marks).not.toContain("span.style.cssText = STYLES.insert");
  expect(marks).toContain(
    "class: [cssClass, glowClass].filter(Boolean).join(' '),\n            'data-mark-id': mark.id,"
  );

  const cursors = readFileSync(join(upstreamSrc, "editor/plugins/collab-cursors.ts"), "utf8");
  expect(cursors).not.toContain("cursorWidget.style.setProperty('--proof-collab-cursor-color'");
  expect(cursors).toContain(
    "cursorWidget.setAttribute('data-proof-collab-cursor', proofSelectionStyleFor(color));"
  );
  expect(cursors).toContain("'data-proof-collab-selection': proofSelectionStyleFor(color),");
  expect(cursors).toContain("proof-collab-selection-styles");
});

test("src/upstream-types.ts still describes the files it was copied from", () => {
  const copied = readFileSync(join(import.meta.dir, "..", "src", "upstream-types.ts"), "utf8");
  const regions = [
    ...copied.matchAll(
      /\/\* --- copied from proof-sdk src\/(\S+) @ 24a5fc94 --- \*\/([\s\S]*?)\/\* --- end copy --- \*\//g
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
