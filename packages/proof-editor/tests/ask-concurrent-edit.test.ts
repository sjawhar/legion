/**
 * An ask two collaborators' concurrent edits leave off its `paragraph+ bullet_list?` rule (here
 * a second option list) stays in every editor and in the shared Yjs document, where y-prosemirror
 * used to delete it, with all its text, the moment its `createChecked` threw (sjawhar/legion#2433). Local
 * edits on it still hold to the rule, and every other check y-prosemirror relies on is as before.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { Fragment, type Node as ProseMirrorNode, Slice } from "@milkdown/kit/prose/model";
import { TextSelection, type Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import * as Y from "yjs";
import { BLOCK_ID_ATTR } from "../src/lib-headless.js";
import { serverBlockSchema as blockSchema, blocks } from "./block-schema-fixture";
import { withMarksEditor } from "./marks-editor";

/** `Intro`, then an ask whose question is `Which one?` and whose options are `Option A` and `After`. */
const ASK_DOC = "Intro\n\n:::ask{#a1}\nWhich one?\n\n- Option A\n- After\n:::\n";
/** Every word of `ASK_DOC` and of the list a collaborator adds to its ask. */
const WORDS = ["Intro", "Which one?", "Option A", "After", "Option B"];

/** The `ask` block of `doc`, if it holds one. */
function askOf(doc: ProseMirrorNode): ProseMirrorNode | undefined {
  let ask: ProseMirrorNode | undefined;
  doc.descendants((node) => {
    if (node.type.name === "ask") ask = node;
    return ask === undefined;
  });
  return ask;
}

/** The `ask` block of `doc`, which must hold one. */
function requireAsk(doc: ProseMirrorNode): ProseMirrorNode {
  const ask = askOf(doc);
  if (ask === undefined) throw new Error("the ask is gone");
  return ask;
}

/** The position just inside the start of the first text block of `doc` that reads `text`. */
function textblockStart(doc: ProseMirrorNode, text: string): number {
  let at = -1;
  doc.descendants((node, pos) => {
    if (at === -1 && node.isTextblock && node.textContent === text) at = pos + 1;
    return at === -1;
  });
  if (at === -1) throw new Error(`no text block reads ${JSON.stringify(text)}`);
  return at;
}

/** The ask's element in a Yjs document's `prosemirror` fragment, as y-prosemirror stores it. */
function yAskOf(ydoc: Y.Doc): Y.XmlElement | undefined {
  return ydoc
    .getXmlFragment("prosemirror")
    .toArray()
    .find(
      (child): child is Y.XmlElement => child instanceof Y.XmlElement && child.nodeName === "ask"
    );
}

/** The ask's element in `ydoc`, which must hold one. */
function requireYAsk(ydoc: Y.Doc): Y.XmlElement {
  const ask = yAskOf(ydoc);
  if (ask === undefined) throw new Error("the Yjs document holds no ask");
  return ask;
}

/** The node names of the ask's children in `ydoc`'s fragment, an element's `nodeName` each. */
function yAskChildren(ydoc: Y.Doc): string[] {
  return requireYAsk(ydoc)
    .toArray()
    .map((child) => (child instanceof Y.XmlElement ? child.nodeName : "#text"));
}

/** A collaborator's replica of the editor's shared document. */
function peerOf(ydoc: Y.Doc): Y.Doc {
  const peer = new Y.Doc();
  Y.applyUpdate(peer, Y.encodeStateAsUpdate(ydoc));
  return peer;
}

/** Delivers the edits `peer` made since it last synced to the editor's document. */
function deliver(peer: Y.Doc, ydoc: Y.Doc): void {
  Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(peer, Y.encodeStateVector(ydoc)));
}

/** The child of `element` at `index`, which must be an element. */
function childElement(element: Y.XmlElement, index: number): Y.XmlElement {
  const child = element.get(index);
  if (!(child instanceof Y.XmlElement))
    throw new Error(`child ${index} of ${element.nodeName} is not an element`);
  return child;
}

/**
 * A one-item bullet list reading `text`, as y-prosemirror stores one: an element per node, each
 * carrying its node's attributes, copied from the ask's own list `template`, with a fresh block
 * id when `ids` and none otherwise.
 */
