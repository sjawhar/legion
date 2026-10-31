/**
 * The margin composer's provisional record mark: retyping it with the Comment / Suggest / Ask
 * switch, and putting it back over its words once the document has lost it.
 *
 * The selection bar (./dispatch-action-bar.ts `runAction`) writes a mark of the pressed kind and
 * the server verifies that a record's `mark_id` names a mark of the record's own kind
 * (`docs.VerifyMark`). When the reader switches the composer's kind, the mark has to follow:
 * `retypeMark` writes a fresh mark of the new kind over the same text with the three creators
 * the bar uses - which mint the new mark's id, so the id changes with the kind, as the server's
 * bookkeeping expects (it keys a mark by type and id) - then removes the old one.
 *
 * Neither the new mark nor the removal is an undo step: ./record-mark-history.ts keeps the
 * composer's own record-mark writes out of undo history, so Ctrl/Cmd+Z after a switch never puts
 * the old mark back and redo never writes it again. The creation takes nothing from another
 * record: the three record marks declare `excludes: ''`, so a mark of the new kind over text that
 * another record's mark of that kind covers sits beside that mark rather than cutting it
 * (LEGION-458).
 *
 * A mark the document has lost is `remarkRecordMark`'s case: the reader held the composer open
 * while another writer rewrote the sentence, or their tab was away past the provisional mark's
 * time, so the mark is gone and the send the server refuses (`409 ANCHOR_MISSING`) has nothing to
 * anchor to. The margin hands back what the composer kept of the selection: the quote its own
 * request carries, and the placement the bar reported for it (`RecordMarkPlacement` - the id of
 * the textblock those words sat in, the text the editor read on each side of them, and how many
 * readings of the quote that block held then). Quote and surroundings together are the ordinary
 * three-part text-quote selector, and a reading of the quote is the reader's own only where the
 * text before it ends with the remembered prefix and the text after it starts with the
 * remembered suffix.
 *
 * The fresh mark goes over the first of these that holds: the mark's own range, while the
 * document still has it; inside the remembered block, while the document still holds it and it
 * still reads the quote, the one reading there whose surroundings match, or its only reading of
 * the quote when none of them match and the block held the quote once when the reader selected
 * it (a block that read their words once then and reads them once now is their passage however
 * the sentence around them was rewritten, there having been no other reading there to confuse it
 * with, while a block that read them twice may have been left the reading they never selected);
 * and otherwise, the block being gone or reading the quote nowhere, the one reading in the whole
 * document whose surroundings match. The document is read as one text, its blocks joined by a
 * newline for each break, exactly as the selection bar reads a quote out of it, so the words of
 * a selection that ran across a block break - which no placement can describe, no one block's
 * text reading them - are found where they read now. Nothing is taken for the reader's passage
 * on the strength of the words alone: another paragraph that reads them among different words is
 * a different passage.
 *
 * Only the first step asks this editor's state for anything, and nothing of it survives another
 * writer's change: y-prosemirror applies a remote update as one replacement of the whole
 * document, which collapses every range mapped through it, and a reconnect rebuilds the editor,
 * taking every plugin's state with it. The placement is what carries the reader's own passage
 * through that, because the composer keeps it outside the editor and the editor resolves it by
 * reading the document alone - no browser-versus-server counting, and no index that another
 * writer's reading of the same words in front of the reader's silently moves. Where no reading
 * can be chosen - more than one answers the surroundings, or none does and the block held the
 * words more than once - the re-mark is refused rather than anchored to a passage the reader
 * never selected, which no refusal would ever tell them about. The refusal says which case it
 * was: the words read nowhere (`"gone"`), their own block still reads them but no reading there
 * is surrounded as theirs was (`"unsurrounded"`), two readings are surrounded alike - two of
 * their own block, or two of the document once that block is gone - so the words are where they
 * were selected and nothing says which of them the draft is about (`"ambiguous"`), the composer
 * kept nothing of where they selected and the words read in more than one place, so there is
 * nothing to tell those readings apart by (`"unremembered"`), or they read elsewhere in the
 * document and no reading of them is the reader's own to take (`"unplaceable"`), which the
 * margin puts to the reader in different words.
 *
 * `removeRecordMark` is the precise removal both this module and the handle's `removeMark` use:
 * span by span, by id, whatever the mark's type. Upstream's `deleteMark` is not it - it cannot
 * see a provisional comment (`buildAnchorMarks` drops a comment anchor whose body is empty, and
 * the bar creates comments with an empty body) - and nothing else is needed: once the spans are
 * gone, the marks plugin's own `normalizeMetadata` drops the metadata of every id no longer
 * anchored.
 */

