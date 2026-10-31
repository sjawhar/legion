import { expect, test } from "bun:test";
import { EditorState } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { comment, getMarks, marksPluginKey } from "proof-sdk-upstream/src/editor/plugins/marks";
import { createAskMark } from "../src/dispatch-marks";
import {
  findRecordMark,
  placementOf,
  QUOTE_CONTEXT_CHARS,
  RECORD_MARK_TYPES,
  remarkRecordMark,
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

/** The attributes of the first span of the record mark `markId`. */
function markAttrs(view: EditorView, markId: string): Record<string, unknown> | undefined {
  let attrs: Record<string, unknown> | undefined;
  view.state.doc.descendants((node) => {
    if (!node.isInline) return true;
    for (const mark of node.marks) {
      if (
        attrs === undefined &&
        RECORD_MARK_TYPES.has(mark.type.name) &&
        mark.attrs.id === markId
      ) {
        attrs = mark.attrs;
      }
    }
    return true;
  });
  return attrs;
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
      placement: { blockId: expect.any(String), prefix: "The ", readings: 1, suffix: " fox" },
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

test("a retype onto text another record's mark of the new kind covers goes through and leaves that mark whole", async () => {
  await withMarksEditor(SENTENCE, ({ view }) => {
    // Bob's recorded comment covers "quick brown"; Alice asks about "brown" inside it, then
    // switches her ask to a comment, a suggestion and back to an ask.
    const brown = { from: 11, to: 16 };
    const bob = comment(view, "quick brown", "bob", "", RANGE);
    const alice = createAskMark(view, brown, BY);
    if (alice === null) throw new Error("the ask mark was not written");
    const bobsSpans = [
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
    ];
    const bobs = () => marksInDoc(view).filter((mark) => mark.id === bob.id);
    expect(bobs()).toEqual(bobsSpans);

    // Alice's comment over "brown" sits inside Bob's rather than cutting "brown" out of it.
    const asComment = retypeMark(view, alice.id, "comment", BY);
    if ("refused" in asComment) {
      throw new Error(`the comment retype was refused: ${asComment.refused}`);
    }
    expect(asComment.quote).toBe("brown");
    expect(findRecordMark(view.state.doc, asComment.markId)).toEqual({
      range: brown,
      type: "proofComment",
    });
    expect(bobs()).toEqual(bobsSpans);

    const suggest = retypeMark(view, asComment.markId, "suggest", BY);
    if ("refused" in suggest) throw new Error(`the suggest retype was refused: ${suggest.refused}`);
    expect(bobs()).toEqual(bobsSpans);
    const back = retypeMark(view, suggest.markId, "ask", BY);
    if ("refused" in back) throw new Error(`the ask retype was refused: ${back.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 5, to: 11 }),
      expect.objectContaining({ type: "proofComment", id: bob.id, from: 11, to: 16 }),
      expect.objectContaining({ type: "dispatchAsk", id: back.markId, from: 11, to: 16 }),
    ]);
    expect(findRecordMark(view.state.doc, bob.id)).toEqual({ range: RANGE, type: "proofComment" });
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

test("a retype over an inline image another record's mark of the new kind holds leaves that mark on the image", async () => {
  await withMarksEditor("The quick ![pic](/p.png) brown fox", ({ view }) => {
    // Bob's recorded comment covers only the image; Alice's ask over "quick [image] brown"
    // becomes a comment over the image beside his.
    const image = imagePosition(view);
    const bob = comment(view, "\n", "bob", "", { from: image, to: image + 1 });
    const alice = createAskMark(view, { from: 5, to: wordEnd(view, "brown") }, BY);
    if (alice === null) throw new Error("the ask mark was not written");
    const asComment = retypeMark(view, alice.id, "comment", BY);
    if ("refused" in asComment) {
      throw new Error(`the comment retype was refused: ${asComment.refused}`);
    }
    expect(recordMarksOnInline(view)).toContain(`image:proofComment#${bob.id}`);
    expect(recordMarksOnInline(view)).toContain(`image:proofComment#${asComment.markId}`);
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

/** The sentence the margin's recovery is measured on, and the words a composer is open over. */
const PARAGRAPH = "The quick brown fox jumps over the lazy dog.";
const QUOTE = "jumps over the lazy dog";

/** Where `QUOTE` sits in a document of `PARAGRAPH` alone: the paragraph opens at 0, text at 1. */
function quoteRange(text = PARAGRAPH): { from: number; to: number } {
  const at = text.indexOf(QUOTE) + 1;
  return { from: at, to: at + QUOTE.length };
}

/** Another writer rewriting the whole sentence, as a collaborator's update reaches the document:
 *  the text it leaves carries no mark, so every record mark over it is gone. */
function rewriteParagraph(view: EditorView, text: string): void {
  const paragraph = view.state.doc.child(0);
  view.dispatch(
    view.state.tr.replaceWith(1, 1 + paragraph.content.size, view.state.schema.text(text))
  );
}

/** Another writer deleting the whole paragraph the composer was opened in: the block that held
 *  the mark leaves the document, taking its id with it. */
function dropParagraph(view: EditorView): void {
  view.dispatch(view.state.tr.delete(0, view.state.doc.child(0).nodeSize));
}

/** The record mark `markId` taken off the text it covers while that text stays as it is - the
 *  mark is gone, its words are not. Every row that uses it rebuilds the state afterwards
 *  (`rebuildEditorState`), because that is how the mark's loss reaches this editor in
 *  production: as a whole new document, from the sweep or from another writer. */
function dropMark(view: EditorView, markId: string): void {
  const held = findRecordMark(view.state.doc, markId);
  if (held === null) throw new Error(`the document does not hold ${markId}`);
  const type = view.state.schema.marks[held.type];
  if (type === undefined) throw new Error(`no mark type ${held.type}`);
  view.dispatch(view.state.tr.removeMark(held.range.from, held.range.to, type));
}

/** The editor rebuilt around the document it holds: a fresh `EditorState` over the same doc,
 *  where every plugin starts again. The browser meets this - y-prosemirror applies a remote
 *  update as one replacement of the whole document rather than through a mapping a local step
 *  would give, and a reconnect rebuilds the editor - so the quote and the placement the composer
 *  kept are all the recovery has left to find the words by. */
function rebuildEditorState(view: EditorView): void {
  view.updateState(EditorState.create({ doc: view.state.doc, plugins: view.state.plugins }));
}

/** The placement the selection bar reports for the mark it writes over `range`, which is what a
 *  composer keeps beside its quote; the test's marks are written by the creators directly, so it
 *  asks for it the way the bar does. */
function placementFor(view: EditorView, range: { from: number; to: number }, quote: string) {
  const placement = placementOf(view.state.doc, range, quote);
  if (placement === null) throw new Error(`no placement for: ${quote}`);
  return placement;
}

/** A paragraph whose quoted words read twice, as a phrase a reader picks out of a specification
 *  often does, and the second reading of them: the one the rows below select. The paragraph
 *  opens at 0 and its text at 1, so a text offset is one less than the position. */
const TWICE = "The quick brown fox jumps over the lazy dog. The vet looks at the lazy dog again.";
const REPEATED = "the lazy dog";
const SECOND = TWICE.indexOf(REPEATED, TWICE.indexOf(REPEATED) + 1) + 1;
const SECOND_RANGE = { from: SECOND, to: SECOND + REPEATED.length };

test("remarkRecordMark re-marks the text the document still holds the mark on, under a fresh id", async () => {
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const range = quoteRange();
    const start = comment(view, QUOTE, BY, "", range);
    const placement = placementFor(view, range, QUOTE);

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    // The kind did not change and the id did: the server refused the mark it was sent, so the
    // retry carries one it has never judged.
    expect(again.markId).not.toBe(start.id);
    expect(again.quote).toBe(QUOTE);
    // The mark it wrote is placed as the bar's own was, so a second recovery is as well placed.
    expect(again.placement).toEqual(placement);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...range }),
    ]);

    const third = remarkRecordMark(view, again.markId, "comment", BY, QUOTE, again.placement);
    if ("refused" in third) throw new Error(`the re-mark was refused: ${third.refused}`);
    expect(third.markId).not.toBe(again.markId);
  });
});

test("remarkRecordMark puts the mark back on the reading its surroundings name, in a block that still holds both", async () => {
  await withMarksEditor(TWICE, ({ view }) => {
    const start = comment(view, REPEATED, BY, "", SECOND_RANGE);
    const placement = placementFor(view, SECOND_RANGE, REPEATED);
    expect(placement).toEqual({
      blockId: expect.any(String),
      prefix: TWICE.slice(SECOND - 1 - QUOTE_CONTEXT_CHARS, SECOND - 1),
      readings: 2,
      suffix: " again.",
    });
    // The tab was away past the sweep and came back: the mark is gone, the reconnect rebuilt the
    // editor, and the document's text never changed. Only the composer's own copy is left.
    dropMark(view, start.id);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(again.placement).toEqual(placement);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...SECOND_RANGE }),
    ]);
  });
});

