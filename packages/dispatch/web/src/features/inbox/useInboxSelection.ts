/**
 * What the reader marked in the Inbox, and the two bulk writes the bar presses on it: the one
 * Snooze over the asks among the marks, and the one Dismiss over the threads.
 *
 * The state lives here rather than in `BulkBar` because either write can take every marked row
 * off the view at once: a bar that owned the write would be unmounted before its own refusal
 * arrived. The bar reports one press downward and renders what it is told.
 */

import { type RefObject, useCallback, useEffect, useRef, useState } from "react";

import type { InboxFilters } from "./InboxBar";
import { type InboxListRow, rowId } from "./list-row";
import { type AskSnoozeFailure, useAskSnoozeMany } from "./useAskSnooze";
import {
  dismissRow,
  type InboxDismiss,
  type InboxDismissRow,
  useInboxDismiss,
} from "./useInboxDismiss";

/**
 * What the bar says after a pick the server refused in part. The count alone cannot tell a
 * sign-out from `snoozed_until must be in the future`, so the reason travels with it: one reason
 * when every refusal gave the same one, otherwise the first and how many others there are.
 */
function snoozeRefusalText(failed: readonly AskSnoozeFailure[], of: number): string | undefined {
  const first = failed[0];
  if (first === undefined) return undefined;
  const reasons = new Set(failed.map(({ reason }) => reason));
  const head = `Could not snooze ${failed.length} of ${of}: ${first.reason}`;
  if (reasons.size === 1) return `${head}.`;
  const others = reasons.size - 1;
  return `${head}, and ${others} other reason${others === 1 ? "" : "s"}.`;
}

/** The selection of one Inbox list, and the two presses it feeds. */
export interface InboxSelection {
  /** Whether the reader pressed **Select all**, which takes in every row the narrowing matches,
   *  the ones a fold or a page is holding back included. The bar says so instead of counting. */
  readonly allRows: boolean;
  /** The bar's own element, which the keymap reaches through this: Escape from the bulk picker
   *  focuses the bar itself when a press has left no row to return to. */
  readonly barRef: RefObject<HTMLFieldSetElement | null>;
  /** Whether the bar shows: a mark is on the page, a pick is in the air, or a refusal is
   *  standing. Escape with no row focused is the keyboard's Clear and is offered on exactly this,
   *  so the two cannot drift. */
  readonly barShown: boolean;
  readonly clearSelection: () => void;
  /** Whether the reader pressed **Dismiss** on this row here and that press is still out, which
   *  is what the list's motion hold skips: a row they dismissed leaves the list at once even with
   *  their hand still on it. */
  readonly dismissedHere: (id: string) => boolean;
  /** One press over the threads the bar is counting. */
  readonly dismissPick: () => void;
  /** One press over the rows given, from a row's own control or from `e`: each row with what
   *  the page drew in it (`InboxDismissRow`). */
  readonly dismissRows: (rows: readonly InboxDismissRow[]) => void;
  /** The one dismiss write this list shares, for what it is doing and what it was refused. */
  readonly dismissWrite: InboxDismiss;
  /** Every row the reader has marked, by `rowId`, a folded one's included. */
  readonly marked: ReadonlySet<string>;
  readonly onMark: (id: string) => void;
  readonly selectAll: () => void;
  /** The marked asks the view is drawing, which is what **Snooze** defers. */
  readonly selectedAsks: readonly string[];
  /** The thread rows a **Dismiss** press names, each with what the page drew in it, which is
   *  what the bar counts for that press. */
  readonly selectedThreads: readonly InboxDismissRow[];
  readonly snoozePick: (until: string) => void;
  /** What the reader is told about the last bulk snooze the server refused in part. */
  readonly snoozeRefusal: string | undefined;
  /** How many asks the snooze now in flight covers, 0 when none is: the optimistic fold empties
   *  the selection the bar would otherwise count. */
  readonly snoozing: number;
}