import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkRange } from "proof-sdk-upstream/src/editor/plugins/marks";
import { comment, suggestReplace } from "proof-sdk-upstream/src/editor/plugins/marks";
import type { RecordMarkPlacement, SelectionBarActionKind } from "./dispatch-marks";
import { createAskMark } from "./dispatch-marks";
import { blockIdOf } from "./editor/schema/block-ids";

const MARK_TYPE_FOR_KIND: Record<SelectionBarActionKind, string> = {
  ask: "dispatchAsk",
  comment: "proofComment",
  suggest: "proofSuggestion",
};
/** The mark types a Dispatch record anchors to: the server's `docs.MarkKind` values. */
export const RECORD_MARK_TYPES: ReadonlySet<string> = new Set(Object.values(MARK_TYPE_FOR_KIND));

export interface RetypedMark {
  readonly markId: string;
  /** Where the mark sits, for the composer that owns it to keep beside the quote; null when
   *  there is nothing to keep (`placementOf`). */
  readonly placement: RecordMarkPlacement | null;
  readonly quote: string;
}

/** Why a retype changed nothing: the document no longer holds the mark on any text, or the new
 *  kind cannot be written over its text (a suggestion across table cells). */
export interface RetypeRefusal {
  readonly refused: "missing" | "unmarkable";
}

export type RetypeOutcome = RetypedMark | RetypeRefusal;

/** Why a re-mark changed nothing: the new kind cannot be written over the text, as a retype's
 *  `"unmarkable"` says; the words the mark was written over read nowhere in the document any
 *  more (`"gone"`); the block the reader selected in still reads them, but no reading there is
 *  surrounded as theirs was, so the text around their words changed and nothing says which
 *  reading was theirs (`"unsurrounded"`); two readings are surrounded as theirs was - two of
 *  their own block, or two of the document once that block is gone - so their words are where
 *  they selected them and nothing says which of the two the draft is about (`"ambiguous"`); the
 *  composer kept nothing of where they selected, so nothing but the words themselves is left to
 *  go by, and they read in more than one place (`"unremembered"`); or they read somewhere else
 *  in the document and no reading of them can be taken for the reader's own (`"unplaceable"`).
 *  None of the six is the same thing to say to a reader, so none of them is the same answer. A
 *  re-mark never answers `"missing"`: a mark the document has lost is the case it is for. */
export interface RemarkRefusal {
  readonly refused:
    | "ambiguous"
    | "gone"
    | "unmarkable"
    | "unplaceable"
    | "unremembered"
    | "unsurrounded";
}

export type RemarkOutcome = RetypedMark | RemarkRefusal;

/** The record mark `markId` as the document holds it: its type, and the range from the start of
 *  its first inline run to the end of its last. Inline atoms such as an image count toward the
 *  range: the bar's `addMark` marks them with the text. Null when no text node holds it: the
 *  server anchors a record to text alone (`pmdoc.MarkSpans`), so a mark left only on an atom is
 *  gone as far as a send can tell. */
export function findRecordMark(
  doc: ProseMirrorNode,
  markId: string
): { range: MarkRange; type: string } | null {
  let from = -1;
  let to = -1;
  let type = "";
  let sawText = false;
  doc.descendants((node, pos) => {
    if (!node.isInline) return true;
    const held = node.marks.find(
      (mark) => RECORD_MARK_TYPES.has(mark.type.name) && mark.attrs.id === markId
    );
    if (held === undefined) return true;
    if (from === -1) {
      from = pos;
      type = held.type.name;
    }
    to = pos + node.nodeSize;
    if (node.isText) sawText = true;
    return true;
  });
  return from === -1 || !sawText ? null : { range: { from, to }, type };
}

