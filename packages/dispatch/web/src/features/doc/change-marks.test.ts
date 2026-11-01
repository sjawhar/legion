import { beforeAll, expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import * as Y from "yjs";

import { renderWithEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import {
  ADDED_WORDS_CLASS,
  CHANGED_BLOCK_CLASS,
  changeMarksAgainst,
  REMOVED_BLOCK_CLASS,
  REMOVED_WORDS_CLASS,
} from "./change-marks";
import { createEditor } from "./editor";
import { compiledBlockSchema } from "./schema";

/** The document the editor would render for `markdown`: `changeMarksAgainst` reads the editor's
 *  own document, and the headless engine builds the same one. */
function render(markdown: string): ProseMirrorNode {
  let doc: ProseMirrorNode | undefined;
  renderWithEngine((engine) => {
    doc = engine?.parse(markdown);
  });
  if (doc === undefined) throw new Error(`the engine rendered nothing for "${markdown}"`);
  return doc;
}

// The marks are computed from the shared engine in the task that asks for them, as the page's
// shell has warmed it by the time a document version is on the screen.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

test("a word changed inside a paragraph marks that paragraph, the added words and the lost ones", () => {
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe end.\n";
  const doc = render("# Plan\n\nThe plan ships behind a canary today.\n\nThe end.\n");
  const text = "The plan ships behind a canary today.";

  expect(changeMarksAgainst(baseline, doc)).toEqual({
    text: [
      { blockIndex: 1, className: CHANGED_BLOCK_CLASS },
      {
        blockIndex: 1,
        className: ADDED_WORDS_CLASS,
        range: { from: text.indexOf("canary"), to: text.indexOf("canary") + "canary".length },
      },
    ],
    // The word the paragraph lost is drawn at the offset its replacement now stands at.
    widgets: [
      {
        blockIndex: 1,
        className: REMOVED_WORDS_CLASS,
        offset: text.indexOf("canary"),
        text: "flag",
      },
    ],
  });
});

test("a changed pair too far apart to diff is drawn as the whole block, struck through and added", () => {
  // The word diff gives up past its bound (`change-set.ts`), and the column draws what it is
  // handed: the baseline's block beside the one that replaced it, rather than nothing.
  const before = Array.from({ length: 600 }, (_, index) => `before${index}`).join(" ");
  const after = Array.from({ length: 600 }, (_, index) => `after${index}`).join(" ");
  const doc = render(`# Plan\n\n${after}\n`);

  expect(changeMarksAgainst(`# Plan\n\n${before}\n`, doc)).toEqual({
    text: [
      { blockIndex: 1, className: CHANGED_BLOCK_CLASS },
      { blockIndex: 1, className: ADDED_WORDS_CLASS, range: { from: 0, to: after.length } },
    ],
    widgets: [{ blockIndex: 1, className: REMOVED_WORDS_CLASS, offset: 0, text: before }],
  });
});

test("a paragraph that only lost words draws them where they stood", () => {
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const doc = render("# Plan\n\nThe plan ships today.\n");
  const text = "The plan ships today.";

  expect(changeMarksAgainst(baseline, doc)).toEqual({
    text: [{ blockIndex: 1, className: CHANGED_BLOCK_CLASS }],
    widgets: [
      {
        blockIndex: 1,
        className: REMOVED_WORDS_CLASS,
        offset: text.indexOf("today"),
        text: "behind a flag ",
      },
    ],
  });
});

// INFRA-575: a paragraph the version replaced by a rule pairs as a changed block, and the block
// that replaced it holds no text for the lost words to stand inside. The approver is shown the
// changed-block bar, so the words must reach the page too; this row runs them through the real
// editor, which is what draws the decoration set the marks name.
test("a paragraph replaced by a rule draws the words it lost in the editor", async () => {
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const lost = "The plan ships behind a flag today.";
  const root = document.body.appendChild(document.createElement("div"));
  const handle = await createEditor(root, {
    awareness: null,
    blockSchema: compiledBlockSchema,
    user: { color: "#f59e0b", name: "alice" },
    ydoc: new Y.Doc(),
  });

  try {
    handle.setMarkdown("# Plan\n\n---\n");
    const marks = changeMarksAgainst(baseline, handle.view.state.doc);
    expect(marks).toEqual({
      text: [{ blockIndex: 1, className: CHANGED_BLOCK_CLASS }],
      widgets: [{ blockIndex: 1, className: REMOVED_WORDS_CLASS, offset: 0, text: lost }],
    });

    handle.setChangeDecorations(marks);

    // The rule holds no text, so the editor draws the words before it rather than dropping them.
    expect(
      Array.from(
        handle.view.dom.querySelectorAll(`.${REMOVED_WORDS_CLASS}`),
        (element) => element.textContent
      )
    ).toEqual([lost]);
  } finally {
    handle.destroy();
    root.remove();
  }
});

test("a block removed since the baseline becomes a widget between the two blocks it sat between", () => {
  const baseline = "# Plan\n\nThe middle block.\n\nThe end.\n";
  const doc = render("# Plan\n\nThe end.\n");

  expect(changeMarksAgainst(baseline, doc)).toEqual({
    text: [],
    widgets: [{ blockIndex: 1, className: REMOVED_BLOCK_CLASS, text: "The middle block." }],
  });
});

test("a block removed from the end of the baseline becomes a widget at the end of the document", () => {
  const baseline = "# Plan\n\nThe end.\n";
  const doc = render("# Plan\n");

  expect(changeMarksAgainst(baseline, doc)).toEqual({
    text: [],
    widgets: [{ blockIndex: 1, className: REMOVED_BLOCK_CLASS, text: "The end." }],
  });
});

test("a baseline equal to the rendered document marks nothing", () => {
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";

  expect(changeMarksAgainst(markdown, render(markdown))).toEqual({ text: [], widgets: [] });
});

test("an absent baseline marks nothing", () => {
  expect(changeMarksAgainst("", render("# Plan\n\nA first approval request.\n"))).toEqual({
    text: [],
    widgets: [],
  });
});

test("a baseline the engine refuses marks nothing", () => {
  // A mark span of a kind this schema does not carry: the parser throws and the engine answers
  // nothing for it (`markdown-engine.ts`).
  const refused = 'see <span data-dispatch="bogus">this</span> now';

  expect(changeMarksAgainst(refused, render("# Plan\n\nThe only block.\n"))).toEqual({
    text: [],
    widgets: [],
  });
});