/** The paragraph the defect was found in, and what another writer left behind: the reader's words
 *  read twice to begin with, and three times once a reading of them stands in front of both. The
 *  reading the reader selected is the paragraph's second and then its third, so an index of it is
 *  a different passage afterwards and the text around it is not
 *  (`packages/dispatch/e2e/compose-wait.e2e.ts`). */
const TWO_READINGS = "Alpha the lazy dog. Beta the lazy dog.";
const THREE_READINGS = "Zero the lazy dog. Alpha the lazy dog. Beta the lazy dog.";

/** Where the reader's own words sit in `text`, which reads them last. */
function lastReading(text: string): { from: number; to: number } {
  const at = text.lastIndexOf(REPEATED) + 1;
  return { from: at, to: at + REPEATED.length };
}

test("remarkRecordMark keeps the reader's own reading when another writer adds one in front of it in the same block", async () => {
  await withMarksEditor(TWO_READINGS, ({ view }) => {
    const range = lastReading(TWO_READINGS);
    const start = comment(view, REPEATED, BY, "", range);
    const placement = placementFor(view, range, REPEATED);
    // Another writer's reading of the same words, in front of the reader's and inside their own
    // block, written in the edit that took the mark: their words are the paragraph's third
    // reading now, where they were its second.
    rewriteParagraph(view, THREE_READINGS);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        ...lastReading(THREE_READINGS),
      }),
    ]);
  });
});

