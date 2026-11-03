/**
 * What a thread draws, and in what order.
 *
 * Every surface that draws a thread - the Inbox's reading pane and its cards, an ask's thread, a
 * document block's thread, a document page's margin thread and the Conversation tab's threads -
 * lays its replies out by this one rule, so a thread reads the same way wherever it is drawn. The
 * order is the reader's own choice, made once in the Inbox's View menu and kept for their browser
 * (`preferences.ts`, `inbox.thread-order`); no other surface offers a control of its own.
 *
 * The rule: a collapsing surface shows the root and the two newest replies and folds the rest
 * behind **Show N earlier replies**, except that an unread reply is never folded, so the folded
 * set is the oldest replies that are both read and outside the newest two. **Oldest first** draws
 * the fold control under the root, then the replies oldest first, then the reply box; **Newest
 * first** draws the reply box under the root, then the replies newest first, then the fold
 * control. Either way the reply box - and an ask's options and answer box - sits at the end of
 * the chosen order, next to the newest message.
 */

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { myRepliesOwnerQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";
import { compareTimestamps } from "../../lib/timestamps";
import type { RepliesOwner } from "../replies/useMarkThreadRead";
import type { ThreadOrder } from "./preferences";

/** How many of the newest replies a collapsing surface always shows. */
const KEPT_REPLIES = 2;

/** The label of the control that opens a thread's fold, naming what it holds. */
export function earlierRepliesLabel(count: number): string {
  return `Show ${count} earlier ${count === 1 ? "reply" : "replies"}`;
}

export interface ThreadLayoutOptions<Reply> {
  /** Whether this surface folds older replies behind a control. The reading pane, a card, a
   *  document block's thread and an ask's thread do; a document page's margin thread and the
   *  Conversation tab's threads show every reply. */
  readonly collapse: boolean;
  readonly order: ThreadOrder;
  /** The thread's replies, oldest first, as every call site holds them. */
  readonly replies: readonly Reply[];
  /** The ids of the replies the reader has not read (`useUnreadReplies`): an unread reply is
   *  never folded away. Without it nothing is unread, which is what a surface that reads no
   *  unread set - the Inbox's own cards, whose rows carry the fact themselves - wants. */
  readonly unread?: ReadonlySet<string>;
}

export interface ThreadLayout<Reply> {
  /** The replies behind **Show N earlier replies**, in the chosen order: the oldest replies that
   *  are both read and outside the newest two. Empty where nothing folds. */
  readonly folded: readonly Reply[];
  /** The replies the thread always shows, in the chosen order. */
  readonly shown: readonly Reply[];
  /** Whether the reply box - and an ask's options and answer box - sits above the replies rather
   *  than below them: the box sits next to the newest message, so **Newest first** puts it under
   *  the root and **Oldest first** at the end. The fold control sits at the other end, furthest
   *  from the newest message. */
  readonly composerFirst: boolean;
}

const NOTHING: readonly never[] = [];

/** What a thread's drawer renders: which replies show, which fold, and where the reply box
 *  goes. */
export function threadLayout<Reply extends { readonly id: string }>({
  collapse,
  order,
  replies,
  unread,
}: ThreadLayoutOptions<Reply>): ThreadLayout<Reply> {
  const foldable = collapse ? replies.length - KEPT_REPLIES : 0;
  let foldedCount = 0;
  // Every reply from the oldest unread one on stays on screen, that unread one with it.
  while (foldedCount < foldable && unread?.has(replies[foldedCount].id) !== true) {
    foldedCount += 1;
  }
  const folded = foldedCount === 0 ? NOTHING : replies.slice(0, foldedCount);
  const shown = foldedCount === 0 ? replies : replies.slice(foldedCount);
  const newestFirst = order === "newest";
  return {
    composerFirst: newestFirst,
    folded: newestFirst ? [...folded].reverse() : folded,
    shown: newestFirst ? [...shown].reverse() : shown,
  };
}

/** The owner query a disabled hook names: never fetched, since `enabled` is false. */
const NO_OWNER: RepliesOwner = { issue: "" };

const NOTHING_UNREAD: ReadonlySet<string> = new Set();

export interface UnreadRepliesOptions<Reply> {
  /** Whether this surface reads the owner's unread set at all - the gate `useMarkThreadRead`
   *  takes, so a surface that marks nothing read starts no read of its own. */
  readonly enabled: boolean;
  readonly owner: RepliesOwner | undefined;
  /** The thread's replies, oldest first; none while the thread is still loading. */
  readonly replies: readonly Reply[] | undefined;
  /** The ask's id, or the comment or message thread's root id. */
  readonly threadId: string;
}

/**
 * Which of a thread's replies the reader has not read, from the unread set of the thread's owner
 * (`myRepliesOwnerQuery`): the one read `useMarkThreadRead` already watches, under the same
 * gates, so every thread on one owner's page shares one request and a surface that marks nothing
 * asks for nothing. A thread the set does not hold has nothing unread; a thread it holds with no
 * read mark is unread whole.
 */
export function useUnreadReplies<
  Reply extends { readonly id: string; readonly created_at: string },
>({ enabled, owner, replies, threadId }: UnreadRepliesOptions<Reply>): ReadonlySet<string> {
  // The routes are human-only; any other principal never asks, so never cycles on a 403.
  const human = useIsHuman();
  const watching = enabled && human && owner !== undefined;
  const unreadSet = useQuery({ ...myRepliesOwnerQuery(owner ?? NO_OWNER), enabled: watching });
  const rows = watching ? unreadSet.data?.rows : undefined;
  return useMemo(() => {
    const row = rows?.find((candidate) => candidate.thread_id === threadId);
    if (row === undefined || !row.unread || replies === undefined) return NOTHING_UNREAD;
    const mark = row.read_through;
    const unread = new Set<string>();
    for (const reply of replies) {
      if (mark === undefined || compareTimestamps(reply.created_at, mark) > 0) unread.add(reply.id);
    }
    return unread;
  }, [replies, rows, threadId]);
}
