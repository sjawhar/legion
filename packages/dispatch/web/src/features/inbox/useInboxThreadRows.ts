import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { type RefObject, useMemo } from "react";

import { dismissedThreadsQuery, inboxThreadsQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";
import type { InboxShow } from "../refs/routes";
import type { InboxListRow } from "./list-row";
import { useMotionHold } from "./motion-hold";

/** The thread side of the Inbox: the rows it lists, the mark a write over that list carries, and
 *  the page behind **Show more** where the list is paged. */
export interface InboxThreadRows {
  /** The read mark of the list read these rows came from, which **Mark all read** and a
   *  **Select all** dismiss both carry. */
  readonly asOf: string | undefined;
  readonly rows: readonly InboxListRow[];
  /** Reads the next page of the Dismissed view, or undefined where there is none to read: the
   *  Inbox's own list is unpaged, and the last dismissed page names no next cursor. */
  readonly showMore: (() => void) | undefined;
  /** Whether the page **Show more** asks for is already on its way. */
  readonly showingMore: boolean;
}

/**
 * The reader's own threads as Inbox rows: the unpaged read of their Inbox, the paged read of what
 * they have dismissed out of it, and the pointer hold over whichever is showing.
 *
 * A thread stays in the Inbox, read or not, until it is dismissed, so the Inbox read answers for
 * every row the list draws and reading one takes nothing away. The Dismissed read answers newest
 * dismissal first, `INBOX_DISMISSED_PAGE` rows at a time, and **Show more** follows its cursor.
 */
export function useInboxThreadRows({
  asksOnly,
  listRef,
  show,
}: {
  /** Whether this list is narrowed to asks (an agent filter, or `?section=needs-you`), which
   *  lists no thread row at all. */
  asksOnly: boolean;
  /** The list the pointer travels across, which the motion hold watches. */
  listRef: RefObject<HTMLElement | null>;
  /** Which of the reader's two lists is showing. */
  show: InboxShow;
}): InboxThreadRows {
  // The reader's own threads, in one unpaged read. The route needs the `person` scope, which a
  // signed-in browser holds, so the list only asks once the sign-in read says the principal is a
  // user; neither read is made for the view that is not showing.
  const human = useIsHuman();
  const threads = useQuery({
    ...inboxThreadsQuery(),
    enabled: human && !asksOnly && show === "inbox",
  });
  const dismissed = useInfiniteQuery({
    ...dismissedThreadsQuery(),
    enabled: human && !asksOnly && show === "dismissed",
  });
  const pages = dismissed.data?.pages;
  // One array per set of pages, and the same array until they change: the motion hold adopts what
  // it is given by identity (`motion-hold.ts`), so a fresh `flatMap` on every render would have it
  // adopt - and so re-render - for ever. The Inbox read above hands over its own `rows`, which the
  // query cache keeps for it.
  const paged = useMemo(() => pages?.flatMap((page) => page.rows ?? []), [pages]);
  const read = show === "dismissed" ? paged : (threads.data?.rows ?? undefined);
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adopted = useMotionHold(read, listRef);
  const listed = asksOnly ? [] : (adopted ?? []);
  return {
    // The mark of the read these rows came from: the Dismissed view's is its first page's, the
    // one snapshot its cursor walks on from.
    asOf: show === "dismissed" ? pages?.[0]?.as_of : threads.data?.as_of,
    rows: listed.map((thread): InboxListRow => ({ kind: "thread", thread })),
    showMore:
      show === "dismissed" && dismissed.hasNextPage
        ? () => void dismissed.fetchNextPage()
        : undefined,
    showingMore: dismissed.isFetchingNextPage,
  };
}