test("the surroundings the cap keeps tell those readings apart where a shorter prefix would not", async () => {
  await withMarksEditor(TWO_READINGS, ({ view }) => {
    const range = lastReading(TWO_READINGS);
    const start = comment(view, REPEATED, BY, "", range);
    const placement = placementFor(view, range, REPEATED);
    // The whole text before the words, the paragraph beginning nearer than the cap.
    expect(placement.prefix).toBe("Alpha the lazy dog. Beta ");
    expect(placement.prefix.length).toBeLessThan(QUOTE_CONTEXT_CHARS);
    rewriteParagraph(view, THREE_READINGS);
    rebuildEditorState(view);

    // Two characters of that prefix - all a cap of two would have kept - read in front of
    // "Alpha" as much as in front of "Beta", so two readings of the reader's own block answer
    // them and neither can be taken for theirs: the block step's "ambiguous", driven by a
    // placement whose surroundings really do answer both.
    const shorter = { ...placement, prefix: placement.prefix.slice(-2) };
    expect(remarkRecordMark(view, start.id, "comment", BY, REPEATED, shorter)).toEqual({
      refused: "ambiguous",
    });
    expect(marksInDoc(view)).toEqual([]);

    const again = remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        ...lastReading(THREE_READINGS),
      }),
    ]);
  });
});

/** A line a writer repeated, which reads the quoted words six times with the same words on both
 *  sides of each. Two of those readings - the third and the fourth - carry the cap's worth of
 *  text on each side, so their surroundings are the same text and nothing tells them apart; the
 *  readings nearer the paragraph's ends have less text on one side than the cap keeps. */
const REPEATED_LINE = "Ask the operator. ".repeat(6).trim();
const OPERATOR = "the operator";