/** The meta key `removeRecordMark` sets on its transaction, and the value it sets there. Both are
 *  this module's own, so no other writer's meta can be read as this label by accident: ProseMirror
 *  files a meta under the key's derived string (`"recordMarkRemoval$"`), which another meta could
 *  share, so the label is this module's symbol rather than `true`. It guards against that
 *  collision, not against a forger - any writer can set `addToHistory: false` itself.
 *  ./record-mark-history.ts asks `isRecordMarkRemoval`, which reads it. */
const recordMarkRemoval = new PluginKey("recordMarkRemoval");
const composerRemoval = Symbol("composer's own record-mark removal");

/** Whether `transaction` is `removeRecordMark`'s removal of a provisional mark the composer
 *  owns, which ./record-mark-history.ts keeps out of undo history. */
export function isRecordMarkRemoval(transaction: Transaction): boolean {
  return transaction.getMeta(recordMarkRemoval) === composerRemoval;
}

/** Removes every span of the record mark `markId`, whatever its type, from text and from inline
 *  atoms such as an image alike. Answers whether a span was removed. The transaction is labelled
 *  as the composer's own removal, so it is not an undo step (./record-mark-history.ts). */
export function removeRecordMark(view: EditorView, markId: string): boolean {
  let transaction = view.state.tr.setMeta(recordMarkRemoval, composerRemoval);
  view.state.doc.descendants((node, pos) => {
    if (!node.isInline) return true;
    for (const mark of node.marks) {
      if (RECORD_MARK_TYPES.has(mark.type.name) && mark.attrs.id === markId) {
        transaction = transaction.removeMark(pos, pos + node.nodeSize, mark);
      }
    }
    return true;
  });
  if (!transaction.docChanged) return false;
  view.dispatch(transaction);
  return true;
}

/** The mark of `kind` the selection bar's creators write over `range`, by `by`, over the text
 *  `quote`, and the id they mint for it; null when upstream will not write that kind there - a
 *  suggestion across table cells, or one that starts or ends mid-word. The same creators, with
 *  the same arguments, as the bar's `runAction`: every record mark is written one way. */
function createRecordMark(
  view: EditorView,
  kind: SelectionBarActionKind,
  range: MarkRange,
  quote: string,
  by: string
): string | null {
  if (kind === "comment") return comment(view, quote, by, "", range).id;
  if (kind === "suggest") return suggestReplace(view, quote, by, "", range)?.id ?? null;
  return createAskMark(view, range, by)?.id ?? null;
}

/** Replaces the record mark `markId` with a fresh mark of `kind` over the same text, written by
 *  `by`, and answers the new mark - or the mark as it is, when it already has that kind - with
 *  the placement to remember for it, computed as the selection bar computes the one it reports,
 *  so a re-mark after a kind switch is as well placed as one after the bar's own mark. Refused,
 *  with nothing changed, when the document does not hold `markId` or when `kind` cannot be marked
 *  over its text. */
export function retypeMark(
  view: EditorView,
  markId: string,
  kind: SelectionBarActionKind,
  by: string
): RetypeOutcome {
  const held = findRecordMark(view.state.doc, markId);
  if (held === null) return { refused: "missing" };
  const quote = view.state.doc.textBetween(held.range.from, held.range.to, "\n", "\n");
  const placement = placementOf(view.state.doc, held.range, quote);
  if (held.type === MARK_TYPE_FOR_KIND[kind]) return { markId, placement, quote };
  const created = createRecordMark(view, kind, held.range, quote, by);
  if (created === null) return { refused: "unmarkable" };
  removeRecordMark(view, markId);
  return { markId: created, placement, quote };
}

/** How much of the text on each side of the selected words a placement keeps. Measured over this
 *  repository's own prose - every `.md` of the root, `docs/`, `packages/dispatch/docs/` and
 *  `packages/proof-editor/`, taking each run of three to eight words (16 to 80 characters) that
 *  reads more than once in one block for a reader's selection: 32 characters each side tell
 *  those 13,876 readings apart in 90.4% of cases, where 16 reach 81.1% and 64 reach 96.9% for
 *  twice the text on every compose and in the held-send store. The paragraph this was written
 *  for needs 3 ("ta " of "Beta " against "ha " of "Alpha " in front of the same words), and a
 *  cap that short only ever tells readings apart by cutting into the word before them, where a
 *  neighbour's last letters match by accident. A cap that separates nothing costs a refusal,
 *  never a record on the wrong passage. */
