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
 * document still has it; the tracked range the plugin below keeps, when its text reads as that
 * quote; inside the remembered block, while the document still holds it and it still reads the
 * quote, the one reading there whose surroundings match, or its only reading of the quote when
 * none of them match and the block held the quote once when the reader selected it (a block that
 * read their words once then and reads them once now is their passage however the sentence
 * around them was rewritten, there having been no other reading there to confuse it with, while
 * a block that read them twice may have been left the reading they never selected); and
 * otherwise, the block being gone or reading the quote nowhere, the one reading in the whole
 * document whose surroundings match. Nothing is taken for the reader's passage on the strength
 * of the words alone: another paragraph that reads them among different words is a different
 * passage.
 *
 * Only the first two steps ask this editor's state for anything, and nothing of it survives
 * another writer's change: y-prosemirror applies a remote update by rebuilding the document
 * rather than through a mapping a local step would give, and a reconnect rebuilds the editor,
 * taking every plugin's state with it. The placement is what carries the reader's own passage
 * through that, because the composer keeps it outside the editor and the editor resolves it by
 * reading the document alone - no browser-versus-server counting, and no index that another
 * writer's reading of the same words in front of the reader's silently moves. Where no reading
 * can be chosen - more than one answers the surroundings, or none does and the block held the
 * words more than once - the re-mark is refused rather than anchored to a passage the reader
 * never selected, which no refusal would ever tell them about. The refusal says which case it
 * was: the words read nowhere (`"gone"`), their own block still reads them but no reading there
 * is surrounded as theirs was (`"unsurrounded"`), or they read elsewhere in the document and no
 * reading of them is the reader's own to take (`"unplaceable"`), which the margin puts to the
 * reader in different words.
 *
 * `removeRecordMark` is the precise removal both this module and the handle's `removeMark` use:
 * span by span, by id, whatever the mark's type. Upstream's `deleteMark` is not it - it cannot
 * see a provisional comment (`buildAnchorMarks` drops a comment anchor whose body is empty, and
 * the bar creates comments with an empty body) - and nothing else is needed: once the spans are
 * gone, the marks plugin's own `normalizeMetadata` drops the metadata of every id no longer
 * anchored. It is also where a tracked entry ends: the composer that owned the mark is over.
 */