test("remarkRecordMark refuses two readings of a block whose surroundings both match", async () => {
  await withMarksEditor(REPEATED_LINE, ({ view }) => {
    const at = REPEATED_LINE.indexOf(OPERATOR, REPEATED_LINE.indexOf(OPERATOR) + 1) + 1;
    const third = REPEATED_LINE.indexOf(OPERATOR, at) + 1;
    const range = { from: third, to: third + OPERATOR.length };
    const start = comment(view, OPERATOR, BY, "", range);
    const placement = placementFor(view, range, OPERATOR);
    expect(placement.prefix).toHaveLength(QUOTE_CONTEXT_CHARS);
    expect(placement.suffix).toHaveLength(QUOTE_CONTEXT_CHARS);
    dropMark(view, start.id);
    rebuildEditorState(view);

    // The reading after the reader's is surrounded by the same text to the cap's depth on both
    // sides. Marking either of them could put the record on the passage they never selected -
    // and their words are exactly where they selected them, so the refusal is the block step's
    // "ambiguous", for two readings of their own block nothing tells apart, never the
    // "unplaceable" that tells a reader their words are no longer where they left them.
    expect(remarkRecordMark(view, start.id, "comment", BY, OPERATOR, placement)).toEqual({
      refused: "ambiguous",
    });
    expect(marksInDoc(view)).toEqual([]);

    // With the last of those lines gone, the twin has less text after it than the cap kept, so
    // one reading answers the surroundings and it is marked: the refusal above was two readings
    // answering them, not none.
    rewriteParagraph(view, "Ask the operator. ".repeat(5).trim());
    rebuildEditorState(view);
    const again = remarkRecordMark(view, start.id, "comment", BY, OPERATOR, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        from: third,
        to: third + OPERATOR.length,
      }),
    ]);
  });
});

test("remarkRecordMark marks the one reading of a block that held the quote once, however its sentence was rewritten", async () => {
  const rewritten = `The swift red fox ${QUOTE}.`;
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    expect(placement.readings).toBe(1);
    // Another writer rewrote the words in front of the reader's, which took the mark with them.
    // The block read those words once when the reader selected them and reads them once now, so
    // there was never another reading there to confuse this one with: it is their passage.
    rewriteParagraph(view, rewritten);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(again.placement).toEqual({
      blockId: placement.blockId,
      prefix: "The swift red fox ",
      readings: 1,
      suffix: ".",
    });
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        ...quoteRange(rewritten),
      }),
    ]);
  });
});