export const QUOTE_CONTEXT_CHARS = 32;

/** The text a reading has to sit between to be taken for the reader's own, as `placementOf` read
 *  it when they selected. */
interface Surroundings {
  readonly prefix: string;
  readonly suffix: string;
}

/** The one way to ask a scan for no filter: the empty surroundings, which the text on both sides
 *  of every reading answers. A re-mark whose composer kept no placement matches against these -
 *  the words are all there ever was in that case - and so does a count of every reading a block
 *  holds. */
const NO_SURROUNDINGS: Surroundings = { prefix: "", suffix: "" };

/** What one scan of a text found: every reading of the quote counted, the first of them, and the
 *  first two whose surroundings match. A third surrounded reading answers nothing any caller
 *  asks - one of them is the answer and two are the refusal - so none is kept. */
interface Readings {
  /** How many readings the text holds, whatever surrounds them. */
  readonly count: number;
  /** The first reading of the text, or undefined where it holds none. */
  readonly first: MarkRange | undefined;
  /** The first two readings the surroundings answer, in the order the text reads them. */
  readonly surrounded: readonly MarkRange[];
}

/** Whether the reading of a `length`-character quote at offset `at` of `text` is surrounded by
 *  `surroundings`: the text before it ends with the remembered prefix, and the text after it
 *  starts with the remembered suffix. Read in place, so scanning a text for its readings copies
 *  none of it. */
function surroundedBy(
  text: string,
  at: number,
  length: number,
  surroundings: Surroundings
): boolean {
  const prefixAt = at - surroundings.prefix.length;
  return (
    prefixAt >= 0 &&
    text.startsWith(surroundings.prefix, prefixAt) &&
    text.startsWith(surroundings.suffix, at + length)
  );
}

/** The readings of `quote` in `text`: the one scan this module makes over any text, so a
 *  placement's count of a block and a re-mark's choice between that block's readings can never
 *  disagree about what reads there. `positionAt` says where the character at an offset of `text`
 *  begins in the document, which is what turns a reading into a range a mark can be written over;
 *  it is asked only for the readings a caller can still use. */
function readingsOf(
  text: string,
  quote: string,
  surroundings: Surroundings,
  positionAt: (offset: number) => number
): Readings {
  const surrounded: MarkRange[] = [];
  let count = 0;
  let first: MarkRange | undefined;
  if (quote === "") return { count, first, surrounded };
  for (let at = text.indexOf(quote); at !== -1; at = text.indexOf(quote, at + 1)) {
    count += 1;
    const matches = surrounded.length < 2 && surroundedBy(text, at, quote.length, surroundings);
    if (first !== undefined && !matches) continue;
    const range = { from: positionAt(at), to: positionAt(at + quote.length) };
    first ??= range;
    if (matches) surrounded.push(range);
  }
  return { count, first, surrounded };
}

/** One block of a document's text: where its own content opens in the document, where its first
 *  character sits in that text, and how many characters it reads there. */
interface TextBlock {
  readonly at: number;
  readonly base: number;
  readonly length: number;
}

/** One block's own text, read as the selection bar reads a quote out of the document:
 *  `textBetween` with a newline for each break, where an inline atom stands for one character as
 *  it does for one position, so a text offset is a position offset. The one read behind every
 *  scan of a block - a placement's count of its readings, and a re-mark's choice between them -
 *  so no two of them can disagree about what reads there. */
function textOf(block: ProseMirrorNode): string {
  return block.textBetween(0, block.content.size, "\n", "\n");
}

/** The whole document as one text, with where each of its blocks sits in it: every textblock's
 *  own text, joined by a newline for each block break, which is what `textBetween(from, to, "\n",
 *  "\n")` reads out of the document and so what the selection bar reads a quote out of
 *  (./dispatch-action-bar.ts `runAction`). A quote the reader selected across a block break -
 *  which no placement can describe, no one block's text reading it - is found here and nowhere
 *  else. */