function yBulletList(template: Y.XmlElement, text: string, ids: boolean): Y.XmlElement {
  const copy = (source: Y.XmlElement, child: Y.XmlElement | Y.XmlText): Y.XmlElement => {
    const element = new Y.XmlElement(source.nodeName);
    for (const [key, value] of Object.entries(source.getAttributes())) {
      if (key !== BLOCK_ID_ATTR && value !== undefined) element.setAttribute(key, value);
    }
    if (ids) element.setAttribute(BLOCK_ID_ATTR, crypto.randomUUID());
    element.insert(0, [child]);
    return element;
  };
  const item = childElement(template, 0);
  const paragraph = childElement(item, 0);
  return copy(template, copy(item, copy(paragraph, new Y.XmlText(text))));
}

/**
 * A collaborator appends a second option list, one option reading `text` (`Option B` unless
 * given), to the ask, and the editor receives it.
 */
function peerAppendsList(ydoc: Y.Doc, ids: boolean, text = "Option B"): void {
  const peer = peerOf(ydoc);
  const ask = requireYAsk(peer);
  ask.insert(ask.length, [yBulletList(childElement(ask, 1), text, ids)]);
  deliver(peer, ydoc);
}

/** Asserts every word of the document and the collaborator's list is in the view and in Yjs. */
function expectEveryWord(view: EditorView, ydoc: Y.Doc): void {
  const yText = ydoc.getXmlFragment("prosemirror").toString();
  for (const word of WORDS) {
    expect(view.state.doc.textContent).toContain(word);
    expect(yText).toContain(word);
  }
}

/** Asserts the editor's ask holds the question and two option lists, in the view and in Yjs. */
function expectTwoListAsk(view: EditorView, ydoc: Y.Doc): ProseMirrorNode {
  const ask = requireAsk(view.state.doc);
  expect(blocks(ask)).toEqual([
    ["paragraph", "Which one?"],
    ["bullet_list", "Option AAfter"],
    ["bullet_list", "Option B"],
  ]);
  expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list", "bullet_list"]);
  expectEveryWord(view, ydoc);
  return ask;
}

test("an ask a collaborator gives a second option list survives in the view and in Yjs", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      expect(askOf(view.state.doc)).toBeDefined();
      peerAppendsList(ydoc, true);
      expectTwoListAsk(view, ydoc);
    },
    { blockSchema }
  );
});

test("a collaborator's second option list without block ids gets one from the next local edit", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, false);
      const before = expectTwoListAsk(view, ydoc);
      expect(before.child(2).attrs[BLOCK_ID_ATTR]).toBeFalsy();
      const introEnd = textblockStart(view.state.doc, "Intro") + "Intro".length;
      expect(() => view.dispatch(view.state.tr.insertText("!", introEnd))).not.toThrow();
      const ask = expectTwoListAsk(view, ydoc);
      const listId = ask.child(2).attrs[BLOCK_ID_ATTR];
      expect(typeof listId).toBe("string");
      expect(listId).not.toBe("");
      expect(childElement(requireYAsk(ydoc), 2).getAttribute(BLOCK_ID_ATTR)).toBe(listId);
    },
    { blockSchema }
  );
});

test("edits on an ask holding two option lists keep every word and join the lists back into one", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      expectTwoListAsk(view, ydoc);

      // Typing in the question.
      const questionEnd = textblockStart(view.state.doc, "Which one?") + "Which one?".length;
      view.dispatch(view.state.tr.insertText(" Pick", questionEnd));
      expect(blocks(requireAsk(view.state.doc))[0]).toEqual(["paragraph", "Which one? Pick"]);
      expectEveryWord(view, ydoc);

      // Bold from the first list into the second.
      const strong = view.state.schema.marks.strong;
      const from = textblockStart(view.state.doc, "Option A");
      const to = textblockStart(view.state.doc, "Option B") + "Option B".length;
      view.dispatch(view.state.tr.addMark(from, to, strong.create()));
      expect(view.state.doc.rangeHasMark(from, from + "Option A".length, strong)).toBe(true);
      expect(view.state.doc.rangeHasMark(to - "Option B".length, to, strong)).toBe(true);
      expectEveryWord(view, ydoc);

      // Backspace at the start of the second list joins it to the first.
      const secondList = textblockStart(view.state.doc, "Option B");
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, secondList)));
      press("Backspace");
      const ask = requireAsk(view.state.doc);
      expect(blocks(ask)).toEqual([
        ["paragraph", "Which one? Pick"],
        ["bullet_list", "Option AAfterOption B"],
      ]);
      expect(ask.type.contentMatch.matchFragment(ask.content)?.validEnd).toBe(true);
      expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list"]);
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

