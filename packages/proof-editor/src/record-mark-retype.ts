/**
 * Retyping a provisional record mark: the margin composer's Comment / Suggest / Ask switch.
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
 * record because a retype over text another record already marks with the new kind is refused.
 *
 * `removeRecordMark` is the precise removal both this module and the handle's `removeMark` use:
 * span by span, by id, whatever the mark's type. Upstream's `deleteMark` is not it - it cannot
 * see a provisional comment (`buildAnchorMarks` drops a comment anchor whose body is empty, and
 * the bar creates comments with an empty body) - and nothing else is needed: once the spans are
 * gone, the marks plugin's own `normalizeMetadata` drops the metadata of every id no longer
 * anchored.
 */

import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
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

/** Why a retype changed nothing: the document no longer holds the mark on any text; the new kind
 *  cannot be written over its text (a suggestion across table cells); or part of that text already
 *  carries another record's mark of the new kind, which writing over it would cut out of that
 *  record (LEGION-458). */
export interface RetypeRefusal {
  readonly refused: "missing" | "unmarkable" | "overlaps";
}

export type RetypeOutcome = RetypedMark | RetypeRefusal;

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

/** Whether any inline node in `range` - text or an atom such as an image - carries a mark of
 *  `type` under an id other than `markId`. */
function rangeHoldsAnother(
  doc: ProseMirrorNode,
  range: MarkRange,
  type: string,
  markId: string
): boolean {
  let held = false;
  doc.nodesBetween(range.from, range.to, (node) => {
    if (held || !node.isInline) return !held;
    held = node.marks.some((mark) => mark.type.name === type && mark.attrs.id !== markId);
    return !held;
  });
  return held;
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

/** Replaces the record mark `markId` with a fresh mark of `kind` over the same text, written by
 *  `by`, and answers the new mark - or the mark as it is, when it already has that kind. Refused,
 *  with nothing changed, when the document does not hold `markId`, when `kind` cannot be marked
 *  over its text, or when part of that text already carries another record's mark of `kind`: a
 *  mark type excludes itself, so `addMark` would cut that record's anchor there (LEGION-458). */
export function retypeMark(
  view: EditorView,
  markId: string,
  kind: SelectionBarActionKind,
  by: string
): RetypeOutcome {
  const held = findRecordMark(view.state.doc, markId);
  if (held === null) return { refused: "missing" };
  const quote = view.state.doc.textBetween(held.range.from, held.range.to, "\n", "\n");
  const type = MARK_TYPE_FOR_KIND[kind];
  if (held.type === type) return { markId, quote };
  if (rangeHoldsAnother(view.state.doc, held.range, type, markId)) return { refused: "overlaps" };
  // The same creators, with the same arguments, as the selection bar's `runAction`.
  let created: string;
  if (kind === "comment") {
    created = comment(view, quote, by, "", held.range).id;
  } else if (kind === "suggest") {
    const suggestion = suggestReplace(view, quote, by, "", held.range);
    if (suggestion === null) return { refused: "unmarkable" };
    created = suggestion.id;
  } else {
    const ask = createAskMark(view, held.range, by);
    if (ask === null) return { refused: "unmarkable" };
    created = ask.id;
  }
  removeRecordMark(view, markId);
  return { markId: created, quote };
}
