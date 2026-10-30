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
 * anchor to. The margin hands back the words the composer is anchored to - the quote its own
 * request carries - and the fresh mark goes over the first of these that holds: the mark's own
 * range, while the document still has it; the tracked range the plugin below keeps, when its text
 * reads as that quote; the one place the quote reads in the textblock that range starts in; the
 * one place the quote reads in the whole document. That last one needs no tracking at all, which
 * is what carries the recovery through a change another writer made: y-prosemirror applies a
 * remote update by rebuilding the document rather than through a mapping a local step would give,
 * and a reconnect can rebuild the editor, taking every plugin's state with it. One place, one
 * meaning: a second reading of those words would anchor the record to a passage the reader never
 * selected, which no refusal would ever tell them about, so two readings and no usable tracked
 * range is a refusal.
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
import type { SelectionBarActionKind } from "./dispatch-marks";
import { createAskMark } from "./dispatch-marks";

const MARK_TYPE_FOR_KIND: Record<SelectionBarActionKind, string> = {
  ask: "dispatchAsk",
  comment: "proofComment",
  suggest: "proofSuggestion",
};
/** The mark types a Dispatch record anchors to: the server's `docs.MarkKind` values. */
export const RECORD_MARK_TYPES: ReadonlySet<string> = new Set(Object.values(MARK_TYPE_FOR_KIND));

export interface RetypedMark {
  readonly markId: string;
  readonly quote: string;
}

/** Why a retype changed nothing: the document no longer holds the mark on any text, or the new
 *  kind cannot be written over its text (a suggestion across table cells). */
export interface RetypeRefusal {
  readonly refused: "missing" | "unmarkable";
}

export type RetypeOutcome = RetypedMark | RetypeRefusal;

/** Why a re-mark changed nothing: the new kind cannot be written over the text, as a retype's
 *  `"unmarkable"` says, or the words the mark was written over are no longer there to mark
 *  (`"gone"`). A re-mark never answers `"missing"`: a mark the document has lost is the case it
 *  is for. */
export interface RemarkRefusal {
  readonly refused: "gone" | "unmarkable";
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
 *  `by`, and answers the new mark - or the mark as it is, when it already has that kind. Refused,
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
  if (held.type === MARK_TYPE_FOR_KIND[kind]) return { markId, quote };
  const created = createRecordMark(view, kind, held.range, quote, by);
  if (created === null) return { refused: "unmarkable" };
  removeRecordMark(view, markId);
  return { markId: created, quote };
}

/** The one place `quote` reads inside `scope`, as a range in the document, where `scope`'s own
 *  content starts at position `base`: a textblock scope is read on its own, any other node
 *  textblock by textblock. The text is read as the selection bar reads a quote out of the
 *  document - `textBetween` with a newline for each break - so a quote that runs across two
 *  blocks is never found here. Null when the quote reads nowhere, and null when it reads twice:
 *  the second reading could be the passage the reader selected just as well as the first, and a
 *  record on the wrong passage is worse than a refusal the reader can act on. */
function soleOccurrence(scope: ProseMirrorNode, base: number, quote: string): MarkRange | null {
  if (quote === "") return null;
  const found: MarkRange[] = [];
  const readBlock = (block: ProseMirrorNode, start: number): void => {
    // An inline atom stands for one character here, as it does in the block's positions, so a
    // text offset is a position offset.
    const text = block.textBetween(0, block.content.size, "\n", "\n");
    for (let at = text.indexOf(quote); at !== -1; at = text.indexOf(quote, at + 1)) {
      found.push({ from: start + at, to: start + at + quote.length });
      if (found.length > 1) return;
    }
  };
  if (scope.isTextblock) {
    readBlock(scope, base);
  } else {
    scope.descendants((node, pos) => {
      if (found.length > 1) return false;
      if (!node.isTextblock) return true;
      // `descendants` counts from the start of `scope`'s content, and a block's own content opens
      // one position inside it.
      readBlock(node, base + pos + 1);
      return false;
    });
  }
  return found.length === 1 ? found[0] : null;
}

/** Where the record mark `markId` was written, as the document reads now, for a composer anchored
 *  to `quote`: the tracked range, when its text still reads as that quote, else the one place the
 *  quote reads in the textblock that range starts in. Null when nothing tracks the mark - the
 *  state the mark was written in is gone - and null when neither reading is the one place those
 *  words are, which leaves the whole document to `remarkRecordMark`'s last step. */
function trackedRange(state: EditorState, markId: string, quote: string): MarkRange | null {
  const tracked = recordMarkTracking.getState(state)?.get(markId);
  if (tracked === undefined || quote === "") return null;
  const size = state.doc.content.size;
  const from = Math.min(tracked.from, size);
  const to = Math.min(tracked.to, size);
  if (to > from && state.doc.textBetween(from, to, "\n", "\n") === quote) {
    return { from, to };
  }
  // A textblock holds inline content alone, so no ancestor of a parent that is not one can be a
  // textblock either: the mapped start either sits in text or in nothing this can mark.
  const $start = state.doc.resolve(from);
  return $start.parent.isTextblock ? soleOccurrence($start.parent, $start.start(), quote) : null;
}

/**
 * Writes a fresh mark of `kind`, by `by`, over `quote` - the words the composer that owns the
 * record mark `markId` is anchored to - wherever they are now, takes the old mark out, and answers
 * the mark it wrote. The first of these that holds: the mark's own range, while the document still
 * holds it; the tracked range, when its text reads as `quote`; the one place `quote` reads in the
 * textblock that range starts in; the one place `quote` reads in the whole document. The last step
 * asks the tracking for nothing, so it still answers after a remote update rebuilt the document or
 * a reconnect rebuilt the editor; the two before it are the precise answers while this editor's
 * own state stands, and the only ones that tell two readings of the same words apart.
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
  quote: string
): RemarkOutcome {
  const range =
    findRecordMark(view.state.doc, markId)?.range ??
    trackedRange(view.state, markId, quote) ??
    soleOccurrence(view.state.doc, 0, quote);
  if (range === null) return { refused: "gone" };
  // What the mark ends up over, which is the quote itself except under a mark the document still
  // holds over text that has changed since: the record is re-addressed to the words it marks.
  const marked = view.state.doc.textBetween(range.from, range.to, "\n", "\n");
  const created = createRecordMark(view, kind, range, marked, by);
  if (created === null) return { refused: "unmarkable" };
  removeRecordMark(view, markId);
  return { markId: created, quote: marked };
}
