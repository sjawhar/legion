import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { blockIdOf } from "../src/editor/schema/block-ids";
import { withMarksEditor } from "./marks-editor";

const ADDED = "dispatch-change-added";
const CHANGED = "dispatch-change-block";
const REMOVED = "dispatch-change-removed";
const REMOVED_WORDS = "dispatch-change-removed-words";

/** The text of each element the editor draws with `className`, in document order. */
function drawn(view: EditorView, className: string): string[] {
  return Array.from(
    view.dom.querySelectorAll(`.${className}`),
    (element) => element.textContent ?? ""
  );
}

/** Every element the editor draws at the top of the document, as its tag and its text: a widget
 *  stands among the blocks, so this says where one was drawn. */
function topOfDocument(view: EditorView): string[] {
  return Array.from(
    view.dom.children,
    (element) => `${element.tagName.toLowerCase()}:${element.textContent ?? ""}`
  );
}

/** The offsets of `word` in the text of the document's block at `index`, as a host computes them
 *  from the block's text alone. */
function wordRange(view: EditorView, index: number, word: string): { from: number; to: number } {
  const text = view.state.doc.child(index).textContent;
  const from = text.indexOf(word);
  if (from < 0) throw new Error(`block ${index} ("${text}") does not hold "${word}"`);
  return { from, to: from + word.length };
}

test("an inline decoration marks the characters its offsets name, and a block entry the whole block", async () => {
  await withMarksEditor(
    "The plan ships behind a canary today.\n\nA second paragraph.",
    ({ handle, view }) => {
      handle.setChangeDecorations({
        text: [
          { blockIndex: 0, className: CHANGED },
          { blockIndex: 0, className: ADDED, range: wordRange(view, 0, "canary") },
        ],
        widgets: [],
      });

      expect(drawn(view, ADDED)).toEqual(["canary"]);
      expect(drawn(view, CHANGED)).toEqual(["The plan ships behind a canary today."]);
      // The block entry is a node decoration: the class sits on the paragraph itself.
      expect(view.dom.querySelector(`p.${CHANGED}`)?.textContent).toBe(
        "The plan ships behind a canary today."
      );
      // The second paragraph is unmarked.
      expect(view.dom.querySelectorAll(`.${ADDED}, .${CHANGED}`)).toHaveLength(2);
    },
    { readOnly: true }
  );
});

test("an inline decoration counts a block's own text, not its document positions", async () => {
  // The image takes no character of the block's text and two positions in the document, and the
  // emphasis makes "bold" a text node of its own: an offset after both still lands on its word.
  await withMarksEditor("![shot](shot.png)A *bold* claim here.", ({ handle, view }) => {
    handle.setChangeDecorations({
      text: [{ blockIndex: 0, className: ADDED, range: wordRange(view, 0, "claim") }],
      widgets: [],
    });

    expect(drawn(view, ADDED)).toEqual(["claim"]);
  });
});

test("an inline decoration counts the newline a hard break draws in the block's text", async () => {
  // Commonmark's `hardbreak` declares `leafText: () => "\n"`, so the block's text counts one
  // character for the break where its text nodes count none: a host reading its offsets off
  // that text - the approval column's marks do - names characters one further on per break.
  await withMarksEditor("Line one  \nthe canary word.", ({ handle, view }) => {
    expect(view.state.doc.child(0).textContent).toBe("Line one\nthe canary word.");

    handle.setChangeDecorations({
      text: [{ blockIndex: 0, className: ADDED, range: wordRange(view, 0, "canary") }],
      widgets: [],
    });

    expect(drawn(view, ADDED)).toEqual(["canary"]);
  });
});

test("an entry naming a block the document does not hold, or text it does not reach, draws nothing", async () => {
  await withMarksEditor("One short block.", ({ handle, view }) => {
    handle.setChangeDecorations({
      text: [
        { blockIndex: 7, className: CHANGED },
        { blockIndex: 7, className: ADDED, range: { from: 0, to: 3 } },
        { blockIndex: 0, className: ADDED, range: { from: 2, to: 400 } },
      ],
      widgets: [],
    });

    expect(drawn(view, ADDED)).toEqual([]);
    expect(drawn(view, CHANGED)).toEqual([]);
    expect(view.state.doc.textContent).toBe("One short block.");
  });
});

test("a widget is drawn before the block it names and at the end of the document", async () => {
  await withMarksEditor(
    "First kept.\n\nSecond kept.",
    ({ handle, view }) => {
      handle.setChangeDecorations({
        text: [],
        widgets: [
          { blockIndex: 1, className: REMOVED, text: "The block that stood here." },
          { blockIndex: view.state.doc.childCount, className: REMOVED, text: "The last block." },
        ],
      });

      expect(topOfDocument(view)).toEqual([
        "p:First kept.",
        "div:The block that stood here.",
        "p:Second kept.",
        "div:The last block.",
      ]);
      // The widget is drawn beside the document, never read back as part of it.
      for (const widget of view.dom.querySelectorAll(`.${REMOVED}`)) {
        expect(widget.getAttribute("contenteditable")).toBe("false");
      }
      expect(view.state.doc.textContent).toBe("First kept.Second kept.");
    },
    { readOnly: true }
  );
});