test("Enter at the end of an option in an ask holding two option lists keeps every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const optionEnd = textblockStart(view.state.doc, "Option A") + "Option A".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, optionEnd)));
      press("Enter");
      const ask = requireAsk(view.state.doc);
      expect(ask.childCount).toBe(3);
      expect(ask.child(1).childCount).toBe(3);
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

test("an ask a collaborator empties of every block is still deleted", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      const peer = peerOf(ydoc);
      const ask = requireYAsk(peer);
      ask.delete(0, ask.length);
      deliver(peer, ydoc);
      expect(askOf(view.state.doc)).toBeUndefined();
      expect(yAskOf(ydoc)).toBeUndefined();
      expect(blocks(view.state.doc)).toEqual([["paragraph", "Intro"]]);
    },
    { blockSchema }
  );
});

test("a list a collaborator leaves holding a bare paragraph is still deleted", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      const peer = peerOf(ydoc);
      const paragraph = new Y.XmlElement("paragraph");
      paragraph.insert(0, [new Y.XmlText("Stray words")]);
      const list = new Y.XmlElement("bullet_list");
      list.insert(0, [paragraph]);
      peer.getXmlFragment("prosemirror").insert(1, [list]);
      deliver(peer, ydoc);
      expect(view.state.doc.textContent).not.toContain("Stray words");
      expect(ydoc.getXmlFragment("prosemirror").toString()).not.toContain("Stray words");
      expect(blocks(view.state.doc).map(([type]) => type)).toEqual(["paragraph", "ask"]);
    },
    { blockSchema }
  );
});

test("the server's block node types and the schema's typed blocks are the live schema's block group", async () => {
  // The list pmdoc's isBlockNodeType embeds (packages/envoy/internal/dispatch/pmdoc/node.go).
  const serverBlockNodeTypes: string[] = JSON.parse(
    readFileSync(
      new URL("../../envoy/internal/dispatch/pmdoc/schema/block-node-types.json", import.meta.url),
      "utf8"
    )
  );
  await withMarksEditor(
    "",
    ({ view }) => {
      const blockGroup = Object.values(view.state.schema.nodes)
        .filter((type) => type.isInGroup("block"))
        .map((type) => type.name);
      const accepted = [...serverBlockNodeTypes, ...blockSchema.types.map((type) => type.name)];
      expect(accepted.sort()).toEqual(blockGroup.sort());
    },
    { blockSchema }
  );
});

test("Enter at the end of a valid ask's question splits it as before", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view }) => {
      const questionEnd = textblockStart(view.state.doc, "Which one?") + "Which one?".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, questionEnd)));
      press("Enter");
      const ask = requireAsk(view.state.doc);
      expect(blocks(ask)).toEqual([
        ["paragraph", "Which one?"],
        ["paragraph", ""],
        ["bullet_list", "Option AAfter"],
      ]);
    },
    { blockSchema }
  );
});

test("Enter twice at the end of the second option list's option keeps the ask and every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const optionEnd = textblockStart(view.state.doc, "Option B") + "Option B".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, optionEnd)));
      expect(() => press("Enter")).not.toThrow();
      expect(() => press("Enter")).not.toThrow();
      requireAsk(view.state.doc);
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

test("Backspace at the start of the first option in an ask holding two option lists keeps every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const optionStart = textblockStart(view.state.doc, "Option A");
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, optionStart)));
      expect(() => press("Backspace")).not.toThrow();
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

test("Backspace over a selection from the question into an option of a two-list ask keeps every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const from = textblockStart(view.state.doc, "Which one?") + "Which".length;
      const to = textblockStart(view.state.doc, "Option A") + "Opt".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
      expect(() => press("Backspace")).not.toThrow();
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

/**
 * Cuts the selection as ProseMirror's cut listener does: every `handleDOMEvents.cut` first, then,
 * unless one takes the event, its own cut, which deletes the selection. Returns the event.
 */
function cutSelection(view: EditorView): ClipboardEvent {
  const cut = new window.ClipboardEvent("cut", { bubbles: true, cancelable: true });
  if (!view.someProp("handleDOMEvents", (handlers) => handlers.cut?.(view, cut))) {
    view.dispatch(view.state.tr.deleteSelection());
  }
  return cut;
}

/**
 * Types `text` over the selection as ProseMirror's keypress listener does: every
 * `handleTextInput` first, then its own `insertText` over the selection when none takes it.
 * happy-dom does not deliver a synthetic keypress to that listener, so the harness makes the calls
 * itself.
 */
function typeText(view: EditorView, text: string): void {
  const { from, to } = view.state.selection;
  const deflt = () => view.state.tr.insertText(text);
  if (!view.someProp("handleTextInput", (handle) => handle(view, from, to, text, deflt))) {
    view.dispatch(deflt());
  }
}

test("typing over a selection from the question into an option of a two-list ask keeps every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const from = textblockStart(view.state.doc, "Which one?") + "Which".length;
      const to = textblockStart(view.state.doc, "Option A") + "Opt".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
      expect(() => typeText(view, "x")).not.toThrow();
      expectTwoListAsk(view, ydoc);
    },
    { blockSchema }
  );
});

