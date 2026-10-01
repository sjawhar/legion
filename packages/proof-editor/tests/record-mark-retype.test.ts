import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { comment, getMarks, marksPluginKey } from "proof-sdk-upstream/src/editor/plugins/marks";
import { createAskMark } from "../src/dispatch-marks";
import {
  findRecordMark,
  RECORD_MARK_TYPES,
  removeRecordMark,
  retypeMark,
} from "../src/record-mark-retype";
import { withMarksEditor } from "./marks-editor";

const BY = "alice";
const SENTENCE = "The quick brown fox";
// "quick brown" inside the paragraph "The quick brown fox" (the paragraph opens at 0, text at 1).
const RANGE = { from: 5, to: 16 };

function marksInDoc(view: EditorView) {
  const marks: { type: string; id: unknown; from: number; to: number; kind?: unknown }[] = [];
  view.state.doc.descendants((node, pos) => {
    if (!node.isText) return true;
    for (const mark of node.marks) {
      marks.push({
        type: mark.type.name,
        id: mark.attrs.id,
        from: pos,
        to: pos + node.nodeSize,
        kind: mark.attrs.kind,
      });
    }
    return true;
  });
  return marks;
}

test("retypeMark replaces the mark with one of the new kind over the same text, under a new id", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    const start = comment(view, "quick brown", BY, "", RANGE);

    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(ask.quote).toBe("quick brown");
    expect(ask.markId).not.toBe(start.id);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "dispatchAsk", id: ask.markId, from: 5, to: 16 }),
    ]);
    expect(findRecordMark(view.state.doc, ask.markId)).toEqual({
      range: RANGE,
      type: "dispatchAsk",
    });

    const suggest = retypeMark(view, ask.markId, "suggest", BY);
    if ("refused" in suggest) throw new Error(`the suggest retype was refused: ${suggest.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofSuggestion",
        id: suggest.markId,
        from: 5,
        to: 16,
        kind: "replace",
      }),
    ]);
    expect(getMarks(view.state).map((mark) => ({ id: mark.id, kind: mark.kind }))).toEqual([
      { id: suggest.markId, kind: "replace" },
    ]);

    const back = retypeMark(view, suggest.markId, "comment", BY);
    if ("refused" in back) throw new Error(`the comment retype was refused: ${back.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: back.markId, from: 5, to: 16 }),
    ]);
    // The suggestion's metadata went with its spans: the marks plugin normalises its metadata on
    // every document change, so no stale entry is left for it to re-synthesise as a pending
    // suggestion.
    expect(Object.keys(marksPluginKey.getState(view.state)?.metadata ?? {})).toEqual([back.markId]);
  });
});

test("retypeMark refuses, changing nothing, a mark the document does not hold and a suggestion upstream will not write", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    const start = comment(view, "quick brown", BY, "", RANGE);
    const before = marksInDoc(view);
    expect(retypeMark(view, "never", "ask", BY)).toEqual({ refused: "missing" });
    expect(marksInDoc(view)).toEqual(before);
    expect(retypeMark(view, start.id, "comment", BY)).toEqual({
      markId: start.id,
      quote: "quick brown",
    });
    expect(marksInDoc(view)).toEqual(before);
    // "ick" inside "quick": upstream refuses a suggestion that starts or ends mid-word.
    const midWord = comment(view, "ick", BY, "", { from: 7, to: 10 });
    const withMidWord = marksInDoc(view);
    expect(retypeMark(view, midWord.id, "suggest", BY)).toEqual({ refused: "unmarkable" });
    expect(marksInDoc(view)).toEqual(withMidWord);
  });
  // table > header row > header("alpha"), header("beta"): "alpha" is 4..9, "beta" is 13..17.
  await withMarksEditor("| alpha | beta |\n| --- | --- |\n| x | y |", ({ view }) => {
    const across = comment(view, "alpha\nbeta", BY, "", { from: 4, to: 17 });
    const before = marksInDoc(view);
    expect(before.map((mark) => mark.type)).toEqual(["proofComment", "proofComment"]);
    expect(retypeMark(view, across.id, "suggest", BY)).toEqual({ refused: "unmarkable" });
    expect(marksInDoc(view)).toEqual(before);
    const ask = retypeMark(view, across.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(marksInDoc(view).map((mark) => mark.type)).toEqual(["dispatchAsk", "dispatchAsk"]);
  });
});