function documentText(doc: ProseMirrorNode): { blocks: TextBlock[]; text: string } {
  const blocks: TextBlock[] = [];
  let text = "";
  doc.descendants((node, pos) => {
    // A block that holds blocks carries no text of its own; a leaf block - a horizontal rule -
    // reads as the one newline `textBetween` gives it, and a textblock as its own text.
    const leaf = node.isLeaf && !node.isTextblock;
    if (!node.isTextblock && !leaf) return true;
    const own = leaf ? "\n" : textOf(node);
    if (blocks.length > 0) text += "\n";
    blocks.push({ at: text.length, base: leaf ? pos : pos + 1, length: own.length });
    text += own;
    return false;
  });
  return { blocks, text };
}

/** Where the character at offset `at` of a document's text begins in the document. Inside a block
 *  an offset counts as a position does; the newline between two blocks is the one character that
 *  spans a gap, and it begins where the block in front of it ends - which is where a reading that
 *  stops at the break stops, and where one that starts there starts. */
function positionIn(blocks: readonly TextBlock[], at: number): number {
  let low = 0;
  let high = blocks.length - 1;
  while (low < high) {
    const middle = (low + high + 1) >> 1;
    if (blocks[middle].at <= at) low = middle;
    else high = middle - 1;
  }
  const block = blocks[low];
  return block.base + Math.min(at - block.at, block.length);
}

/** The textblock `blockId` names and where its content starts, or null once the document holds
 *  no such block. */
function blockWithId(
  doc: ProseMirrorNode,
  blockId: string
): { base: number; block: ProseMirrorNode } | null {
  let found: { base: number; block: ProseMirrorNode } | null = null;
  doc.descendants((node, pos) => {
    if (found !== null || !node.isBlock) return false;
    if (!node.isTextblock) return true;
    if (blockIdOf(node) === blockId) found = { base: pos + 1, block: node };
    // A textblock holds inline content alone, and no block id sits below one.
    return false;
  });
  return found;
}

/** Where a mark written over `range` sits, for the composer that owns it to remember: the id of
 *  the textblock that holds it, the text that block reads on each side of `quote`, at most
 *  `QUOTE_CONTEXT_CHARS` of it and shorter where the block begins or ends nearer than that, and
 *  how many readings of `quote` that block holds. Null when there is nothing to remember - the
 *  range is not inside a textblock, or that block's text does not read `quote` there, which is
 *  every selection that runs across a block break - which leaves a later re-mark the quote alone
 *  to go by. A block with no id is remembered all the same: its surroundings still say which
 *  passage this is. */
export function placementOf(
  doc: ProseMirrorNode,
  range: MarkRange,
  quote: string
): RecordMarkPlacement | null {
  if (quote === "") return null;
  const $start = doc.resolve(range.from);
  if (!$start.parent.isTextblock) return null;
  const base = $start.start();
  const text = textOf($start.parent);
  const offset = range.from - base;
  if (text.slice(offset, offset + quote.length) !== quote) return null;
  const after = offset + quote.length;
  return {
    blockId: blockIdOf($start.parent),
    prefix: text.slice(Math.max(0, offset - QUOTE_CONTEXT_CHARS), offset),
    // The scan a re-mark reads this block with, so the count kept here and the readings found
    // then are the same readings.
    readings: readingsOf(text, quote, NO_SURROUNDINGS, (at) => base + at).count,
    suffix: text.slice(after, after + QUOTE_CONTEXT_CHARS),
  };
}

/** Where the fresh mark goes, by the order this module's banner gives, or the refusal to answer
 *  where no reading of `quote` can be taken for the one the composer was opened over: the step
 *  that refuses names it, because only it knows which case the document is in. */
