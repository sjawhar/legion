import { Plugin } from "@milkdown/kit/prose/state";
import { AddMarkStep, RemoveMarkStep } from "@milkdown/kit/prose/transform";
import { $prose } from "@milkdown/kit/utils";

import { isRecordMarkRemoval, RECORD_MARK_TYPES } from "./record-mark-retype";

/**
 * Keeps the composer's own record-mark writes out of undo history.
 *
 * A comment, suggestion or ask mark (`proofComment`, `proofSuggestion`, `dispatchAsk`) is the
 * anchor of a server-owned record, or the provisional anchor of one being composed: the selection
 * bar writes it, the margin retypes or removes it, the server writes and sweeps it. None of those
 * is an edit the reader made to the text, so none of them should be an undo step. Recorded,
 * Ctrl/Cmd+Z after a Comment → Ask switch would put the comment mark back under an id no composer
 * holds and no record will claim, and redo would write it again - in every browser, since marks
 * are document content.
 *
 * Two shapes of transaction are stamped `addToHistory: false`, which both undo managers honour
 * (prosemirror-history skips the transaction and maps its own items through it; y-prosemirror's
 * sync plugin copies the meta onto the Yjs transaction, where its UndoManager's
 * `captureTransaction` reads it): a pure creation, whose steps are all `AddMarkStep`s of record
 * types; and `removeRecordMark`'s removal of the composer's own provisional mark, which only that
 * function can label (`isRecordMarkRemoval`). A creation that also removes a mark is one that
 * displaced another record's span - a mark type excludes itself, so `addMark` over text another
 * comment covers cuts that comment's anchor there (LEGION-458) - and it keeps its undo step,
 * exactly as before this plugin, so one undo still restores the span. A mixed transaction - a
 * suggestion accept that replaces text and drops its mark - is the reader's edit and stays
 * undoable too.
 */
export const recordMarkHistoryPlugin = $prose(
  () =>
    new Plugin({
      filterTransaction(transaction) {
        if (transaction.steps.length === 0 || transaction.getMeta("addToHistory") === false) {
          return true;
        }
        const recordMarkSteps = transaction.steps.every(
          (step) =>
            (step instanceof AddMarkStep || step instanceof RemoveMarkStep) &&
            RECORD_MARK_TYPES[step.mark.type.name] === true
        );
        if (!recordMarkSteps) return true;
        const ownWriteOnly =
          isRecordMarkRemoval(transaction) ||
          transaction.steps.every((step) => step instanceof AddMarkStep);
        if (ownWriteOnly) {
          transaction.setMeta("addToHistory", false);
        }
        return true;
      },
    })
);
