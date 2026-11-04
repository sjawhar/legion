import { useQuery } from "@tanstack/react-query";
import type { RefObject } from "react";

import { inboxAllThreadCountQuery, inboxThreadsQuery } from "../../api/queries";
import type { MyReplyRow } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { type InboxListRow, rowId } from "./list-row";
import { useMotionHold } from "./motion-hold";

/** The thread side of the Inbox: the rows it lists, and the two numbers the **Show read
 *  threads** switch carries. */
export interface InboxThreadRows {
  /** How many threads there are in all, the other side of the switch; undefined until the read
   *  that answers for it has landed, and while nothing has asked for it. */
  readonly allThreadCount: number | undefined;
  /** The read mark of the list read these rows came from, which **Mark all read** carries. */
  readonly asOf: string | undefined;
  readonly rows: readonly InboxListRow[];
  /** How many of the reader's threads are unread, whether or not this list shows them. */
  readonly unreadThreadCount: number;
}

/**
 * The reader's own threads as Inbox rows: the unpaged read, the pointer hold over it, the rows
 * the reader read in place that the read no longer holds, and the switch's two counts.
 *
 * All of it in one place because the four answer together: the list the reader sees is the read
 * plus what they read in front of it, and the counts beside the switch are read from whichever
 * side of it knows.
 */
export function useInboxThreadRows({
  asksOnly,
  listRef,
  readHeld,
  state,
  viewOpened,
}: {
  /** Whether this list is narrowed to asks (an agent filter, or `?section=needs-you`), which
   *  lists no thread row at all; the counts beside the switch are still the reader's own. */
  asksOnly: boolean;
  /** The list the pointer travels across, which the motion hold watches. */
  listRef: RefObject<HTMLElement | null>;
  /** The threads the reader read where they stand (`pane-selection.ts`). */
  readHeld: ReadonlyMap<string, MyReplyRow>;
  /** Which threads the list asks for: the unread ones, or every one of them. */
  state: "all" | "unread";
  /** Whether the reader has opened the View menu, the only place the count below is drawn. */
  viewOpened: boolean;
}): InboxThreadRows {
  // The reader's own threads, in one unpaged read: Unread by default, every one of them with
  // the switch on. Human-only, as the route is.
  const human = useIsHuman();
  const threads = useQuery({ ...inboxThreadsQuery(state), enabled: human });
  // The count the switch's other side carries. With the switch on, the unpaged read above
  // counts every thread and the unread ones are the rows that say so; with it off, that read
  // counts only the unread, so how many there are in all is a count-only read of its own. It
  // is the costliest shape this route answers, and the View menu's open panel is the only
  // place its number is drawn, so nothing asks for it until the reader has opened that menu.
  const allThreads = useQuery({
    ...inboxAllThreadCountQuery(),
    enabled: human && state !== "all" && viewOpened,
  });
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adopted = useMotionHold(threads.data?.rows, listRef);

  // The thread rows the list draws: what the server last answered, plus every row the reader
  // read in place that answer no longer holds (`readHeld`). A row read while the column shows
  // it, or read as a card where there is no column, stays where it was read and is drawn as
  // read, so neither the next list read nor a live refresh moves the list under them.
  const listed = asksOnly ? [] : (adopted ?? []);
  const listedIds = new Set(listed.map((thread) => rowId({ kind: "thread", thread })));
  const rows: InboxListRow[] = [
    ...listed.map((thread): InboxListRow => ({ kind: "thread", thread })),
    ...(asksOnly
      ? []
      : [...readHeld]
          .filter(([id]) => !listedIds.has(id))
          .map(([, thread]): InboxListRow => ({ kind: "thread", thread }))),
  ];
  // The two numbers the **Show read threads** switch carries, each read from the side of it
  // that knows: the unpaged read counts its own state's rows, and the count-only read above
  // answers for the other.
  const read = adopted ?? [];
  return {
    allThreadCount: state === "all" ? threads.data?.total : allThreads.data,
    asOf: threads.data?.as_of,
    rows,
    unreadThreadCount:
      state === "all" ? read.filter((thread) => thread.unread).length : read.length,
  };
}