test("a widget with an offset is drawn at that point inside the block's text", async () => {
  await withMarksEditor(
    "The plan ships today.\n\nSecond kept.",
    ({ handle, view }) => {
      const at = view.state.doc.child(0).textContent.indexOf("today");
      handle.setChangeDecorations({
        text: [],
        widgets: [{ blockIndex: 0, className: REMOVED_WORDS, offset: at, text: "behind a flag " }],
      });

      // The widget stands where the words it holds were removed from, not before the block.
      expect(topOfDocument(view)).toEqual([
        "p:The plan ships behind a flag today.",
        "p:Second kept.",
      ]);
      const widget = view.dom.querySelector(`.${REMOVED_WORDS}`);
      expect(widget?.parentElement?.tagName.toLowerCase()).toBe("p");
      // Inside a block's text only an inline element belongs.
      expect(widget?.tagName.toLowerCase()).toBe("span");
      expect(widget?.getAttribute("contenteditable")).toBe("false");
      expect(view.state.doc.textContent).toBe("The plan ships today.Second kept.");
    },
    { readOnly: true }
  );
});

test("a widget whose offset the block's text does not reach draws nothing", async () => {
  await withMarksEditor("One short block.", ({ handle, view }) => {
    handle.setChangeDecorations({
      text: [],
      widgets: [
        { blockIndex: 0, className: REMOVED_WORDS, offset: 400, text: "Gone since v3." },
        { blockIndex: 7, className: REMOVED_WORDS, offset: 0, text: "Gone with its block." },
      ],
    });

    expect(drawn(view, REMOVED_WORDS)).toEqual([]);
    expect(view.state.doc.textContent).toBe("One short block.");
  });
});

// INFRA-575: a paragraph a version replaced by a rule or an image pairs as a changed block, and
// its lost words arrive as a widget at an offset into a block whose text holds no run at all.
// There is no position inside such a block to draw them at, so they are drawn before it rather
// than dropped: an approver who is shown the changed-block bar must be shown what it lost.
test("a widget with an offset in a block with no text of its own is drawn at that block's start", async () => {
  await withMarksEditor(
    "First kept.\n\n---\n\nSecond kept.",
    ({ handle, view }) => {
      handle.setChangeDecorations({
        text: [{ blockIndex: 1, className: CHANGED }],
        widgets: [
          { blockIndex: 1, className: REMOVED_WORDS, offset: 0, text: "The plan ships today." },
        ],
      });

      expect(drawn(view, REMOVED_WORDS)).toEqual(["The plan ships today."]);
      // The block holds no text to stand inside, so the words stand among the blocks, before it.
      expect(topOfDocument(view)).toEqual([
        "p:First kept.",
        "div:The plan ships today.",
        "hr:",
        "p:Second kept.",
      ]);
      expect(view.state.doc.textContent).toBe("First kept.Second kept.");
    },
    { readOnly: true }
  );
});

test("both lists are drawn again after setMarkdown replaces the document", async () => {
  await withMarksEditor(
    "Before the replacement.",
    ({ handle, view }) => {
      handle.setChangeDecorations({
        text: [{ blockIndex: 0, className: CHANGED }],
        widgets: [{ blockIndex: 1, className: REMOVED, text: "Gone since v3." }],
      });
      expect(drawn(view, CHANGED)).toEqual(["Before the replacement."]);

      handle.setMarkdown("The plan ships behind a canary today.\n\nA second paragraph.\n");

      expect(drawn(view, CHANGED)).toEqual(["The plan ships behind a canary today."]);
      expect(drawn(view, REMOVED)).toEqual(["Gone since v3."]);
      expect(topOfDocument(view)).toEqual([
        "p:The plan ships behind a canary today.",
        "div:Gone since v3.",
        "p:A second paragraph.",
      ]);

      handle.setChangeDecorations({ text: [], widgets: [] });
      expect(drawn(view, CHANGED)).toEqual([]);
      expect(drawn(view, REMOVED)).toEqual([]);
    },
    { readOnly: true }
  );
});

// INFRA-575 asked whether a document `setMarkdown` builds carries a block id on an untyped block,
// since an entry that named its block by id would need one. It does: the block-id plugin stamps
// every identified block of a document it is given, a plain paragraph the import parser built
// included, so the ids are fresh ones rather than the replaced document's. The two methods above
// name a block by its index among the document's top-level blocks, so nothing here depends on it.
test("a document setMarkdown builds carries a block id on a plain paragraph", async () => {
  await withMarksEditor(
    "Seeded through the collab document.",
    ({ handle, view }) => {
      const seeded = blockIdOf(view.state.doc.child(0));
      expect(seeded).not.toBeNull();

      handle.setMarkdown("A plain paragraph.\n");

      const replaced = blockIdOf(view.state.doc.child(0));
      expect(replaced).not.toBeNull();
      expect(replaced).not.toBe(seeded);
    },
    { readOnly: true }
  );
});