test("retypeMark refuses a kind whose mark another record already holds over part of the text", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    // Bob's recorded comment covers "quick brown"; Alice asks about "brown" inside it.
    const bob = comment(view, "quick brown", "bob", "", RANGE);
    const alice = createAskMark(view, { from: 11, to: 16 }, BY);
    if (alice === null) throw new Error("the ask mark was not written");
    const before = marksInDoc(view);
    expect(before).toEqual([
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
      expect.objectContaining({ type: "dispatchAsk", id: alice.id, from: 11, to: 16 }),
    ]);
    // A comment of Alice's over "brown" would cut "brown" out of Bob's comment: refused, and
    // Bob's spans are exactly as they were.
    expect(retypeMark(view, alice.id, "comment", BY)).toEqual({ refused: "overlaps" });
    expect(marksInDoc(view)).toEqual(before);
    // A suggestion over the same text displaces no comment, so it goes through; and back to an
    // ask, leaving Bob's comment whole.
    const suggest = retypeMark(view, alice.id, "suggest", BY);
    if ("refused" in suggest) throw new Error(`the suggest retype was refused: ${suggest.refused}`);
    const back = retypeMark(view, suggest.markId, "ask", BY);
    if ("refused" in back) throw new Error(`the ask retype was refused: ${back.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
      expect.objectContaining({ type: "dispatchAsk", id: back.markId, from: 11, to: 16 }),
    ]);
    expect(findRecordMark(view.state.doc, bob.id)).toEqual({ range: RANGE, type: "proofComment" });
    // The control for the refusal: the creator itself does cut Bob's anchor when asked to write
    // a comment over "brown" - a mark type excludes itself (LEGION-458).
    comment(view, "brown", BY, "", { from: 11, to: 16 });
    expect(findRecordMark(view.state.doc, bob.id)).toEqual({
      range: { from: 5, to: 11 },
      type: "proofComment",
    });
  });
});

test("removeRecordMark removes a provisional comment the unified marks system cannot see", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    // Upstream's `deleteMark` answers false here: a comment with no body is dropped by
    // buildAnchorMarks, so only a span-by-span removal reaches it.
    const start = comment(view, "quick brown", BY, "", RANGE);
    expect(removeRecordMark(view, start.id)).toBe(true);
    expect(marksInDoc(view)).toEqual([]);
    expect(removeRecordMark(view, start.id)).toBe(false);
  });
});

/** Every record mark on an inline node - text or an inline image - as `node:markType#id`. */
function recordMarksOnInline(view: EditorView): string[] {
  const marks: string[] = [];
  view.state.doc.descendants((node) => {
    if (!node.isInline) return true;
    for (const mark of node.marks) {
      if (RECORD_MARK_TYPES.has(mark.type.name)) {
        marks.push(`${node.type.name}:${mark.type.name}#${mark.attrs.id}`);
      }
    }
    return true;
  });
  return marks;
}

/** Where the first inline image sits in `view`'s document. */
function imagePosition(view: EditorView): number {
  let image = -1;
  view.state.doc.descendants((node, pos) => {
    if (node.type.name === "image" && image === -1) image = pos;
    return true;
  });
  if (image === -1) throw new Error("the document has no image");
  return image;
}

/** Where the first occurrence of `word` in `view`'s document ends. */
function wordEnd(view: EditorView, word: string): number {
  let end = -1;
  view.state.doc.descendants((node, pos) => {
    const at = node.isText ? (node.text ?? "").indexOf(word) : -1;
    if (at !== -1 && end === -1) end = pos + at + word.length;
    return true;
  });
  if (end === -1) throw new Error(`the document has no "${word}"`);
  return end;
}

test("a retype and a removal over a selection with an inline image leave no record mark on the image", async () => {
  // The bar's `addMark` marks an inline image inside the selection as well as the text.
  await withMarksEditor("The quick ![pic](/p.png) brown fox", ({ view }) => {
    const start = comment(view, "quick \n brown", BY, "", { from: 5, to: wordEnd(view, "brown") });
    expect(recordMarksOnInline(view)).toContain(`image:proofComment#${start.id}`);
    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(recordMarksOnInline(view).filter((mark) => mark.includes(start.id))).toEqual([]);
    expect(recordMarksOnInline(view)).toContain(`image:dispatchAsk#${ask.markId}`);
    removeRecordMark(view, ask.markId);
    expect(recordMarksOnInline(view)).toEqual([]);
  });
});

test("a retype keeps a mark that starts on an inline image whole", async () => {
  await withMarksEditor("The ![pic](/p.png) brown fox", ({ view }) => {
    const image = imagePosition(view);
    const brownEnd = wordEnd(view, "brown");
    const start = comment(view, "\n brown", BY, "", { from: image, to: brownEnd });
    expect(findRecordMark(view.state.doc, start.id)).toEqual({
      range: { from: image, to: brownEnd },
      type: "proofComment",
    });
    const ask = retypeMark(view, start.id, "ask", BY);
    if ("refused" in ask) throw new Error(`the ask retype was refused: ${ask.refused}`);
    expect(recordMarksOnInline(view)).toEqual([
      `image:dispatchAsk#${ask.markId}`,
      `text:dispatchAsk#${ask.markId}`,
    ]);
  });
});

test("retypeMark refuses a kind whose mark another record holds on an inline image in the range", async () => {
  await withMarksEditor("The quick ![pic](/p.png) brown fox", ({ view }) => {
    // Bob's recorded comment covers only the image; a comment of Alice's over "quick [image]
    // brown" would cut it off the image.
    const image = imagePosition(view);
    const bob = comment(view, "\n", "bob", "", { from: image, to: image + 1 });
    const alice = createAskMark(view, { from: 5, to: wordEnd(view, "brown") }, BY);
    if (alice === null) throw new Error("the ask mark was not written");
    expect(retypeMark(view, alice.id, "comment", BY)).toEqual({ refused: "overlaps" });
    expect(recordMarksOnInline(view)).toContain(`image:proofComment#${bob.id}`);
  });
});

test("retypeMark refuses a mark the document holds only on an inline image", async () => {
  // The server anchors a record to text alone (`pmdoc.MarkSpans` skips every node that is not
  // text), so a mark left only on an image is gone: a retype there would write a mark on the
  // image that no send can verify.
  await withMarksEditor("The quick ![pic](/p.png) brown fox", ({ view }) => {
    const image = imagePosition(view);
    const start = comment(view, "quick \n brown", BY, "", { from: 5, to: wordEnd(view, "brown") });
    // Another reader deletes the marked text on both sides of the image: " brown", then "quick ".
    view.dispatch(view.state.tr.delete(image + 1, wordEnd(view, "brown")));
    view.dispatch(view.state.tr.delete(5, image));
    expect(recordMarksOnInline(view)).toEqual([`image:proofComment#${start.id}`]);
    expect(findRecordMark(view.state.doc, start.id)).toBeNull();
    expect(retypeMark(view, start.id, "ask", BY)).toEqual({ refused: "missing" });
    expect(recordMarksOnInline(view)).toEqual([`image:proofComment#${start.id}`]);
    // The composer's cancel still takes the mark off the image.
    expect(removeRecordMark(view, start.id)).toBe(true);
    expect(recordMarksOnInline(view)).toEqual([]);
  });
});
