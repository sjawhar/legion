import { Plugin, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import { AddMarkStep, RemoveMarkStep, type Step } from "@milkdown/kit/prose/transform";
import type { EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

import { isRecordMarkRemoval, RECORD_MARK_TYPES } from "./record-mark-retype";

/** The plugin's state: the record mark the open margin composer holds, or null. */
const composerMarkKey = new PluginKey<string | null>("recordMarkHistory");

/** Transactions this plugin kept out of history, for `appendTransaction` to recognise. */
const stamped = new WeakSet<Transaction>();

/** Tells the editor which record mark the open margin composer holds, or that none is open: a
 *  write that cuts into that mark - the reader refining the selection before closing the
 *  composer - is the composer's own and stays out of undo history. */
export function setComposerMark(view: EditorView, markId: string | null): void {
  view.dispatch(view.state.tr.setMeta(composerMarkKey, markId));
}

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

/** Whether record-mark `steps` take nothing from another record: every span they remove is of
 *  the composer's own mark, or added back with the same type and id. Mark steps map no positions,
 *  so every step's range is in the same coordinates. */
function takesNothing(
  steps: readonly (AddMarkStep | RemoveMarkStep)[],
  composerMarkId: string | null
): boolean {
  const removed = new Map<string, Span[]>();
  const added = new Map<string, Span[]>();
  for (const step of steps) {
    const { id } = step.mark.attrs;
    if (step instanceof RemoveMarkStep && composerMarkId !== null && id === composerMarkId) {
      continue;
    }
    const spans = step instanceof RemoveMarkStep ? removed : added;
    const key = `${step.mark.type.name} ${id}`;
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
 * only that function can label, `isRecordMarkRemoval`), or a write whose every removal is either
 * of the mark the open composer holds (`setComposerMark`) or put straight back over the same text.
 * Upstream's creators write that last kind: they restamp every suggestion mark whose attributes
 * differ from its metadata - a recorded suggestion, whose replacement only the marks map carries -
 * with a removal and an addition of the same mark. A write that removes another record's mark is
 * one that displaced that record's span - a mark type excludes itself, so `addMark` over text
 * another comment covers cuts that comment's anchor there (LEGION-458) - and it keeps its undo
 * step, exactly as before this plugin, so one undo still restores the span. A mixed transaction - a
 * suggestion accept that replaces text and drops its mark - is the reader's edit and stays
 * undoable too.
 *
 * Both undo managers honour the stamp. prosemirror-history skips the transaction, and the
 * transactions other plugins append to it. y-prosemirror's sync plugin copies the flag of the
 * batch's last transaction onto the Yjs transaction its UndoManager reads, and plugins append
 * after a mark write - commonmark's hard-break mark cleaner appends one after every creation - so
 * the plugin ends a stamped transaction's batch with a stamped transaction of its own.
 */
export const recordMarkHistoryPlugin = $prose(
  () =>
    new Plugin<string | null>({
      appendTransaction(transactions, _oldState, newState) {
        const last = transactions[transactions.length - 1];
        if (last === undefined) return null;
        const root: Transaction = last.getMeta("appendedTransaction") ?? last;
        if (last === root || !stamped.has(root) || last.getMeta("addToHistory") === false) {
          return null;
        }
        return newState.tr.setMeta("addToHistory", false);
      },
      filterTransaction(transaction, state) {
        if (transaction.steps.length === 0 || transaction.getMeta("addToHistory") === false) {
          return true;
        }
        const steps = transaction.steps;
        if (!steps.every(isRecordMarkStep)) return true;
        if (
          isRecordMarkRemoval(transaction) ||
          takesNothing(steps, composerMarkKey.getState(state) ?? null)
        ) {
          transaction.setMeta("addToHistory", false);
          stamped.add(transaction);
        }
        return true;
      },
      key: composerMarkKey,
      state: {
        apply: (transaction, held) => {
          const next: string | null | undefined = transaction.getMeta(composerMarkKey);
          return next === undefined ? held : next;
        },
        init: () => null,
      },
    })
);
