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

/** The newest reply `sessionId` wrote in an exchange, by its own (the server's) timestamp. */
function newestSessionReply(read: MessageRead, sessionId: string): string | undefined {
  let newest: string | undefined;
  for (const reply of read.replies) {
    if (reply.author.kind !== "session" || reply.author.id !== sessionId) continue;
    if (newest === undefined || Date.parse(reply.created_at) > Date.parse(newest)) {
      newest = reply.created_at;
    }
  }
  return newest;
}

/** How far the viewer has read a session's conversation: the later of their read mark and their
 *  Clear. A reply after it is one the server counts unread. */
function readWatermark(state: UserAgentState | undefined): number {
  return Math.max(
    state?.cleared_before === undefined
      ? Number.NEGATIVE_INFINITY
      : Date.parse(state.cleared_before),
    state?.read_through === undefined ? Number.NEGATIVE_INFINITY : Date.parse(state.read_through)
  );
}

/**
 * The viewer's read watermark for a session as it stood when the conversation opened, or
 * undefined until the viewer's state has loaded. It does not move while the conversation stays
 * open, so an exchange shown because it held an unread reply stays shown once that reply is
 * marked read.
 */
export function useWatermarkAtOpen(sessionId: string): number | undefined {
  const states = useQuery(userAgentStateQuery());
  const [frozen, setFrozen] = useState<number | undefined>(undefined);
  if (frozen === undefined && states.data !== undefined) {
    const watermark = readWatermark(states.data[sessionId]);
    setFrozen(watermark);
    return watermark;
  }
  return frozen;
}

/**
 * Whether an exchange holds a reply the server counts unread for the viewer: it is one of the
 * viewer's own direct messages to the session (issue-less, by their login in any casing, as the
 * server matches it) and the session replied in it after `watermark`.
 */
export function holdsUnreadReply(
  read: MessageRead,
  sessionId: string,
  viewerLogin: string | undefined,
  watermark: number
): boolean {
  if (
    viewerLogin === undefined ||
    read.message.issue_key !== null ||
    read.message.author.kind !== "user" ||
    read.message.author.id.toLowerCase() !== viewerLogin.toLowerCase()
  ) {
    return false;
  }
  const newest = newestSessionReply(read, sessionId);
  return newest !== undefined && Date.parse(newest) > watermark;
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
  let newest: string | undefined;
  for (const read of exchanges ?? []) {
    const reply = newestSessionReply(read, sessionId);
    if (reply !== undefined && (newest === undefined || Date.parse(reply) > Date.parse(newest))) {
      newest = reply;
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
    onSuccess: (next) => storeAgentState(queryClient, sessionId, next),
    retry: 2,
  });
  useEffect(() => {
    if (unread === 0 || newest === undefined || marked.current === newest) return;
    marked.current = newest;
    mutate(newest);
  }, [mutate, newest, unread]);
}
