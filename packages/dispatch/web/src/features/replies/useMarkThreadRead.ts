import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api } from "../../api/client";
import { myRepliesKey, myRepliesOwnerQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useSendMarkOnce } from "../../lib/useSendMarkOnce";
import { type DwellRef, useDwell } from "../shell/useDwell";

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
}

/**
 * Marks one thread of the Replies page read once the reader has seen its newest reply: the
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
 */
export function useMarkThreadRead({
  enabled,
  newestShownAt,
  owner,
  threadId,
}: MarkThreadReadOptions): DwellRef {
  const queryClient = useQueryClient();
  // The routes are human-only; any other principal never asks, so never cycles on a 403.
  const human = useIsHuman();
  const watching = enabled && human && owner !== undefined && newestShownAt !== undefined;
  const unreadSet = useQuery({ ...myRepliesOwnerQuery(owner ?? NO_OWNER), enabled: watching });
  const rows = unreadSet.data;
  const unread =
    watching &&
    rows !== undefined &&
    (rows.total > rows.rows.length || rows.rows.some((row) => row.thread_id === threadId));
  // The newest reply this surface saw dwell, for the thread it saw it in.
  const [seen, setSeen] = useState<{ readonly at: string; readonly threadId: string }>();
  const ref = useDwell({
    enabled: unread,
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
    send: (readThrough: string) => api.markThreadRead(threadId, readThrough),
    shouldSend: unread,
    wake: unreadSet.dataUpdatedAt,
  });
  return ref;
}
