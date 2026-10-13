import type { QueryClient } from "@tanstack/react-query";

import { myRepliesOwnerQuery, whoAmIQuery } from "../../api/queries";
import type { MyReplyRow } from "../../api/types";
import type { RepliesOwner } from "./useMarkThreadRead";

/** One unread row on the Replies page for `threadId`, as `GET /api/v1/me/replies` answers it. */
export function replyRow(threadId: string, overrides: Partial<MyReplyRow> = {}): MyReplyRow {
  return {
    issue: { key: "CORE-1", title: "Core" },
    kind: "comment",
    ref: `/issues/CORE-1/comments/${threadId}`,
    reply: {
      author: { id: "s1", kind: "session" },
      created_at: "2026-10-12T00:00:00Z",
      first_line: "Done.",
      id: `${threadId}-reply`,
      kind: "reply",
    },
    subject: "Root",
    thread_id: threadId,
    ...overrides,
  };
}

/** Seeds the signed-in viewer (alice) and `owner`'s unread threads, so every thread mounted on
 *  that owner reads them from the cache rather than a request. */
export function seedUnread(
  queryClient: QueryClient,
  owner: RepliesOwner,
  threadIds: readonly string[]
): void {
  queryClient.setQueryData(whoAmIQuery().queryKey, { kind: "user", login: "alice" });
  queryClient.setQueryData(myRepliesOwnerQuery(owner).queryKey, {
    limit: 200,
    offset: 0,
    rows: threadIds.map((id) => replyRow(id)),
    total: threadIds.length,
  });
}
