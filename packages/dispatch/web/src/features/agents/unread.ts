import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type {
  Message,
  MessageRead,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";

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
      // A session id and a person's email are disjoint namespaces, so the id test implies the kind.
      // The server's rule tests the kind and this one tests the id; a reader restoring the
      // symmetry by dropping the other side's clause would break it.
      if (reply.author.id !== sessionId) continue;
      if (newest === undefined || compareTimestamps(reply.created_at, newest) > 0) {
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

/** One read mark a view sent: the key it was sent for and the body that carried it. */
interface SentMark {
  body: UserAgentStateInput;
  key: string;
}

/**
 * The write both read hooks make: `input(value)` for `sessionId`, once per `dedupeKey(value)`,
 * whenever the server counts one of the session's replies unread, with the answer put into the
 * shared agent-state query. The key, not the value, decides when to write: the effect reruns when
 * the key or the unread count changes, never on a value a caller rebuilds on every
 * render. A failed write is retried twice with backoff; if it still fails while it is the view's
 * latest write, its key is forgotten, so it is sent again when the unread count or the key next
 * changes or the view is reopened, rather than the badge staying up until the session replies
 * once more. Rerunning on the value would instead resend on every render after a failure, for as
 * long as the server kept refusing. A write that a newer key replaced while it was retrying
 * forgets nothing when it fails: the newer write stands for the view, and forgetting its key would
 * send it once more at the next change in the count.
 */
function useSendReadMark<V>(
  sessionId: string,
  value: V | undefined,
  dedupeKey: (value: V) => string,
  input: (value: V) => UserAgentStateInput
): void {
  const queryClient = useQueryClient();
  const unread = useQuery(userAgentStateQuery()).data?.[sessionId]?.unread_replies ?? 0;
  // The mark this view last sent, so a re-render does not send its key again.
  const sent = useRef<SentMark | undefined>(undefined);
  const { mutate } = useMutation({
    mutationFn: (mark: SentMark) => api.putAgentState(sessionId, mark.body),
    onError: (_error, mark) => {
      if (sent.current === mark) sent.current = undefined;
    },
    onSuccess: (next) => storeAgentState(queryClient, sessionId, next),
    retry: 2,
  });
  const key = value === undefined ? undefined : dedupeKey(value);
  // This render's value, which the effect reads when the key calls for a write.
  const latest = useRef(value);
  latest.current = value;
  useEffect(() => {
    const current = latest.current;
    if (unread === 0 || current === undefined || key === undefined || sent.current?.key === key) {
      return;
    }
    const mark = { body: input(current), key };
    sent.current = mark;
    mutate(mark);
  }, [input, key, mutate, unread]);
}

const ownString = (value: string): string => value;

const readThrough = (newest: string): UserAgentStateInput => ({ read_through: newest });

const joinedIds = (ids: readonly string[]): string => ids.join(" ");

const readReplies = (ids: readonly string[]): UserAgentStateInput => ({ read_replies: ids });

/**
 * Records that the viewer has read a session's conversation, through the newest reply the
 * session wrote in `exchanges`, whenever the server counts one unread. Call it only where the
 * whole conversation is on screen: the expanded agent row and the live view. The mark is the
 * reply's own timestamp (the server's clock), so a browser clock that is off cannot leave a reply
 * unread or mark one read before it arrived.
 */
export function useMarkRepliesRead(
  sessionId: string,
  exchanges: readonly MessageRead[] | undefined
): void {
  useSendReadMark(
    sessionId,
    newestSessionReply(exchanges ?? [], sessionId),
    ownString,
    readThrough
  );
}

/**
 * Records that the viewer has read the replies `sessionId` wrote among `replies`, by their ids,
 * whenever the server counts one of the session's replies unread. Call it where a view shows only
 * some of a session's replies, as the broadcast page shows each recipient's reply to that
 * broadcast alone: unlike `useMarkRepliesRead`'s mark, which covers every reply up to a moment,
 * this marks nothing else, so the session's older reply to another message, and any reply after
 * these, stay unread until a view shows them.
 */
export function useMarkShownRepliesRead(sessionId: string, replies: readonly Message[]): void {
  // The server's own test for an id it takes (`putUserAgentState`): a message whose author is the
  // session, by kind and id. It refuses the whole write over one id that fails it.
  const shown = replies
    .filter((reply) => reply.author.kind === "session" && reply.author.id === sessionId)
    .map((reply) => reply.id);
  useSendReadMark(sessionId, shown.length === 0 ? undefined : shown, joinedIds, readReplies);
}
