import { type RefObject, useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import type { InboxRow } from "../../api/types";
import { buildInboxPath, type InboxRoute } from "../refs/routes";
import { answerDraft } from "./answer-drafts";
import { ROW_ATTRIBUTE } from "./inbox-row";
import { answerOutstanding } from "./outstanding-answers";
import type { InboxSection } from "./sections";

/** A row as the reader last saw it: the section it sat in, whatever the server says now. */
export interface PlacedRow {
  ask: InboxRow;
  section: InboxSection;
}

export interface PaneSelectionOptions {
  /** How many rows a bulk snooze is deferring; zero while none is out. */
  bulkSnoozing: number;
  /** The address this list is on. The selection is written into its `?item=`. */
  filter: InboxRoute;
  /** The list's root, where the selected row's own element is looked up. */
  listRef: RefObject<HTMLElement | null>;
  /** Opens the folded band, for an `?item=` naming a row the reader deferred. */
  openFoldedBand: () => void;
  /** Whether the reading column is on. Without it there is no selection, the column shows
   *  nothing, and nothing is written to the address. */
  paneShown: boolean;
  /** The rows the reader can see, in the order they see them, as the list last rendered them.
   *  The list keeps this record for its own motion hold, and the selection is answered against
   *  exactly the rows in it. */
  presented: RefObject<readonly PlacedRow[]>;
  /** Ask ids whose own snooze write is out. */
  snoozing: ReadonlySet<string>;
}

export interface PaneSelection {
  /** The row the column is showing, or undefined while it shows none. */
  id: string | undefined;
  /** Selects a row, or nothing, and writes it to the address. */
  select: (askId: string | undefined) => void;
  /** The selection as it stands now, for a callback whose closure is older than it: an answer's
   *  own card holds the `onAnswered` of the render its write began in, and by the time the
   *  server records that answer the row has left the list and the selection has moved on. */
  selectedNow: RefObject<string | undefined>;
  /** The selected row's own element, or null while the list holds none. */
  selectedRow: () => HTMLElement | null;
  /** Selects the row `delta` from the selected one, in the order the reader sees: what `j`, `k`
   *  and the arrows do while the reading column is on, in place of roving the focus. */
  step: (delta: 1 | -1) => void;
  /**
   * Every row the list holds this commit, a folded band's included, and, from the rows the
   * reader can see, the question the column shows. Called once per commit, after `presented`
   * is written: where the selection would go next is read there, off the rows as they stand,
   * rather than when it is wanted.
   */
  showing: (listed: readonly PlacedRow[]) => InboxRow | undefined;
  /** Where the selection goes when the row it is on leaves the bands. */
  nextAfterSelected: RefObject<string | undefined>;
}

/**
 * The Inbox reading column's selection: which question the column shows, how the keys and the
 * address move it, and what keeps it on a question whose row has gone.
 *
 * The address holds the selection (`?item=`), so a reload or a link keeps the reader's place;
 * below the column's width, and in the drawer, there is no selection and nothing is written.
 */
export function usePaneSelection({
  bulkSnoozing,
  filter,
  listRef,
  openFoldedBand,
  paneShown,
  presented,
  snoozing,
}: PaneSelectionOptions): PaneSelection {
  const navigate = useNavigate();
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);
  const selectedNow = useRef<string | undefined>(undefined);
  selectedNow.current = selectedId;
  /** The `?item=` this list has already answered: without it, writing the selection to the
   *  address would read back as a reader's own navigation and select the row again, one step
   *  behind every `j`. */
  const shownItem = useRef<string | undefined>(undefined);

  /** Every row this list holds, a folded band's included: what `?item=` is answered against. */
  const listedRows = useRef<readonly PlacedRow[]>([]);
  /** The question the column is showing, as the list last held it, and whether it is being kept
   *  there past its row. */
  const paneSubject = useRef<{ ask: InboxRow; kept: boolean } | undefined>(undefined);
  // Where the selection goes when the row it is on leaves the bands: the next row the reader
  // can see after it, else the nearest above it, so the column always shows a question they
  // can answer. One rule for every way a row leaves - the reader's own answer, which takes it
  // at once below, their own snooze, which takes it once the write lands, and the server
  // withdrawing, resolving or answering the question elsewhere - and read on every commit
  // rather than when it is wanted, because a snooze folds its row into `Later` the moment they
  // pick: while that write is out the read is held, so where they are sent is read off the list
  // as it stood when they deferred. The commit that drops the row holds it for the same reason:
  // the row is gone from that commit's list, and where it was heading is exactly what the
  // settling effect is about to read this for.
  const nextAfterSelected = useRef<string | undefined>(undefined);

  const select = (askId: string | undefined): void => {
    setSelectedId(askId);
    selectedNow.current = askId;
    shownItem.current = askId;
    if (askId !== filter.item) {
      // Replace, never push: a reader walking the list with `j` would otherwise have to press
      // Back once per row to leave the page, the rule the assignee filter already holds to.
      // Selecting nothing clears `?item=` rather than leaving it on the row that has just
      // left: a reload reads the address, and an address naming a row the reader deferred
      // would open the folded band and put that question back in front of them.
      navigate(buildInboxPath({ ...filter, item: askId }), { replace: true });
    }
  };

  const step = (delta: 1 | -1): void => {
    const visible = presented.current;
    const at = visible.findIndex((placed) => placed.ask.id === selectedId);
    if (at !== -1) {
      const next = visible[at + delta];
      if (next !== undefined) select(next.ask.id);
      return;
    }
    // The column is holding a question past its row (their unsent answer, or one the server has
    // not decided yet), so there is no row to step from. The row that took its place is the one
    // `nextAfterSelected` named while the question still stood there, so stepping starts from
    // where it stood rather than from the top of the list.
    const stood = visible.findIndex((placed) => placed.ask.id === nextAfterSelected.current);
    const next =
      stood === -1
        ? visible[delta === 1 ? 0 : visible.length - 1]
        : visible[delta === 1 ? stood : stood - 1];
    if (next !== undefined) select(next.ask.id);
  };

  /** The row the column is showing, or null while it is showing none. Held to the selection,
   *  because the effect keeping that row on screen is the selection's and nothing else's. */
  const selectedRow = useCallback(
    () =>
      selectedId === undefined
        ? null
        : (listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${selectedId}"]`) ??
          null),
    [listRef, selectedId]
  );

  /**
   * What the column shows this commit, given the selected row among the ones the list renders
   * (none, where it renders none at all). Normally that row's question, which leaves the column
   * as its row leaves the list. Two things outlive the row, both of them the reader's own answer
   * to that question:
   *
   * Their unsent answer. A question whose row the server dropped while they had typed one into
   * its box stays in the column - the card draws its closed state, with that text under it -
   * until they select another row, so an answer they are in the middle of writing is never taken
   * off the screen under their hands.
   *
   * Their sent answer, until the server decides it (`outstanding-answers.ts`). `useAskAnswerForm`
   * drops the row the moment the answer goes out, and the column hands the selection on when the
   * server records it (`recordAnswered`), not when the page guesses it will: an answer the server
   * refuses leaves the question here with the card's own refusal and its Retry under what they
   * answered - the one surface that refusal is shown on - exactly as it does below this column's
   * width, where the reader's hand on the row holds it (`heldRow`). This is `snoozing`'s rule for
   * a deferred row, for the one write whose own optimistic update takes the row out of the Inbox
   * read rather than merely out of this view.
   *
   * A question the reader has neither typed nor sent an answer to is held by nothing, the first
   * row this list selected for them included.
   */
  const paneAsk = (listed: InboxRow | undefined): InboxRow | undefined => {
    const subject = paneSubject.current;
    if (!paneShown || selectedId === undefined) {
      paneSubject.current = undefined;
      return undefined;
    }
    if (listed !== undefined) {
      paneSubject.current = { ask: listed, kept: false };
      return listed;
    }
    if (subject === undefined || subject.ask.id !== selectedId) {
      paneSubject.current = undefined;
      return undefined;
    }
    const kept = subject.kept || answerOutstanding(selectedId) || answerDraft(selectedId) !== "";
    paneSubject.current = kept ? { ask: subject.ask, kept: true } : undefined;
    return kept ? subject.ask : undefined;
  };

  const showing = (listed: readonly PlacedRow[]): InboxRow | undefined => {
    listedRows.current = listed;
    const visible = presented.current;
    // Read here, where the rows stand as the reader sees them, and held while a snooze of the
    // selected row is out: that write folds its row into `Later` on the pick, so a read taken
    // after it would answer with the row above rather than the row that took its place. Held too
    // on a commit whose list no longer holds the selected row at all: that is the commit the
    // settling effect reads this on, and the answer it wants is the one taken while the row was
    // still there.
    if (selectedId === undefined) {
      nextAfterSelected.current = undefined;
    } else if (!snoozing.has(selectedId) && bulkSnoozing === 0) {
      const at = visible.findIndex(({ ask }) => ask.id === selectedId);
      if (at !== -1) {
        nextAfterSelected.current = (visible[at + 1] ?? visible[at - 1])?.ask.id;
      }
    }
    return paneAsk(visible.find(({ ask }) => ask.id === selectedId)?.ask);
  };

  // What the selection is, settled after each commit against the rows this list holds.
  //
  // The address names it, and is read as the reader's own move - the first visit, a link, a
  // reload - exactly while it says something this list has not already answered: an ask it holds
  // is selected (its band opened first, if the reader linked into the folded one: following the
  // link is what they asked for), and one it does not hold leaves the first visible row
  // selected and the address rewritten. Otherwise the selection is the reader's, and the address
  // follows it.
  useEffect(() => {
    // A list that holds nothing - still loading, failed, or genuinely empty - answers no
    // address: `?item=` is left unread until there are rows to read it against.
    if (!paneShown || listedRows.current.length === 0) {
      return;
    }
    const visible = presented.current;
    if (filter.item !== shownItem.current) {
      shownItem.current = filter.item;
      const wanted = listedRows.current.find((placed) => placed.ask.id === filter.item);
      if (wanted !== undefined) {
        setSelectedId(wanted.ask.id);
        if (!visible.some((placed) => placed.ask.id === wanted.ask.id)) openFoldedBand();
        return;
      }
    }
    if (visible.some((placed) => placed.ask.id === selectedId)) {
      if (selectedId !== filter.item) select(selectedId);
      return;
    }
    // A question the column is keeping for the reader's unsent answer keeps the selection with
    // it: the row has gone, the question has not, and only their next pick ends that.
    if (paneSubject.current?.kept === true) {
      return;
    }
    // The row the selection was on has left the bands - the reader deferred it, the server
    // took it away - so the selection goes where that row was heading and the address follows
    // it, down to naming no row at all when the list has none left to show.
    const next = nextAfterSelected.current;
    select(visible.some((placed) => placed.ask.id === next) ? next : visible[0]?.ask.id);
  });

  // The selected row is kept on screen as the reader walks the list: the column beside it shows
  // the row they cannot see otherwise.
  useEffect(() => {
    if (!paneShown || selectedId === undefined) {
      return;
    }
    selectedRow()?.scrollIntoView?.({ block: "nearest" });
  }, [paneShown, selectedId, selectedRow]);

  return {
    id: selectedId,
    nextAfterSelected,
    select,
    selectedNow,
    selectedRow,
    showing,
    step,
  };
}
