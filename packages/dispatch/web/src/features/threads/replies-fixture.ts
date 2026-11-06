import type { QueryClient } from "@tanstack/react-query";

import { myRepliesOwnerQuery, whoAmIQuery } from "../../api/queries";
import type { MyReplyAgentRow, MyReplyThreadRow } from "../../api/types";
import type { RepliesOwner } from "./useMarkThreadRead";

/** One unread thread row for `threadId`, as `GET /api/v1/me/replies` answers it: a Reply row,
 *  read through a mark older than its reply, at a version of its own - not 0, so a test cannot
 *  pass on a missing field. An Agent row is a shape of its own, which the tests that need one
 *  build themselves. */
export function replyRow(
  threadId: string,
  overrides: Partial<MyReplyThreadRow> = {}
): MyReplyThreadRow {
  return {
    issue: { key: "CORE-1", title: "Core" },
    kind: "comment",
    read_through: "2026-10-11T00:00:00Z",
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
    unread: true,
    unread_count: 1,
    version: 3,
    ...overrides,
  };
}

/** One unread Agent row for `threadId`, the other shape the route answers: a message the reader
 *  sent a session, with that session's reply. It is on no issue and in no document, so it
 *  carries no owner, no priority, no project and no read mark. */
export function agentRow(
  threadId: string,
  overrides: Partial<MyReplyAgentRow> = {}
): MyReplyAgentRow {
  const { reply, subject, unread, unread_count } = replyRow(threadId);
  return {
    kind: "agent",
    ref: "/agents/s1/live",
    reply,
    session: { id: "s1", name: "Planner" },
    subject,
    thread_id: threadId,
    unread,
    unread_count,
    unread_reply_ids: [reply.id],
    ...overrides,
  };
}

/** The Agent row the Inbox's own tests draw: the row above as a message the reader sent the
 *  planner session, answered by that session, with the one reply they have not read
 *  (`reply-1`). */
export function plannerAgentRow(overrides: Partial<MyReplyAgentRow> = {}): MyReplyAgentRow {
  return agentRow("message-1", {
    ref: "/agents/planner/live",
    reply: {
      author: { id: "planner", kind: "session" },
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Looked at it.",
      id: "reply-1",
      kind: "reply",
    },
    session: { id: "planner", name: "Planner" },
    subject: "Have a look at the build",
    unread_reply_ids: ["reply-1"],
    ...overrides,
  });
}

/** Seeds the signed-in viewer (alice) and `owner`'s unread threads, so every thread mounted on
 *  that owner reads them from the cache rather than a request. */
export function seedUnread(
  queryClient: QueryClient,
  owner: RepliesOwner,
  threadIds: readonly string[]
): void {
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  queryClient.setQueryData(myRepliesOwnerQuery(owner).queryKey, {
    as_of: "2026-10-12T00:00:00Z",
    limit: 200,
    offset: 0,
    rows: threadIds.map((id) => replyRow(id)),
    total: threadIds.length,
  });
}