test("remarkRecordMark refuses the reading left in a block that held the quote twice when the reader's own words are deleted", async () => {
  const kept = "Alpha the lazy dog.";
  await withMarksEditor(TWO_READINGS, ({ view }) => {
    const range = lastReading(TWO_READINGS);
    const start = comment(view, REPEATED, BY, "", range);
    const placement = placementFor(view, range, REPEATED);
    expect(placement.readings).toBe(2);
    // Another writer deleted the sentence the reader's own reading was in. The block reads those
    // words once now, in the sentence the reader never selected, and nothing in the text around
    // it says it was theirs: a comment put there is a record on another passage.
    rewriteParagraph(view, kept);
    rebuildEditorState(view);

    expect(remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement)).toEqual({
      refused: "unsurrounded",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark refuses a block whose readings are all surrounded by words the reader never read", async () => {
  await withMarksEditor(TWO_READINGS, ({ view }) => {
    const range = lastReading(TWO_READINGS);
    const start = comment(view, REPEATED, BY, "", range);
    const placement = placementFor(view, range, REPEATED);
    // Another writer edited the word in front of the reader's own reading. Their words are
    // exactly where they were - it is the text around them that changed - so neither reading
    // answers the surroundings, and the block holding two of them says nothing about which.
    rewriteParagraph(view, "Alpha the lazy dog. Bravo the lazy dog.");
    rebuildEditorState(view);

    expect(remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement)).toEqual({
      refused: "unsurrounded",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark refuses when the reader's own block reads the quote nowhere and another block does", async () => {
  await withMarksEditor(`${TWICE}\n\nAnd again: ${REPEATED}.`, ({ view }) => {
    const start = comment(view, REPEATED, BY, "", SECOND_RANGE);
    const placement = placementFor(view, SECOND_RANGE, REPEATED);
    rewriteParagraph(view, "The swift red fox naps.");
    rebuildEditorState(view);

    // The reader's own block reads those words nowhere now. Another block still does, among
    // words the reader never read beside them, so it is a different passage: nothing is marked -
    // and the words were not deleted either.
    expect(remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement)).toEqual({
      refused: "unplaceable",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark takes the reading the whole document surrounds as the reader's once their block is gone, calls a reading among other words unplaceable, and two readings alike ambiguous", async () => {
  await withMarksEditor(`${PARAGRAPH}\n\n${PARAGRAPH}`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    // Another writer deleted the whole paragraph the composer was opened in: the block the
    // placement names is gone, and the one reading left reads among the reader's own words.
    dropParagraph(view);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...quoteRange() }),
    ]);
  });
  await withMarksEditor(`${PARAGRAPH}\n\nAnd again: ${QUOTE}.`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    dropParagraph(view);
    rebuildEditorState(view);

    // The words read once in the document still, among other words: a sentence of someone
    // else's that happens to quote them is not the passage the reader selected.
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement)).toEqual({
      refused: "unplaceable",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
  await withMarksEditor(`${PARAGRAPH}\n\n${PARAGRAPH}\n\n${PARAGRAPH}`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    dropParagraph(view);
    rebuildEditorState(view);

    // Two readings left, each surrounded as the placement remembers and nothing to tell them
    // apart: a record on the passage the reader never selected is worse than one they can
    // attach again, and their words never moved. This is the document-wide step's "ambiguous" -
    // the surroundings the composer kept really do answer both readings - never the refusal for
    // a selection nothing was kept of.
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement)).toEqual({
      refused: "ambiguous",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark marks the one place the quote reads in the document when nothing was remembered", async () => {
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    // The mark is gone, the editor was rebuilt around the words it covered, and the composer
    // kept no placement - a selection no one textblock's text reads as the quote has none. The
    // quote the composer carries reads in exactly one place.
    dropMark(view, start.id);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, null);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(again.quote).toBe(QUOTE);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...quoteRange() }),
    ]);
  });
  await withMarksEditor(`${PARAGRAPH}\n\nAnd again: ${QUOTE}.`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    dropMark(view, start.id);
    rebuildEditorState(view);

    // Two readings of those words and nothing kept of the reader's selection to tell them
    // apart: with no placement there are no surroundings to answer, and the text around the two
    // readings is not the same text, so this is the document-wide step's "unremembered" rather
    // than the "ambiguous" that says they read alike. The words are still on the page and still
    // where they were, so neither "changed or deleted" nor "moved" is true of them.
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, null)).toEqual({
      refused: "unremembered",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

/** Two paragraphs, and the words a reader selected across the break between them: the selection
 *  bar reads such a selection as the first block's tail, a newline for the break and the second
 *  block's head (`textBetween(range.from, range.to, "\n", "\n")` in `runAction`), and no one
 *  block's text reads it, so the composer keeps no placement for it. */
const FIRST_BLOCK = "The quick brown fox jumps over the lazy dog.";
const SECOND_BLOCK = "The vet looks at the lazy dog again.";
const ACROSS_BLOCKS = "lazy dog.\nThe vet looks";

/** Where `ACROSS_BLOCKS` sits in a document that opens with those two paragraphs: the first
 *  opens at 0 and its text at 1, and the second's text opens two positions past the end of the
 *  first's - the first's close and the second's open. */
function acrossBlocksRange(): { from: number; to: number } {
  const [head, tail] = ACROSS_BLOCKS.split("\n");
  return {
    from: 1 + FIRST_BLOCK.indexOf(head ?? ""),
    to: FIRST_BLOCK.length + 3 + (tail ?? "").length,
  };
}

test("remarkRecordMark puts the mark back over a quote that runs across two blocks", async () => {
  await withMarksEditor(`${FIRST_BLOCK}\n\n${SECOND_BLOCK}`, ({ view }) => {
    const range = acrossBlocksRange();
    expect(view.state.doc.textBetween(range.from, range.to, "\n", "\n")).toBe(ACROSS_BLOCKS);
    const start = comment(view, ACROSS_BLOCKS, BY, "", range);
    // No one block's text reads those words, so the bar reports no placement for them and the
    // quote is the whole of what the composer keeps.
    expect(placementOf(view.state.doc, range, ACROSS_BLOCKS)).toBeNull();
    // The sweep took the mark while the tab was away, and the document came back as a whole new
    // one: the words are where they were, and the editor remembers nothing about the mark.
    dropMark(view, start.id);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, ACROSS_BLOCKS, null);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(again.quote).toBe(ACROSS_BLOCKS);
    expect(again.placement).toBeNull();
    // The fresh mark covers the same words, a span in each block, as the bar's own did.
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        from: range.from,
        to: FIRST_BLOCK.length + 1,
      }),
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        from: FIRST_BLOCK.length + 3,
        to: range.to,
      }),
    ]);
  });
  await withMarksEditor(
    `${FIRST_BLOCK}\n\n${SECOND_BLOCK}\n\n${FIRST_BLOCK}\n\n${SECOND_BLOCK}`,
    ({ view }) => {
      const range = acrossBlocksRange();
      const start = comment(view, ACROSS_BLOCKS, BY, "", range);
      dropMark(view, start.id);
      rebuildEditorState(view);

      // The same words run across a second break further down: two readings, and a selection
      // across a break is one no placement can describe, so nothing kept of it says which
      // reading was theirs. The document-wide step answers "unremembered" - they are plainly on
      // the page and exactly where the reader selected them, so it never says they moved.
      expect(remarkRecordMark(view, start.id, "comment", BY, ACROSS_BLOCKS, null)).toEqual({
        refused: "unremembered",
      });
      expect(marksInDoc(view)).toEqual([]);
    }
  );
});