import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import { AddMarkStep } from "@milkdown/kit/prose/transform";
import type { EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";
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
 *  reading was theirs (`"unsurrounded"`); or they read somewhere else in the document and no
 *  reading of them can be taken for the reader's own (`"unplaceable"`). None of the three is the
 *  same thing to say to a reader, so none of them is the same answer. A re-mark never answers
 *  `"missing"`: a mark the document has lost is the case it is for. */
export interface RemarkRefusal {
  readonly refused: "gone" | "unmarkable" | "unplaceable" | "unsurrounded";
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

/** Every record mark this editor wrote that no `removeRecordMark` has ended, by its id: where
 *  this editor wrote it, mapped through every transaction since. The words it was written over
 *  are not kept here - `remarkRecordMark` is given the composer's own quote, the one copy of
 *  those words that outlives this editor's state. */
type TrackedMarks = ReadonlyMap<string, MarkRange>;

/** The tracking plugin's key, which also names the meta `removeRecordMark` sets to end an entry:
 *  the id it removes. */
const recordMarkTracking = new PluginKey<TrackedMarks>("recordMarkTracking");

/** The record marks `transaction` writes, each id's spans as one range in the document the
 *  transaction leaves: a creator marks text node by text node, and the whole run is the entry.
 *  `undefined` when it writes none, which is nearly every transaction. */
function recordMarksWrittenBy(transaction: Transaction): Map<string, MarkRange> | undefined {
  let written: Map<string, MarkRange> | undefined;
  transaction.steps.forEach((step, index) => {
    if (!(step instanceof AddMarkStep) || !RECORD_MARK_TYPES.has(step.mark.type.name)) return;
    const id: unknown = step.mark.attrs.id;
    if (typeof id !== "string") return;
    // A mark step maps no position, but a later step of the same transaction can, so each span is
    // read in the coordinates the whole transaction leaves.
    const after = transaction.mapping.slice(index + 1);
    const from = after.map(step.from, 1);
    const to = after.map(step.to, -1);
    written ??= new Map<string, MarkRange>();
    const span = written.get(id);
    written.set(
      id,
      span === undefined
        ? { from, to }
        : { from: Math.min(span.from, from), to: Math.max(span.to, to) }
    );
  });
  return written;
}

/** `tracked` after `transaction`: every entry mapped to the document it leaves - kept whether or
 *  not the mark is still on that text - with the marks it writes added and the entry a
 *  `removeRecordMark` ends dropped. */
function trackedAfter(tracked: TrackedMarks, transaction: Transaction): TrackedMarks {
  const written = recordMarksWrittenBy(transaction);
  const ended: unknown = transaction.getMeta(recordMarkTracking);
  if (
    written === undefined &&
    ended === undefined &&
    (tracked.size === 0 || !transaction.docChanged)
  ) {
    return tracked;
  }
  const next = new Map<string, MarkRange>();
  for (const [id, range] of tracked) {
    next.set(
      id,
      transaction.docChanged
        ? {
            // The range keeps to what it covered: text written at either edge is the reader's
            // prose, not the words the record was written over.
            from: transaction.mapping.map(range.from, 1),
            to: transaction.mapping.map(range.to, -1),
          }
        : range
    );
  }
  if (written !== undefined) {
    for (const [id, range] of written) next.set(id, range);
  }
  if (typeof ended === "string") next.delete(ended);
  return next;
}

/**
 * Remembers where every record mark this editor writes - the selection bar's, a retype's, a
 * re-mark's - sits, through every transaction, and goes on remembering it after a transaction
 * takes the mark out of the document: a tracked range is the precise answer for `remarkRecordMark`
 * while this editor's own state stands, and the only one that tells two readings of the same words
 * apart. An entry ends with `removeRecordMark`, where the composer that owned the mark is over,
 * and every other entry with the editor state itself.
 */
export const recordMarkTrackingPlugin = $prose(
  () =>
    new Plugin<TrackedMarks>({
      key: recordMarkTracking,
      state: {
        init: () => new Map<string, MarkRange>(),
        apply: (transaction, tracked) => trackedAfter(tracked, transaction),
      },
    })
);

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
  let transaction = view.state.tr
    .setMeta(recordMarkRemoval, composerRemoval)
    .setMeta(recordMarkTracking, markId);
  view.state.doc.descendants((node, pos) => {
    if (!node.isInline) return true;
    for (const mark of node.marks) {
      if (RECORD_MARK_TYPES.has(mark.type.name) && mark.attrs.id === markId) {
        transaction = transaction.removeMark(pos, pos + node.nodeSize, mark);
      }
    }
    return true;
  });
  const removed = transaction.docChanged;
  // A mark another writer's edit already dropped has an entry and no span, and the composer's end
  // has to reach the tracking all the same.
  if (!removed && recordMarkTracking.getState(view.state)?.has(markId) !== true) return false;
  view.dispatch(transaction);
  return removed;
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

/** What a re-mark matches readings against where the composer kept no placement - a quote that
 *  runs across two blocks has none - and every reading answers it, which leaves the words alone
 *  to go by, as they were all there ever was in that case. */
const NO_SURROUNDINGS: Surroundings = { prefix: "", suffix: "" };

/** Whether the reading of a `length`-character quote at offset `at` of the block text `text` is
 *  surrounded by `surroundings`: the text before it ends with the remembered prefix, and the text
 *  after it starts with the remembered suffix. Read in place, so scanning a block for its
 *  readings copies none of its text. */
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

/** Every reading of `quote` in the textblock `block`, whose own content starts at position
 *  `base`, in the order the block's text reads them, at most `limit` of them - and, where
 *  `surroundings` is given, only those the text around them matches. The block's text is read as
 *  the selection bar reads a quote out of the document - `textBetween` with a newline for each
 *  break - so a quote that runs across two blocks is never found in one. An inline atom stands
 *  for one character there, as it does in the block's positions, so a text offset is a
 *  position offset. */
function readingsInBlock(
  block: ProseMirrorNode,
  base: number,
  quote: string,
  limit: number,
  surroundings?: Surroundings
): MarkRange[] {
  const found: MarkRange[] = [];
  if (quote === "" || limit < 1) return found;
  const text = block.textBetween(0, block.content.size, "\n", "\n");
  for (let at = text.indexOf(quote); at !== -1; at = text.indexOf(quote, at + 1)) {
    if (surroundings !== undefined && !surroundedBy(text, at, quote.length, surroundings)) continue;
    found.push({ from: base + at, to: base + at + quote.length });
    if (found.length === limit) break;
  }
  return found;
}

/** Every reading of `quote` in `doc`, textblock by textblock, at most `limit` of them, matched
 *  against `surroundings` as `readingsInBlock` matches them. Each block's text is read on its
 *  own, so the surroundings of a reading are the words of its own block and never the block
 *  before it. */
function readingsInDoc(
  doc: ProseMirrorNode,
  quote: string,
  limit: number,
  surroundings?: Surroundings
): MarkRange[] {
  const found: MarkRange[] = [];
  if (quote === "" || limit < 1) return found;
  doc.descendants((node, pos) => {
    if (found.length === limit) return false;
    if (!node.isTextblock) return true;
    // `descendants` counts from the start of the document's content, and a block's own content
    // opens one position inside it.
    found.push(...readingsInBlock(node, pos + 1, quote, limit - found.length, surroundings));
    return false;
  });
  return found;
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

/** How many readings of `quote` the block text `text` holds, counted the way `readingsInBlock`
 *  finds them, so a placement's count and a later scan of the same block agree. */
function readingCount(text: string, quote: string): number {
  let count = 0;
  for (let at = text.indexOf(quote); at !== -1; at = text.indexOf(quote, at + 1)) count += 1;
  return count;
}

/** Where a mark written over `range` sits, for the composer that owns it to remember: the id of
 *  the textblock that holds it, the text that block reads on each side of `quote`, at most
 *  `QUOTE_CONTEXT_CHARS` of it and shorter where the block begins or ends nearer than that, and
 *  how many readings of `quote` that block holds. Null when there is nothing to remember - the
 *  range is not inside a textblock, or that block's text does not read `quote` there - which
 *  leaves a later re-mark the quote alone to go by. A block with no id is remembered all the
 *  same: its surroundings still say which passage this is. */
export function placementOf(
  doc: ProseMirrorNode,
  range: MarkRange,
  quote: string
): RecordMarkPlacement | null {
  if (quote === "") return null;
  const $start = doc.resolve(range.from);
  if (!$start.parent.isTextblock) return null;
  const text = $start.parent.textBetween(0, $start.parent.content.size, "\n", "\n");
  const offset = range.from - $start.start();
  if (text.slice(offset, offset + quote.length) !== quote) return null;
  const after = offset + quote.length;
  return {
    blockId: blockIdOf($start.parent),
    prefix: text.slice(Math.max(0, offset - QUOTE_CONTEXT_CHARS), offset),
    readings: readingCount(text, quote),
    suffix: text.slice(after, after + QUOTE_CONTEXT_CHARS),
  };
}

/** The range the tracking kept for `markId`, mapped to the document as it reads now, when the
 *  text there is exactly `quote`: the precise answer while this editor's own state stands, and
 *  the only one that needs no reading of the words at all. Null when nothing tracks the mark -
 *  the state it was written in is gone - and null when the text there has changed, which leaves
 *  those words to the steps after it. */
function trackedRange(state: EditorState, markId: string, quote: string): MarkRange | null {
  const tracked = recordMarkTracking.getState(state)?.get(markId);
  if (tracked === undefined || quote === "") return null;
  const size = state.doc.content.size;
  const from = Math.min(tracked.from, size);
  const to = Math.min(tracked.to, size);
  return to > from && state.doc.textBetween(from, to, "\n", "\n") === quote ? { from, to } : null;
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
  const tracked = trackedRange(state, markId, quote);
  if (tracked !== null) return tracked;
  const surroundings = placement ?? NO_SURROUNDINGS;
  const block =
    placement === null || placement.blockId === null
      ? null
      : blockWithId(state.doc, placement.blockId);
  if (block !== null) {
    // The reader's own block, while it still reads their words. One reading there surrounded as
    // theirs was is theirs; two are a refusal, since another writer's reading of the same words
    // among the same words is not one this editor may choose between.
    const surrounded = readingsInBlock(block.block, block.base, quote, 2, surroundings);
    if (surrounded.length === 1) return surrounded[0];
    if (surrounded.length > 1) return { refused: "unplaceable" };
    // Nothing there is surrounded as theirs was: another writer rewrote the words around them.
    // A block that held those words once and holds them once now is the same passage however
    // the sentence around them was rewritten - there was never another reading there to confuse
    // it with. A block that held them more than once is not: the reading left may be the one
    // the reader never selected, which is what deleting their own sentence leaves behind.
    const readings = readingsInBlock(block.block, block.base, quote, 2);
    if (readings.length === 1 && placement?.readings === 1) return readings[0];
    if (readings.length > 0) return { refused: "unsurrounded" };
  }
  // The block is gone, or reads those words nowhere: the one reading in the whole document
  // surrounded as the reader's was. Words alone are not enough - a reading among other words is
  // another passage, and a record moved there is something no refusal would ever tell them about.
  const readings = readingsInDoc(state.doc, quote, 2, surroundings);
  if (readings.length === 1) return readings[0];
  // Only the document can say which of the two this is, and the margin needs to know: telling the
  // reader the words were changed or deleted while they are plainly on the page is false.
  const onThePage = readings.length > 0 || readingsInDoc(state.doc, quote, 1).length > 0;
  return { refused: onThePage ? "unplaceable" : "gone" };
}

/**
 * Writes a fresh mark of `kind`, by `by`, over `quote` - the words the composer that owns the
 * record mark `markId` is anchored to - wherever they are now, takes the old mark out, and answers
 * the mark it wrote with the placement to remember for it. The first of these that holds: the
 * mark's own range, while the document still holds it; the tracked range, when its text reads as
 * `quote`; inside the block `placement` names, while that block still reads the quote, the one
 * reading there surrounded as `placement` remembers, or its only reading of the quote when none
 * of them is and that block held the quote once when the reader selected it; and otherwise the
 * one reading in the whole document so surrounded. Anything else is a refusal: readings the
 * editor cannot tell apart, readings in a block that held the words more than once and no longer
 * surrounds any of them as the reader's, and readings among words the reader never read beside
 * theirs, are passages it may not choose for them.
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
