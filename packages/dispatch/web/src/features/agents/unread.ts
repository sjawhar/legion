import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type { MessageRead, UserAgentStates } from "../../api/types";

/** The badge an unread count wears wherever it shows: the navigation, the compact header, and
 *  the agent's row. */
export function unreadRepliesLabel(count: number): string {
  return count === 1 ? "New reply 1" : `New replies ${count}`;
}

/** Every session's unread replies to the viewer's direct messages, summed. */
export function totalUnreadReplies(states: UserAgentStates | undefined): number {
  return Object.values(states ?? {}).reduce((total, state) => total + state.unread_replies, 0);
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
  let newest: string | undefined;
  for (const read of exchanges ?? []) {
    for (const reply of read.replies) {
      if (reply.author.kind !== "session" || reply.author.id !== sessionId) continue;
      if (newest === undefined || Date.parse(reply.created_at) > Date.parse(newest)) {
        newest = reply.created_at;
      }
    }
  }
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
    onSuccess: (next) =>
      queryClient.setQueryData<UserAgentStates>(userAgentStateQuery().queryKey, (current) => ({
        ...current,
        [sessionId]: next,
      })),
    retry: 2,
  });
  useEffect(() => {
    if (unread === 0 || newest === undefined || marked.current === newest) return;
    marked.current = newest;
    mutate(newest);
  }, [mutate, newest, unread]);
}