test("typing at a caret in a two-list ask inserts the text", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const optionEnd = textblockStart(view.state.doc, "Option B") + "Option B".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, optionEnd)));
      typeText(view, "!");
      expect(blocks(requireAsk(view.state.doc))[2]).toEqual(["bullet_list", "Option B!"]);
      expectEveryWord(view, ydoc);
    },
    { blockSchema }
  );
});

test("cutting a selection from the question into an option of a two-list ask keeps every word", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const from = textblockStart(view.state.doc, "Which one?") + "Which".length;
      const to = textblockStart(view.state.doc, "Option A") + "Opt".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
      let cut: Event | undefined;
      expect(() => {
        cut = cutSelection(view);
      }).not.toThrow();
      expect(cut?.defaultPrevented).toBe(true);
      expectTwoListAsk(view, ydoc);
    },
    { blockSchema }
  );
});

test("cutting a selection inside one block of a two-list ask deletes it", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const from = textblockStart(view.state.doc, "Which one?") + "Which".length;
      view.dispatch(
        view.state.tr.setSelection(TextSelection.create(view.state.doc, from, from + " one".length))
      );
      expect(cutSelection(view).defaultPrevented).toBe(false);
      expect(blocks(requireAsk(view.state.doc))[0]).toEqual(["paragraph", "Which?"]);
    },
    { blockSchema }
  );
});

/**
 * Offers a `beforeinput` of `inputType` (carrying `data`) to every `handleDOMEvents.beforeinput`,
 * as ProseMirror's listener does, and returns it; a browser applies its own edit to the DOM unless
 * one prevented it.
 */
function beforeInput(view: EditorView, inputType: string, data: string | null): InputEvent {
  const event = new window.InputEvent("beforeinput", { cancelable: true, data, inputType });
  view.someProp("handleDOMEvents", (handlers) => handlers.beforeinput?.(view, event));
  return event;
}

test("input without a keypress over a selection across blocks of a two-list ask is ProseMirror's own edit, not the browser's", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const from = textblockStart(view.state.doc, "Which one?") + "Which".length;
      // Into the first list, which keeps an item after it: ProseMirror's edit throws and is declined.
      const intoFirst = textblockStart(view.state.doc, "Option A") + "Opt".length;
      view.dispatch(
        view.state.tr.setSelection(TextSelection.create(view.state.doc, from, intoFirst))
      );
      expect(beforeInput(view, "insertText", "x").defaultPrevented).toBe(true);
      expectTwoListAsk(view, ydoc);

      // Into the second list, past the whole first one: ProseMirror deletes the range.
      const intoSecond = textblockStart(view.state.doc, "Option B") + "Opt".length;
      view.dispatch(
        view.state.tr.setSelection(TextSelection.create(view.state.doc, from, intoSecond))
      );
      expect(beforeInput(view, "deleteContentBackward", null).defaultPrevented).toBe(true);
      expect(blocks(requireAsk(view.state.doc))).toEqual([["paragraph", "Whichion B"]]);
    },
    { blockSchema }
  );
});

/**
 * Selects from after `Which` in the question to after `Opt` in the first list's first option of a
 * two-list ask, and returns a replace shaped like the one ProseMirror reads back from the DOM after
 * the browser edits that selection itself (`document.execCommand`, no `beforeinput`): it runs from
 * the selection's start to the end of the ask's content and leaves `Whichxion A` and the first
 * list's last option, so the second list, which the selection never touched, is dropped.
 */
