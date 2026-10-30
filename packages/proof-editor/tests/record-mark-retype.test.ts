import { expect, test } from "bun:test";
import { EditorState } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { comment, getMarks, marksPluginKey } from "proof-sdk-upstream/src/editor/plugins/marks";
import { createAskMark } from "../src/dispatch-marks";
import {
  findRecordMark,
  placementOf,
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
      placement: { blockId: expect.any(String), reading: 0 },
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

/** Another writer taking the record mark `markId` off the text it covers while leaving that text
 *  as it is - the mark is gone, its words are not - which no `removeRecordMark` tells the
 *  tracking about. */
function dropMark(view: EditorView, markId: string): void {
  const held = findRecordMark(view.state.doc, markId);
  if (held === null) throw new Error(`the document does not hold ${markId}`);
  const type = view.state.schema.marks[held.type];
  if (type === undefined) throw new Error(`no mark type ${held.type}`);
  view.dispatch(view.state.tr.removeMark(held.range.from, held.range.to, type));
}

/** The editor rebuilt around the document it holds: a fresh `EditorState` over the same doc,
 *  where every plugin starts again and the tracking remembers no mark at all. The browser meets
 *  this - y-prosemirror applies a remote update by rebuilding the document rather than through a
 *  mapping, and a reconnect rebuilds the editor - so the quote and the placement the composer
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

test("remarkRecordMark keeps to the tracked range when the mark is gone, even where the same words read elsewhere", async () => {
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    // Transactions that leave the mark alone: the reader types ahead of it, and the tracked range
    // follows that text rather than staying where it was written.
    view.dispatch(view.state.tr.insertText("Note: ", 1));
    const moved = quoteRange(`Note: ${PARAGRAPH}`);
    expect(findRecordMark(view.state.doc, start.id)).toEqual({
      range: moved,
      type: "proofComment",
    });
    dropMark(view, start.id);
    expect(findRecordMark(view.state.doc, start.id)).toBeNull();

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(again.quote).toBe(QUOTE);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...moved }),
    ]);
  });
  await withMarksEditor(`${PARAGRAPH}\n\nAnd again: ${QUOTE}.`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    dropMark(view, start.id);

    // The quote reads twice in the document, so the search by quote alone would refuse; the
    // tracked range is the reader's own passage, and it is asked first.
    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({ type: "proofComment", id: again.markId, ...quoteRange() }),
    ]);
  });
});

test("remarkRecordMark puts the mark back on the remembered reading, in a block that still holds both", async () => {
  await withMarksEditor(TWICE, ({ view }) => {
    const start = comment(view, REPEATED, BY, "", SECOND_RANGE);
    const placement = placementFor(view, SECOND_RANGE, REPEATED);
    expect(placement).toEqual({ blockId: expect.any(String), reading: 1 });
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

test("remarkRecordMark falls to the block's last reading where it now holds fewer, and refuses where it holds none", async () => {
  const kept = "The vet looks at the lazy dog again.";
  await withMarksEditor(TWICE, ({ view }) => {
    const start = comment(view, REPEATED, BY, "", SECOND_RANGE);
    const placement = placementFor(view, SECOND_RANGE, REPEATED);
    // Another writer cut the sentence the reader's own reading was the second of. The block
    // reads those words once now, and that one is the nearest thing to the reader's.
    rewriteParagraph(view, kept);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    const only = kept.indexOf(REPEATED) + 1;
    expect(again.placement).toEqual({ blockId: placement.blockId, reading: 0 });
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        from: only,
        to: only + REPEATED.length,
      }),
    ]);
  });
  await withMarksEditor(`${TWICE}\n\nAnd again: ${REPEATED}.`, ({ view }) => {
    const start = comment(view, REPEATED, BY, "", SECOND_RANGE);
    const placement = placementFor(view, SECOND_RANGE, REPEATED);
    rewriteParagraph(view, "The swift red fox naps.");
    rebuildEditorState(view);

    // The reader's own block reads those words nowhere now. Another block still does, and it is
    // not the passage they selected, so nothing is marked - and they were not deleted either.
    expect(remarkRecordMark(view, start.id, "comment", BY, REPEATED, placement)).toEqual({
      refused: "unplaceable",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark takes the one place the quote reads once the remembered block is gone, and refuses two", async () => {
  const after = `And again: ${QUOTE}.`;
  await withMarksEditor(`${PARAGRAPH}\n\n${after}`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    // Another writer deleted the whole paragraph the composer was opened in: the block the
    // placement names is gone, and the words read in one other place.
    dropParagraph(view);
    rebuildEditorState(view);

    const again = remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement);
    if ("refused" in again) throw new Error(`the re-mark was refused: ${again.refused}`);
    const only = after.indexOf(QUOTE) + 1;
    expect(marksInDoc(view)).toEqual([
      expect.objectContaining({
        type: "proofComment",
        id: again.markId,
        from: only,
        to: only + QUOTE.length,
      }),
    ]);
  });
  await withMarksEditor(`${PARAGRAPH}\n\n${after}\n\nOnce more: ${QUOTE}.`, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    const placement = placementFor(view, quoteRange(), QUOTE);
    dropParagraph(view);
    rebuildEditorState(view);

    // Two readings left and nothing to tell them apart: a record on the passage the reader never
    // selected is worse than one they can attach again themselves.
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, placement)).toEqual({
      refused: "unplaceable",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
});

test("remarkRecordMark marks the one place the quote reads in the document when nothing was remembered", async () => {
  await withMarksEditor(PARAGRAPH, ({ view }) => {
    const start = comment(view, QUOTE, BY, "", quoteRange());
    // The mark is gone, the editor was rebuilt around the words it covered, and the composer
    // kept no placement - a quote that runs across two blocks has none. The quote the composer
    // carries reads in exactly one place.
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

    // Two readings of those words and nothing left to tell them apart. They are still on the
    // page, so the refusal is not that they were changed or deleted.
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, null)).toEqual({
      refused: "unplaceable",
    });
    expect(marksInDoc(view)).toEqual([]);
  });
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
    // A second reading of the same words in the block, with nothing remembered about the
    // reader's own: marking either one could put the record on the passage they never selected.
    rewriteParagraph(view, `${QUOTE}, and ${QUOTE}.`);
    expect(remarkRecordMark(view, start.id, "comment", BY, QUOTE, null)).toEqual({
      refused: "unplaceable",
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
