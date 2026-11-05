/**
 * What reading a thread in the Inbox does: the mark it writes, and the row it leaves exactly where
 * it was, drawn as read (INFRA-450). Only a dismiss takes a row out of the list.
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import {
  type IntersectionControl,
  installIntersectionObserver,
} from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerAgentStates, answerThreadReads } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import {
  inboxQuery,
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesOwnerQuery,
  userAgentStateQuery,
  whoAmIQuery,
} from "../../api/queries";
import type {
  Agent,
  Comment,
  IssueDetails,
  Message,
  MessageRead,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { Inbox } from "./Inbox";
import { ThreadRowCard } from "./ThreadRowCard";

const AS_OF = "2026-10-12T02:00:00Z";
const NEWEST = "2026-10-12T01:00:00Z";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
  window.localStorage.clear();
});

function spy<K extends keyof typeof api>(name: K, value: unknown): void {
  restores.push(spyOn(api, name).mockResolvedValue(value as never));
}

function commentRow(threadId: string, subject: string): MyReplyRow {
  return replyRow(threadId, {
    issue: { key: "CORE-1", title: "Fix the thing" },
    read_through: "2026-10-12T00:30:00Z",
    reply: {
      author: { id: "bob", kind: "user" },
      created_at: NEWEST,
      first_line: "Agreed.",
      id: `${threadId}-reply`,
      kind: "reply",
    },
    subject,
  });
}

function comment(id: string, body: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...overrides,
  };
}

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** A client holding everything the Inbox reads but the thread's own comments. */
function seeded(rows: readonly MyReplyRow[]): QueryClient {
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxQuery().queryKey, []);
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(rows));
  client.setQueryData(myRepliesCountQuery().queryKey, rows.length);
  // The unread set `useMarkThreadRead` watches: without the thread in it nothing is marked.
  client.setQueryData(myRepliesOwnerQuery({ issue: "CORE-1" }).queryKey, threadsRead(rows));
  return client;
}

function renderInbox(client: QueryClient): void {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Inbox />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The element the read mark watches: the wrapper around the thread's newest message. */
function newestTurn(id: string): Element {
  const turn = document.querySelector(`[data-inbox-thread-turn="${id}"]`);
  const watched = turn?.parentElement;
  if (watched === null || watched === undefined) throw new Error(`${id} is not drawn`);
  return watched;
}

/** One dwell: on screen for the second the rule asks for, then its write let through. */
async function dwell(viewport: IntersectionControl, element: Element): Promise<void> {
  const waited = Promise.withResolvers<void>();
  await act(async () => {
    viewport.show(element);
    setTimeout(() => waited.resolve(), 1_100);
    await waited.promise;
  });
  for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
}

/** The width the Inbox shows its reading column from (1280 px); happy-dom answers `matchMedia`
 *  from the window it is given. */
function widenToReadingPane(): () => void {
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width: 1440 });
  return () => window_.happyDOM.setViewport({ width: was });
}

function mockCommentReads(): void {
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("getCredentialPending", { pending: [] });
  spy("listComments", [
    comment("comment-1", "The retry budget is too small."),
    comment("comment-2", "Agreed.", {
      author: { id: "bob", kind: "user" },
      created_at: NEWEST,
      reply_to: "comment-1",
    }),
    comment("comment-9", "And the timeout."),
  ]);
}

test("a card read by the dwell rule stays in the list, drawn as read", async () => {
  const viewport = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  mockCommentReads();
  const row = commentRow("comment-1", "The retry budget");
  const client = seeded([row]);
  renderInbox(client);
  try {
    await screen.findByTestId("inbox-thread-comment");
    await waitFor(() => expect(document.querySelector('[aria-label="Unread"]')).not.toBeNull());

    await dwell(viewport, newestTurn("comment-2"));
    expect(mark.mock.calls).toEqual([["comment-1", { read_through: NEWEST }, keyedAttempt]]);

    // The next read of the Inbox still holds the row - a thread stays until it is dismissed - and
    // says it is read now.
    act(() => {
      client.setQueryData(
        inboxThreadsQuery().queryKey,
        threadsRead([{ ...row, unread: false, unread_count: 0 }])
      );
    });
    expect(document.querySelector("[data-inbox-thread-card]")).not.toBeNull();
    expect(document.querySelector('[aria-label="Unread"]')).toBeNull();
    expect(screen.queryByLabelText("Inbox empty state")).toBeNull();
  } finally {
    viewport.restore();
  }
});