function readBackReplace(view: EditorView): Transaction {
  const { doc, schema } = view.state;
  const from = textblockStart(doc, "Which one?") + "Which".length;
  const to = textblockStart(doc, "Option A") + "Opt".length;
  view.dispatch(view.state.tr.setSelection(TextSelection.create(doc, from, to)));
  let askEnd = -1;
  doc.descendants((node, pos) => {
    if (node.type.name === "ask") askEnd = pos + node.nodeSize - 1;
    return askEnd === -1;
  });
  const firstList = requireAsk(doc).child(1);
  const slice = new Slice(
    Fragment.from([
      schema.nodes.paragraph.create(null, schema.text("xion A")),
      firstList.copy(Fragment.from(firstList.lastChild)),
    ]),
    1,
    0
  );
  return view.state.tr.replace(from, askEnd, slice);
}

test("a replace read back from the DOM that drops a list the selection never touched in a two-list ask is refused", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const tr = readBackReplace(view);
      // The replace itself would apply: the ask's relaxed check accepts what it leaves.
      expect(blocks(requireAsk(tr.doc))).toEqual([
        ["paragraph", "Whichxion A"],
        ["bullet_list", "After"],
      ]);
      const before = view.state;
      let updates = 0;
      ydoc.on("update", () => updates++);
      expect(() => view.dispatch(tr)).not.toThrow();
      expect(view.state.doc).toBe(before.doc);
      expect(updates).toBe(0);
      expectTwoListAsk(view, ydoc);
    },
    { blockSchema }
  );
});

test("a replace read back from the DOM that drops a list whose text another block of a two-list ask holds is refused", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      // The second list reads `After`, as the first list's last option does, which the replace keeps.
      peerAppendsList(ydoc, true, "After");
      const tr = readBackReplace(view);
      expect(blocks(requireAsk(tr.doc))).toEqual([
        ["paragraph", "Whichxion A"],
        ["bullet_list", "After"],
      ]);
      const before = view.state;
      let updates = 0;
      ydoc.on("update", () => updates++);
      expect(() => view.dispatch(tr)).not.toThrow();
      expect(view.state.doc).toBe(before.doc);
      expect(updates).toBe(0);
      expect(blocks(requireAsk(view.state.doc))).toEqual([
        ["paragraph", "Which one?"],
        ["bullet_list", "Option AAfter"],
        ["bullet_list", "After"],
      ]);
      expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list", "bullet_list"]);
    },
    { blockSchema }
  );
});

test("a whole-document replace and a Backspace joining the lists of a two-list ask still apply", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ handle, press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      // The selection the read-back row makes, so only the edits' own shape decides.
      readBackReplace(view);
      handle.setMarkdown("Intro\n\n:::ask{#a1}\nWhich one?\n\n- Option A\n:::\n");
      expect(blocks(requireAsk(view.state.doc))).toEqual([
        ["paragraph", "Which one?"],
        ["bullet_list", "Option A"],
      ]);
      expect(ydoc.getXmlFragment("prosemirror").toString()).not.toContain("Option B");

      peerAppendsList(ydoc, true);
      const secondList = textblockStart(view.state.doc, "Option B");
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, secondList)));
      press("Backspace");
      expect(blocks(requireAsk(view.state.doc))).toEqual([
        ["paragraph", "Which one?"],
        ["bullet_list", "Option AOption B"],
      ]);
      expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list"]);
    },
    { blockSchema }
  );
});

test("text inserted into an option of a two-list ask the selection does not touch applies", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const question = textblockStart(view.state.doc, "Which one?") + 2;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, question)));
      // Into the middle of the second list's option, then at its start.
      view.dispatch(view.state.tr.insertText("x", textblockStart(view.state.doc, "Option B") + 3));
      expect(blocks(requireAsk(view.state.doc))[2]).toEqual(["bullet_list", "Optxion B"]);
      view.dispatch(view.state.tr.insertText("y", textblockStart(view.state.doc, "Optxion B")));
      expect(blocks(requireAsk(view.state.doc))).toEqual([
        ["paragraph", "Which one?"],
        ["bullet_list", "Option AAfter"],
        ["bullet_list", "yOptxion B"],
      ]);
      expect(ydoc.getXmlFragment("prosemirror").toString()).toContain("yOptxion B");
    },
    { blockSchema }
  );
});

