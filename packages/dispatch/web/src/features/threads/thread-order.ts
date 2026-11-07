/**
 * Which way a thread's replies read: oldest first, as a conversation is written, or newest
 * first, as a list of what has just happened.
 *
 * One choice for every thread in Dispatch - the Inbox's reading column and its cards, an ask's
 * thread, a document block's thread, a document page's margin thread and the Conversation tab's
 * threads, all of which lay their replies out by it (`thread-layout.ts`) - made once in the
 * Inbox's View menu, the only control over it, and kept for the reader's browser. The stored key
 * is still `inbox.thread-order`: it is the reader's own choice, not worth dropping to a rename.
 */

import { useUserPreference } from "../shell/userPreference";

export const THREAD_ORDERS = ["oldest", "newest"] as const;
export type ThreadOrder = (typeof THREAD_ORDERS)[number];

/** What each order is called in the View menu. */
export const THREAD_ORDER_LABELS: Record<ThreadOrder, string> = {
  newest: "Newest first",
  oldest: "Oldest first",
};

/** The reader's thread order, kept for their browser. */
export function useThreadOrder(): [ThreadOrder, (next: ThreadOrder) => void] {
  return useUserPreference<ThreadOrder>(
    "inbox.thread-order",
    (stored) => (stored === "newest" ? "newest" : "oldest"),
    (value) => value
  );
}