function remarkRange(
  state: EditorState,
  markId: string,
  quote: string,
  placement: RecordMarkPlacement | null
): MarkRange | RemarkRefusal {
  const held = findRecordMark(state.doc, markId);
  if (held !== null) return held.range;
  const surroundings = placement ?? NO_SURROUNDINGS;
  const block =
    placement === null || placement.blockId === null
      ? null
      : blockWithId(state.doc, placement.blockId);
  if (block !== null) {
    // The reader's own block, while it still reads their words, read as the selection bar read
    // the quote out of it. One scan of it answers all three questions below.
    const blockText = textOf(block.block);
    const readings = readingsOf(blockText, quote, surroundings, (at) => block.base + at);
    // Two readings there surrounded as theirs was: another writer's reading of the same words
    // among the same words is not one this editor may choose between, and their own words have
    // not moved an inch, so the refusal is neither "elsewhere" nor "the text around them
    // changed".
    if (readings.surrounded.length > 1) return { refused: "ambiguous" };
    if (readings.surrounded.length === 1) return readings.surrounded[0];
    // Nothing there is surrounded as theirs was: another writer rewrote the words around them.
    // A block that held those words once and holds them once now is the same passage however
    // the sentence around them was rewritten - there was never another reading there to confuse
    // it with. A block that held them more than once is not: the reading left may be the one
    // the reader never selected, which is what deleting their own sentence leaves behind.
    if (readings.count === 1 && placement?.readings === 1 && readings.first !== undefined) {
      return readings.first;
    }
    if (readings.count > 0) return { refused: "unsurrounded" };
  }
  // The block is gone, or reads those words nowhere: the one reading in the whole document
  // surrounded as the reader's was. Words alone are not enough - a reading among other words is
  // another passage, and a record moved there is something no refusal would ever tell them about.
  // The document is read as one text across its blocks, so a quote that runs across a break reads
  // here as the bar read it when the reader selected it.
  const { blocks, text } = documentText(state.doc);
  const readings = readingsOf(text, quote, surroundings, (at) => positionIn(blocks, at));
  if (readings.surrounded.length === 1) return readings.surrounded[0];
  // More than one reading surrounded alike, where the composer kept where the reader selected:
  // the words are where they were and nothing says which of them is theirs, exactly as two
  // alike inside the reader's own block. Saying instead that the words are no longer where they
  // were selected would be false - they never moved.
  if (readings.surrounded.length > 1 && placement !== null) return { refused: "ambiguous" };
  // With no placement kept - a selection across a block break, which no one block's text reads,
  // or a send held by a tab older than the placement - there are no surroundings to answer, so
  // every reading answers `NO_SURROUNDINGS` and more than one of them says only that the words
  // read in more than one place. What reads around those readings is not the same text, so the
  // refusal for readings surrounded alike would be false here; what is true is that nothing kept
  // of the selection says which reading was theirs. Their words never moved here either.
  if (readings.surrounded.length > 1) return { refused: "unremembered" };
  // Telling the reader the words were changed or deleted while they are plainly on the page is
  // false too. The count is of that same text, so a quote on the page across a break is never
  // called deleted.
  return { refused: readings.count > 0 ? "unplaceable" : "gone" };
}

/**
 * Writes a fresh mark of `kind`, by `by`, over `quote` - the words the composer that owns the
 * record mark `markId` is anchored to - wherever they are now, takes the old mark out, and answers
 * the mark it wrote with the placement to remember for it. The first of these that holds: the
 * mark's own range, while the document still holds it; inside the block `placement` names, while
 * that block still reads the quote, the one reading there surrounded as `placement` remembers, or
 * its only reading of the quote when none of them is and that block held the quote once when the
 * reader selected it; and otherwise the one reading so surrounded in the whole document, read as
 * one text across its blocks. Anything else is a refusal: readings the editor cannot tell apart,
 * readings in a block that held the words more than once and no longer surrounds any of them as
 * the reader's, and readings among words the reader never read beside theirs, are passages it may
 * not choose for them.
 *
 * The id is always a fresh one. The server refuses a send because it cannot see the mark, and a
 * mark it has dropped whose local copy has not caught up would be refused again under that id, so
 * the retry carries a mark the server has never judged, whether or not the kind changed.
 */
export function remarkRecordMark(
  view: EditorView,
  markId: string,
  kind: SelectionBarActionKind,
  by: string,
  quote: string,
  placement: RecordMarkPlacement | null
): RemarkOutcome {
  const range = remarkRange(view.state, markId, quote, placement);
  if ("refused" in range) return range;
  // What the mark ends up over, which is the quote itself except under a mark the document still
  // holds over text that has changed since: the record is re-addressed to the words it marks.
  const marked = view.state.doc.textBetween(range.from, range.to, "\n", "\n");
  // Read while the positions are still the ones `range` is in, and before the old mark goes.
  const placed = placementOf(view.state.doc, range, marked);
  const created = createRecordMark(view, kind, range, marked, by);
  if (created === null) return { refused: "unmarkable" };
  removeRecordMark(view, markId);
  return { markId: created, placement: placed, quote: marked };
}
