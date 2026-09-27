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
  const marked = useRef<string | undefined>(undefined);
  const { mutate } = useMutation({
    mutationFn: (readThrough: string) =>
      api.putAgentState(sessionId, { read_through: readThrough }),
    onSuccess: (next) =>
      queryClient.setQueryData<UserAgentStates>(userAgentStateQuery().queryKey, (current) => ({
        ...current,
        [sessionId]: next,
      })),
  });
  useEffect(() => {
    if (unread === 0 || newest === undefined || marked.current === newest) return;
    marked.current = newest;
    mutate(newest);
  }, [mutate, newest, unread]);
}
