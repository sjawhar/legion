import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { threadReadRequest } from "../../api/client";
import { myRepliesKey, myRepliesOwnerQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { type DwellRef, useDwell } from "../shell/useDwell";
import { useSendMarkOnce } from "../writes/useSendMarkOnce";
import { unreadWrites } from "./thread-layout";

/** The owner a thread lives on, as `GET /api/v1/me/replies` narrows to it: an issue, or a project
 *  document (an issue's documents are the issue's). */
export type RepliesOwner = { readonly issue: string } | { readonly artifact: string };

/** An ask's owner: its issue, else the project document it was asked on. */
export function askRepliesOwner(
  ask: Pick<Ask, "artifact_id" | "issue_key">
): RepliesOwner | undefined {
  if (ask.issue_key !== null) return { issue: ask.issue_key };
  return ask.artifact_id === null || ask.artifact_id === undefined
    ? undefined
    : { artifact: ask.artifact_id };
}

/** The owner query a disabled hook names: never fetched, since `enabled` is false. */
const NO_OWNER: RepliesOwner = { issue: "" };

export interface MarkThreadReadOptions {
  /** Whether the surface shows the thread's replies: a collapsed card marks nothing. */
  readonly enabled: boolean;
  readonly owner: RepliesOwner | undefined;
  /** The ask's id, or the comment or message thread's root id. */
  readonly threadId: string;
  /** The server timestamp of the newest reply the surface shows; with none there is nothing to
   *  read. */
  readonly newestShownAt: string | undefined;
  /** The thread's writes, as the surface holds them, for the unread set below; none while the
   *  thread is still loading, and none at all from a surface that wants no set. */
  readonly writes?: readonly { readonly id: string; readonly created_at: string }[];
}

/** What a thread's read mark answers: where the watch goes, and what the thread still holds
 *  unread. */
export interface ThreadRead {
  /** The watch, for the newest message's own element. */
  readonly ref: DwellRef;
  /** The ids of `writes` the reader has not read, by the one rule (`unreadWrites`), read from
   *  the same unread set the mark watches: empty while this surface marks nothing, while the
   *  thread is not in the set, and while it holds no writes of its own. */
  readonly unread: ReadonlySet<string>;
}

/**
 * Marks one thread read once the reader has seen its newest reply - the rule the issue's
 * Conversation, a document's margin, an ask's card and the Inbox's threads all read by: the
 * returned ref goes on that reply's own element, and once it has been three-quarters on screen for
 * a second (`useDwell`, whose one IntersectionObserver every thread on the page shares) the thread
 * is marked read through that reply's timestamp (`PUT /api/v1/me/threads/{id}/read`), once per
 * thread and timestamp (`useSendMarkOnce`). It watches the newest reply rather than the whole
 * thread, because a thread taller than the screen never has three-quarters of itself on screen.
 *
 * Only a thread in the owner's unread set is watched: every thread mounted on one owner's page
 * reads that set through one shared request, and a thread the viewer never wrote in is never
 * marked. Past the set's 200 rows a thread missing from it may still be unread, so then every
 * thread is watched and the server, which writes nothing for a thread without a row, decides.
 *
 * That one read is also where the thread's unread writes come from: the row it holds for this
 * thread, read by the one rule (`unreadWrites`), so no surface asks a second time to find out
 * which of the replies in front of the reader are new.
 */
export function useMarkThreadRead({
  enabled,
  newestShownAt,
  owner,
  threadId,
  writes,
}: MarkThreadReadOptions): ThreadRead {
  const queryClient = useQueryClient();
  // The routes need the `person` scope, which a signed-in browser holds; any other principal
  // never asks, so never cycles on a 403.
  const human = useIsHuman();
  const watching = enabled && human && owner !== undefined && newestShownAt !== undefined;
  const unreadSet = useQuery({ ...myRepliesOwnerQuery(owner ?? NO_OWNER), enabled: watching });
  const rows = unreadSet.data;
  // This thread's own row of that set, which says both that it is unread and what in it is.
  const row = watching
    ? rows?.rows.find((candidate) => candidate.thread_id === threadId)
    : undefined;
  // Whether this thread is one the mark watches at all.
  const watched =
    watching && rows !== undefined && (rows.total > rows.rows.length || row !== undefined);
  // The newest reply this surface saw dwell, for the thread it saw it in.
  const [seen, setSeen] = useState<{ readonly at: string; readonly threadId: string }>();
  const ref = useDwell({
    enabled: watched,
    onDwell: () => {
      if (newestShownAt !== undefined) setSeen({ at: newestShownAt, threadId });
    },
    resetKey: `${threadId} ${newestShownAt ?? ""}`,
  });
  useSendMarkOnce({
    mark:
      seen === undefined || seen.threadId !== threadId
        ? undefined
        : { key: `${threadId} ${seen.at}`, value: seen.at },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: myRepliesKey }),
    request: (readThrough: string) => threadReadRequest(threadId, readThrough),
    shouldSend: watched,
    target: `read:thread:${threadId}`,
    wake: unreadSet.dataUpdatedAt,
  });
  const unread = useMemo(() => unreadWrites(row, writes), [row, writes]);
  return { ref, unread };
}