test("a row read in the column stays in the list after the selection moves off it", async () => {
  const narrow = widenToReadingPane();
  const viewport = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  mockCommentReads();
  const rows = [commentRow("comment-1", "The retry budget"), commentRow("comment-9", "Timeouts")];
  const client = seeded(rows);
  renderInbox(client);
  try {
    // The Inbox selects the first row for the reader, and the column draws its thread.
    await screen.findByTestId("inbox-pane");
    await waitFor(() =>
      expect(document.querySelector('[data-inbox-pane-body="issue"]')).not.toBeNull()
    );

    await dwell(viewport, newestTurn("comment-2"));
    expect(mark.mock.calls).toEqual([["comment-1", { read_through: NEWEST }, keyedAttempt]]);

    act(() => {
      client.setQueryData(
        inboxThreadsQuery().queryKey,
        threadsRead([
          { ...(rows[0] as MyReplyRow), unread: false, unread_count: 0 },
          rows[1] as MyReplyRow,
        ])
      );
    });
    const read = document.querySelector('[data-inbox-row="reply:comment-1"]');
    expect(read).not.toBeNull();
    expect(read?.querySelector('[aria-label="Unread"]')).toBeNull();

    // The selection moves off it: the row the reader has read is still listed, drawn as read.
    const other = document.querySelector<HTMLElement>('[data-inbox-row="reply:comment-9"]');
    fireEvent.click(other as HTMLElement);
    await waitFor(() =>
      expect(
        document.querySelector('[data-inbox-row="reply:comment-9"][data-inbox-selected]')
      ).not.toBeNull()
    );
    expect(document.querySelector('[data-inbox-row="reply:comment-1"]')).not.toBeNull();
  } finally {
    viewport.restore();
    narrow();
  }
});

const session: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/work",
  host: "box",
  last_activity: "2026-10-12T00:00:00Z",
  last_seen: Date.now(),
  machine_id: "box",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  title: "Planner",
};

function message(id: string, body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: null,
    target: "session:planner",
    ...overrides,
  };
}

test("an Agent thread is read by reply id, and moves no session read mark", async () => {
  const viewport = installIntersectionObserver();
  const state = answerAgentStates().mockResolvedValue({ unread_replies: 0 });
  const threadMark = answerThreadReads().mockResolvedValue(undefined);
  spy("listAgents", [session]);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [
        message("reply-1", "Looked at it.", {
          author: { id: "planner", kind: "session" },
          created_at: NEWEST,
          in_reply_to: "message-1",
        }),
      ],
    } satisfies MessageRead,
  ]);
  const client = seeded([]);
  // The server still counts a reply of this session's unread, so the mark is wanted.
  client.setQueryData(userAgentStateQuery().queryKey, {
    planner: { unread_replies: 1 },
  });
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ThreadRowCard thread={plannerAgentRow()} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByTestId("inbox-thread-agent");
    await waitFor(() =>
      expect(document.querySelector('[data-inbox-thread-turn="reply-1"]')).not.toBeNull()
    );

    await dwell(viewport, newestTurn("reply-1"));

    // By id, and by id alone: no `read_through` for the session, and no thread mark either.
    expect(state.mock.calls).toEqual([["planner", { read_replies: ["reply-1"] }, keyedAttempt]]);
    expect(threadMark).not.toHaveBeenCalled();
  } finally {
    viewport.restore();
  }
});