test("deleting an image-only paragraph of a broken ask the selection does not touch is refused", async () => {
  await withMarksEditor(
    "Intro\n\n:::ask{#a1}\nWhich one?\n\n- Option A\n:::\n",
    ({ view, ydoc }) => {
      // A collaborator adds a paragraph holding only an image after the options.
      const peer = peerOf(ydoc);
      const paragraph = new Y.XmlElement("paragraph");
      paragraph.setAttribute(BLOCK_ID_ATTR, crypto.randomUUID());
      const image = new Y.XmlElement("image");
      image.setAttribute("src", "https://example.com/x.png");
      image.setAttribute("alt", "alt");
      paragraph.insert(0, [image]);
      const yAsk = requireYAsk(peer);
      yAsk.insert(yAsk.length, [paragraph]);
      deliver(peer, ydoc);
      const ask = requireAsk(view.state.doc);
      expect(ask.lastChild?.firstChild?.type.name).toBe("image");

      const question = textblockStart(view.state.doc, "Which one?") + 2;
      view.dispatch(
        view.state.tr.setSelection(TextSelection.create(view.state.doc, question, question + 3))
      );
      let askPos = -1;
      view.state.doc.descendants((node, pos) => {
        if (node === ask) askPos = pos;
        return askPos === -1;
      });
      const imageBlock = askPos + ask.nodeSize - 1 - (ask.lastChild?.nodeSize ?? 0);
      const before = view.state.doc;
      view.dispatch(view.state.tr.delete(imageBlock, askPos + ask.nodeSize - 1));
      expect(view.state.doc).toBe(before);
      expect(requireAsk(view.state.doc).lastChild?.firstChild?.type.name).toBe("image");
      expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list", "paragraph"]);
    },
    { blockSchema }
  );
});

for (const [kind, refused] of [
  ["hr", true],
  ["paragraph", false],
] as const) {
  test(`deleting ${refused ? "a divider" : "an empty paragraph"} a peer put in a broken ask outside the selection ${refused ? "is refused" : "applies"}`, async () => {
    await withMarksEditor(
      "Intro\n\n:::ask{#a1}\nWhich one?\n\n- Option A\n:::\n",
      ({ view, ydoc }) => {
        // A collaborator adds a leaf divider (or an empty paragraph) after the options.
        const peer = peerOf(ydoc);
        const block = new Y.XmlElement(kind);
        block.setAttribute(BLOCK_ID_ATTR, crypto.randomUUID());
        const yAsk = requireYAsk(peer);
        yAsk.insert(yAsk.length, [block]);
        deliver(peer, ydoc);
        const ask = requireAsk(view.state.doc);
        expect(ask.lastChild?.type.name).toBe(kind);

        const question = textblockStart(view.state.doc, "Which one?") + 2;
        view.dispatch(
          view.state.tr.setSelection(TextSelection.create(view.state.doc, question, question + 3))
        );
        let askPos = -1;
        view.state.doc.descendants((node, pos) => {
          if (node === ask) askPos = pos;
          return askPos === -1;
        });
        const last = askPos + ask.nodeSize - 1 - (ask.lastChild?.nodeSize ?? 0);
        const before = view.state.doc;
        view.dispatch(view.state.tr.delete(last, askPos + ask.nodeSize - 1));
        if (refused) {
          expect(view.state.doc).toBe(before);
          expect(requireAsk(view.state.doc).lastChild?.type.name).toBe(kind);
          expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list", kind]);
        } else {
          expect(requireAsk(view.state.doc).childCount).toBe(2);
          expect(yAskChildren(ydoc)).toEqual(["paragraph", "bullet_list"]);
        }
      },
      { blockSchema }
    );
  });
}

test("undo and redo of a letter typed in the second option list apply with the selection in the question", async () => {
  await withMarksEditor(
    ASK_DOC,
    ({ press, view, ydoc }) => {
      peerAppendsList(ydoc, true);
      const option = textblockStart(view.state.doc, "Option B") + "Option ".length;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, option)));
      view.dispatch(view.state.tr.insertText("x"));
      expect(blocks(requireAsk(view.state.doc))[2]).toEqual(["bullet_list", "Option xB"]);

      const question = textblockStart(view.state.doc, "Which one?") + 2;
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, question)));
      press("Mod-z");
      expectTwoListAsk(view, ydoc);
      expect(ydoc.getXmlFragment("prosemirror").toString()).not.toContain("Option xB");

      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, question)));
      press("Mod-Shift-z");
      expect(blocks(requireAsk(view.state.doc))[2]).toEqual(["bullet_list", "Option xB"]);
      expect(ydoc.getXmlFragment("prosemirror").toString()).toContain("Option xB");
    },
    { blockSchema }
  );
});