test("remarkRecordMark refuses when the words are gone, when the block reads them twice, and for a quote the document never held", async () => {
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    rewriteParagraph(view, "The swift red fox naps.");
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement)).toEqual({
      refused: "gone",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    // A second reading of the same words in the block, with nothing kept of where the reader
    // selected: marking either one could put the record on the passage they never selected, and
    // the words around the two readings differ, so the document-wide step answers
    // "unremembered" rather than "ambiguous".
    rewriteParagraph(view, `${QUOTE}, and ${QUOTE}.`);
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, null)).toEqual({
      refused: "unremembered",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    // The composer ended and the sentence went with it: nothing tracks the mark, and the words
    // its quote names read nowhere in the document - not even for an id no mark ever had.
    removeRecordMark(view, start.id);
    rewriteParagraph(view, "The swift red fox naps.");
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement)).toEqual({
      refused: "gone",
    });
    expect(remarkRecordMark(view, "never", "comment", BY, QUOTE, placement)).toEqual({
      refused: "gone",
    });
  });
});

test("remarkRecordMark writes the mark each composer kind anchors to", async () => {
  for (const kind of ["comment", "suggest", "ask"] as const) {
    await withMarksEditor(PARAGRAPH, ({ view }) => {
      const start = comment(view, QUOTE, BY, "", quoteRange());
      const placement = placementFor(view, quoteRange(), QUOTE);
      rewriteParagraph(view, `The swift red fox ${QUOTE}.`);
      const again = remarkRecordMark(view, start.id, kind, BY, QUOTE, placement);
      if ("refused" in again) throw new Error(`the ${kind} re-mark was refused: ${again.refused}`);
      const range = quoteRange(`The swift red fox ${QUOTE}.`);
      expect(marksInDoc(view)).toEqual([
        expect.objectContaining({
          type:
            kind === "comment"
              ? "proofComment"
              : kind === "suggest"
                ? "proofSuggestion"
                : "dispatchAsk",
          id: again.markId,
          ...range,
          ...(kind === "suggest" ? { kind: "replace" } : {}),
        }),
      ]);
      expect(markAttrs(view, again.markId)).toMatchObject({ by: BY, id: again.markId });
    });
  }
});
