import { Plugin, type Transaction } from "@milkdown/kit/prose/state";
import { AddMarkStep, RemoveMarkStep, type Step } from "@milkdown/kit/prose/transform";
import { $prose } from "@milkdown/kit/utils";

import { isRecordMarkRemoval, RECORD_MARK_TYPES } from "./record-mark-retype";

/** Transactions this plugin kept out of history, for `appendTransaction` to recognise. */
const stamped = new WeakSet<Transaction>();

function isRecordMarkStep(step: Step): step is AddMarkStep | RemoveMarkStep {
  return (
    (step instanceof AddMarkStep || step instanceof RemoveMarkStep) &&
    RECORD_MARK_TYPES.has(step.mark.type.name)
  );
}

interface Span {
  from: number;
  to: number;
}

/** `spans` sorted, with touching and overlapping spans merged. */
function merged(spans: readonly Span[]): Span[] {
  const sorted = [...spans].sort((a, b) => a.from - b.from);
  const result: Span[] = [];
  for (const span of sorted) {
    const last = result[result.length - 1];
    if (last !== undefined && span.from <= last.to) last.to = Math.max(last.to, span.to);
    else result.push({ ...span });
  }
  return result;
}

/** Whether record-mark `steps` take nothing from another record: every span they remove is added
 *  back with the same type and id. Mark steps map no positions, so every step's range is in the
 *  same coordinates. */
function takesNothing(steps: readonly (AddMarkStep | RemoveMarkStep)[]): boolean {
  const removed = new Map<string, Span[]>();
  const added = new Map<string, Span[]>();
  for (const step of steps) {
    const spans = step instanceof RemoveMarkStep ? removed : added;
    const key = `${step.mark.type.name} ${step.mark.attrs.id}`;
    spans.set(key, [...(spans.get(key) ?? []), { from: step.from, to: step.to }]);
  }
  for (const [key, spans] of removed) {
    const back = merged(added.get(key) ?? []);
    const lost = merged(spans).some(
      (span) => !back.some((kept) => kept.from <= span.from && span.to <= kept.to)
    );
    if (lost) return false;
  }
  return true;
}

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
 * A transaction of record-mark steps only is stamped `addToHistory: false` when it takes nothing
 * from another record: `removeRecordMark`'s removal of the composer's own provisional mark (which
 * only that function can label, `isRecordMarkRemoval`), or a write whose every removal is put
 * straight back over the same text. A creation removes nothing at all: the record marks declare
 * `excludes: ''`, so a mark written over text another record's mark covers sits beside that mark
 * rather than cutting it (LEGION-458). Upstream's creators also restamp every suggestion mark
 * whose attributes differ from its metadata - a recorded suggestion, whose replacement only the
 * marks map carries - with a removal and an addition of the same mark, which takes nothing either.
 * Any other removal of a record mark - upstream's `deleteMark`, or any writer's own
 * `tr.removeMark` - is the reader's edit and keeps its undo step, as before this plugin, and so
 * does a mixed transaction: a suggestion accept that replaces text and drops its mark.
 *
 * Both undo managers honour the stamp. prosemirror-history skips the transaction, and the
 * transactions other plugins append to it. y-prosemirror's sync plugin copies the flag of the
 * batch's last transaction onto the Yjs transaction its UndoManager reads, and plugins append
 * after a mark write - commonmark's hard-break mark cleaner appends one after every creation - so
 * the plugin ends a stamped transaction's batch with a stamped transaction of its own.
 */
export const recordMarkHistoryPlugin = $prose(
  () =>
    new Plugin({
      appendTransaction(transactions, _oldState, newState) {
        const last = transactions[transactions.length - 1];
        if (last === undefined) return null;
        const root: Transaction = last.getMeta("appendedTransaction") ?? last;
        if (last === root || !stamped.has(root) || last.getMeta("addToHistory") === false) {
          return null;
        }
        return newState.tr.setMeta("addToHistory", false);
      },
      filterTransaction(transaction) {
        if (transaction.steps.length === 0 || transaction.getMeta("addToHistory") === false) {
          return true;
        }
        const steps = transaction.steps;
        if (!steps.every(isRecordMarkStep)) return true;
        if (isRecordMarkRemoval(transaction) || takesNothing(steps)) {
          transaction.setMeta("addToHistory", false);
          stamped.add(transaction);
        }
        return true;
      },
    })
);
