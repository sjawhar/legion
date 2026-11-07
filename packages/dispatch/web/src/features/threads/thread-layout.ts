/**
 * What a thread draws, and in what order, and which of its writes are unread.
 *
 * Every surface that draws a thread - the Inbox's reading pane and its cards, an ask's thread, a
 * document block's thread, a document page's margin thread and the Conversation tab's threads -
 * lays its replies out by this one rule, so a thread reads the same way wherever it is drawn. The
 * order is the reader's own choice, made once in the Inbox's View menu and kept for their browser
 * (`thread-order.ts`); no other surface offers a control of its own.
 *
 * The rule: a collapsing surface shows the root and the two newest replies and folds the rest
 * behind **Show N earlier replies**, except that an unread reply is never folded, so the folded
 * set is the oldest replies that are both read and outside the newest two. **Oldest first** draws
 * the fold control under the root, then the replies oldest first, then the reply box; **Newest
 * first** draws the reply box under the root, then the replies newest first, then the fold
 * control. Either way the reply box - and an ask's options and answer box - sits at the end of
 * the chosen order, next to the newest message.
 *
 * Which replies are unread is the other half of that rule, and `unreadWrites` below is the one
 * place it is written: both are pure, so neither reads anything of its own - a surface hands in
 * the thread's row, from the Inbox's list or from the owner's unread set the thread read mark
 * already watches (`useMarkThreadRead.ts`).
 */

import { inboxThreadRow } from "@legion/contracts";

import type { MyReplyRow } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import type { ThreadOrder } from "./thread-order";

/** How many of the newest replies a collapsing surface always shows. */
const KEPT_REPLIES = 2;

/** The label of the control that opens a thread's fold, naming what it holds. */
export function earlierRepliesLabel(count: number): string {
  return `Show ${count} earlier ${count === 1 ? "reply" : "replies"}`;
}

/** Whether the reply box - and an ask's options and answer box - sits above the replies rather
 *  than below them. The box sits next to the newest message, so **Newest first** puts it under
 *  the root and **Oldest first** at the end; the fold control takes the other end. A surface
 *  that draws no replies of its own, and so calls no `threadLayout`, reads it here. */
export function composerFirst(order: ThreadOrder): boolean {
  return order === "newest";
}

export interface ThreadLayoutOptions<Reply> {
  /** Whether this surface folds older replies behind a control. The reading pane, a card, a
   *  document block's thread and an ask's thread do; a document page's margin thread and the
   *  Conversation tab's threads show every reply. */
  readonly collapse: boolean;
  readonly order: ThreadOrder;
  /** The thread's replies, oldest first, as every call site holds them. */
  readonly replies: readonly Reply[];
  /** The ids of the replies the reader has not read (`unreadWrites`): an unread reply is never
   *  folded away. Without it nothing is unread, which is what a surface with no row of its own
   *  to read the fact from wants. */
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
  const newestFirst = composerFirst(order);
  return {
    composerFirst: newestFirst,
    folded: newestFirst ? [...folded].reverse() : folded,
    shown: newestFirst ? [...shown].reverse() : shown,
  };
}

const NOTHING_UNREAD: ReadonlySet<string> = new Set();

/**
 * Which writes of a thread the reader has not read: the one rule every surface reads it by, over
 * the thread's own row of `GET /api/v1/me/replies` (the Inbox's list, or the owner's unread set
 * `useMarkThreadRead` watches). An Agent row names its unread replies by id. Every other row
 * carries the reader's mark on the thread, so a write somebody made after that mark is one they
 * have not read - and a row the server calls read holds nothing unread at all, whatever its
 * mark says.
 *
 * `turns` are the thread's writes as the surface holds them, the root with its replies or the
 * replies alone; none while the thread is still loading.
 */
export function unreadWrites<Turn extends { readonly id: string; readonly created_at: string }>(
  row: MyReplyRow | undefined,
  turns: readonly Turn[] | undefined
): ReadonlySet<string> {
  if (row === undefined || !row.unread) return NOTHING_UNREAD;
  const thread = inboxThreadRow(row);
  if (thread.kind === "agent") return new Set(thread.unread_reply_ids);
  if (turns === undefined) return NOTHING_UNREAD;
  const mark = thread.read_through;
  const unread = new Set<string>();
  for (const turn of turns) {
    if (compareTimestamps(turn.created_at, mark) > 0) unread.add(turn.id);
  }
  return unread;
}
