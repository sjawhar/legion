import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type { MessageRead, UserAgentState, UserAgentStates } from "../../api/types";

/** The badge an unread count wears wherever it shows: the navigation, the compact header, and
 *  the agent's row. */
export function unreadRepliesLabel(count: number): string {
  return count === 1 ? "New reply 1" : `New replies ${count}`;
}

/** Every session's unread replies to the viewer's direct messages, summed. An API older than the
 *  count answers a state without it, which counts as none. */
export function totalUnreadReplies(states: UserAgentStates | undefined): number {
  return Object.values(states ?? {}).reduce(
    (total, state) => total + (state.unread_replies ?? 0),
    0
  );
}

/** The newest reply `sessionId` wrote anywhere in `exchanges`, by its own (the server's)
 *  timestamp. */
function newestSessionReply(
  exchanges: readonly MessageRead[],
  sessionId: string
): string | undefined {
  let newest: string | undefined;
  for (const read of exchanges) {
    for (const reply of read.replies) {
      if (reply.author.id !== sessionId) continue;
      if (newest === undefined || Date.parse(reply.created_at) > Date.parse(newest)) {
        newest = reply.created_at;
      }
    }
  }
  return newest;
}

/**
 * The conversations the server marked unread when this view opened, by root message id. The
 * server's own flag is the only unread verdict a view renders (`unreadDirectRepliesCTE` is where
 * it is defined), and it is frozen here: the view's own read mark clears the flag a moment later,
 * and an exchange shown because it held an unread reply has to stay shown while the viewer reads
 * it. Undefined until the conversations have loaded.
 */
export function useUnreadAtOpen(
  exchanges: readonly MessageRead[] | undefined
): ReadonlySet<string> | undefined {
  const [frozen, setFrozen] = useState<ReadonlySet<string> | undefined>(undefined);
  if (frozen === undefined && exchanges !== undefined) {
    const unread = new Set(
      exchanges.filter((read) => read.unread === true).map((read) => read.message.id)
    );
    setFrozen(unread);
    return unread;
  }
  return frozen;
}

/** Puts the session's state a PUT answered with into the viewer's shared agent-state query, so
 *  every badge and row reads it at once. */
export function storeAgentState(
  queryClient: QueryClient,
  sessionId: string,
  next: UserAgentState
): void {
  queryClient.setQueryData<UserAgentStates>(userAgentStateQuery().queryKey, (current) => ({
    ...current,
    [sessionId]: next,
  }));
}

/**
 * Records that the viewer has read a session's conversation, through the newest reply the
 * session wrote in `exchanges`, whenever the server counts one unread. Call it only where the
 * conversation is on screen: the expanded agent row and the live view. The mark is the reply's
 * own timestamp (the server's clock), so a browser clock that is off cannot leave a reply unread
 * or mark one read before it arrived.
 */
export function useMarkRepliesRead(
  sessionId: string,
  exchanges: readonly MessageRead[] | undefined
): void {
  const queryClient = useQueryClient();
  const states = useQuery(userAgentStateQuery());
  const unread = states.data?.[sessionId]?.unread_replies ?? 0;
  const newest = newestSessionReply(exchanges ?? [], sessionId);
  // The reply this view last sent a read mark for, so a re-render does not send it again. A
  // failed write is retried twice with backoff; if it still fails the mark is cleared, so it is
  // sent again when the unread count or the newest reply next changes or the view is reopened,
  // rather than the badge staying up until the session replies once more.
  const marked = useRef<string | undefined>(undefined);
  const { mutate } = useMutation({
    mutationFn: (readThrough: string) =>
      api.putAgentState(sessionId, { read_through: readThrough }),
    onError: () => {
      marked.current = undefined;
    },
    onSuccess: (next) => storeAgentState(queryClient, sessionId, next),
    retry: 2,
  });
  useEffect(() => {
    if (unread === 0 || newest === undefined || marked.current === newest) return;
    marked.current = newest;
    mutate(newest);
  }, [mutate, newest, unread]);
}