/**
 * One list's selection: the marks, **Select all**, and the Snooze and Dismiss the bar presses.
 *
 * **Select all** and the marks differ in what a press covers. A mark the reader cannot see is
 * not acted on - a row a fold or a page is holding back is not on the page, and they may well
 * have set its own moment since marking it - so the marks a press writes are the ones this view
 * is drawing. **Select all** is the other promise: every row the narrowing matches. The Inbox's
 * own thread read is unpaged, so one Dismiss under it names every matching thread id, the rows
 * behind **Show more** and inside a folded band with them (A12); only the paged Dismissed view
 * has ids it has not loaded, and there the press names the list's own narrowing instead (`asOf`).
 * Snooze is unchanged by **Select all**: it defers the marked asks the view is drawing, never a
 * moment the reader set on a row they cannot see.
 */
export function useInboxSelection({
  asOf,
  candidates,
  drawnIds,
  filters,
  rows,
  showDismissed,
}: {
  /** The read mark a press that names the list's own narrowing carries, or undefined where no
   *  press may name it: the server's narrowing holds the five filter fields and knows nothing of
   *  the text typed in the search box. */
  readonly asOf: string | undefined;
  /** Every row the two reads answered, before the filters and the typed text narrow them: a mark
   *  whose row the list no longer holds at all drops against these. */
  readonly candidates: readonly InboxListRow[];
  /** The rows this view is drawing, by `rowId`: a folded band's rows and the ones behind **Show
   *  more** are not among them. */
  readonly drawnIds: ReadonlySet<string>;
  /** The list's own five fields, which a press under the narrowing sends. */
  readonly filters: InboxFilters;
  /** Every row the narrowing matches, by `rowId`, in the order the list holds them. */
  readonly rows: ReadonlyMap<string, InboxListRow>;
  /** Whether the list is showing what the reader dismissed, where a press puts rows back. */
  readonly showDismissed: boolean;
}): InboxSelection {
  // The rows the reader has marked, by `rowId` - asks and threads alike: `x` and the row's own
  // checkbox are the two ways in. An id the list no longer holds drops out here, so a row that is
  // listed again comes back unmarked; a mark whose row this view does not list, or whose band is
  // folded shut, drops out of `selected` below, so the bar never counts - and `h` never writes -
  // a row that is not on the page. The mark itself outlives both: open the band, or switch back,
  // and it is still made.
  const [marked, setMarked] = useState<ReadonlySet<string>>(() => new Set());
  // Whether the reader pressed **Select all**. Any change to the marks ends it.
  const [allRows, setAllRows] = useState(false);
  const bulkWrite = useAskSnoozeMany();
  const [snoozing, setSnoozing] = useState(0);
  const barRef = useRef<HTMLFieldSetElement>(null);
  const [snoozeRefusal, setSnoozeRefusal] = useState<string | undefined>(undefined);
  /** The rows the reader has pressed Dismiss on here, by `rowId`, while that press is out: the
   *  list's hand-hold skips them, so a row they dismissed leaves the list at once even with their
   *  hand still on it. An id leaves once its press settles - the rollback of a refused one puts
   *  an ordinary row back, and one the server took is out of the list - so the hold never stops
   *  covering a row the reader can still see. */
  const pressedHere = useRef<Set<string>>(new Set());
  // The one dismiss write every control on this list shares, the row's own and the bar's: the
  // write takes the row - and every control drawn on it - off the page, so its refusal is the
  // page's to show rather than the row's (`useInboxDismiss`).
  const dismissWrite = useInboxDismiss({
    onSettled: (ids) => {
      for (const id of ids) pressedHere.current.delete(id);
    },
  });
  const clearSelection = useCallback(() => {
    setMarked(new Set());
    setAllRows(false);
    setSnoozeRefusal(undefined);
  }, []);
  const onMark = useCallback((id: string) => {
    setAllRows(false);
    setMarked((current) => {
      const next = new Set(current);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  }, []);
  /** The marks the bar counts: the rows this view is drawing. */
  const selected = [...marked].filter((id) => drawnIds.has(id));
  const selectedAsks = selected.filter((id) => rows.get(id)?.kind === "ask");
  // Under **Select all** the press covers every thread row the narrowing matches, drawn or not:
  // the Inbox's read is unpaged, so these are all of them, and one request names them all, each
  // with the values its own row is drawn from (`dismissRow`).
  const selectedThreads: InboxDismissRow[] = [];
  for (const id of allRows ? [...rows.keys()] : selected) {
    const row = rows.get(id);
    const named = row === undefined ? undefined : dismissRow(row);
    if (named !== undefined) selectedThreads.push(named);
  }
  // One pick, over the ids the bar is counting: the same optimistic move and rollback the per-row
  // control makes, once per ask. The ids the server took leave the selection; the ones it refused
  // are marked again under its own reason, even if Clear ran while the write was in flight, so a
  // second pick retries exactly those.
  const snoozePick = (until: string) => {
    const ids = [...selectedAsks];
    setSnoozeRefusal(undefined);
    setSnoozing(ids.length);
    void bulkWrite.submit(ids, until).then(({ failed }) => {
      setSnoozing(0);
      setSnoozeRefusal(snoozeRefusalText(failed, ids.length));
      const refused = new Set(failed.map(({ askId }) => askId));
      const taken = new Set(ids.filter((id) => !refused.has(id)));
      setMarked((current) => {
        const next = new Set([...current].filter((id) => !taken.has(id)));
        for (const id of refused) next.add(id);
        return next;
      });
    });
  };
  /** One press of the reader's, from a row's own control, from `e`, or from the bar: the ids go
   *  into the hold's exception list first, so the rows leave the list under their hand. */
  const dismissRows = (named: readonly InboxDismissRow[]) => {
    for (const { id } of named) pressedHere.current.add(id);
    dismissWrite.submit({ restore: showDismissed, rows: [...named] });
  };
  // One press over the threads the bar is counting, through the one dismiss write. The ids are
  // in hand wherever the list that holds them is unpaged, which the Inbox's own read is, so the
  // press names them - a **Select all** of a band wider than a page included. The Dismissed view
  // is the one list the server pages, and there a **Select all** has ids it never loaded: that
  // press names the list's own narrowing and the mark the list was read at instead, as **Mark
  // all read** does, which is offered only where nothing is typed in the search box (`asOf`).
  const dismissPick = () => {
    if (allRows && showDismissed && asOf !== undefined) {
      for (const { id } of selectedThreads) pressedHere.current.add(id);
      dismissWrite.submit({ filters, restore: true, through: asOf });
    } else {
      dismissRows(selectedThreads);
    }
    // The rows leave the view at once, so the marks on them go with them; a refusal puts them
    // back under the line the page shows for it, not under a bar the reader must look for.
    clearSelection();
  };
  // A mark whose row the list no longer holds at all - dismissed, answered, resolved elsewhere -
  // drops out here, so a row that comes back is unmarked. Read against every row the two reads
  // answered, which is what the marks are keyed by.
  const candidateIds = candidates.map((row) => rowId(row)).join(" ");
  useEffect(() => {
    const listed = new Set(candidateIds === "" ? [] : candidateIds.split(" "));
    setMarked((current) =>
      [...current].every((id) => listed.has(id))
        ? current
        : new Set([...current].filter((id) => listed.has(id)))
    );
  }, [candidateIds]);

  return {
    allRows,
    barRef,
    // The bar outlives an empty list: a pick that empties this view leaves its write in the air,
    // and the refusal that comes back has to land somewhere the reader can see.
    barShown: selected.length > 0 || snoozing !== 0 || snoozeRefusal !== undefined,
    clearSelection,
    dismissedHere: (id) => pressedHere.current.has(id),
    dismissPick,
    dismissRows,
    dismissWrite,
    marked,
    onMark,
    selectAll: () => {
      setMarked(new Set(rows.keys()));
      setAllRows(true);
    },
    selectedAsks,
    selectedThreads,
    snoozePick,
    snoozeRefusal,
    snoozing,
  };
}
