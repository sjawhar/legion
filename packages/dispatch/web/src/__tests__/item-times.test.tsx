import { afterAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { Ask, AskRead, Comment, IssueClaim, Message, MessageRead } from "../api/types";
import { ConversationTab } from "../features/conversation/ConversationTab";
import { ReplyTurn } from "../features/conversation/ReplyTurn";
import { AskCard } from "../features/inbox/AskCard";
import { ClaimChip } from "../features/issue/ClaimChip";
import { ThreadCard } from "../features/margin/ThreadCard";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { commentDeliveryFields } from "./comment-fixture";

// Every time below is its item's own: each item gets a moment of its own, so a test finds the
// time it means by its `datetime`, then reads the link around it.
const askedAt = "2026-09-09T00:00:00Z";
const askReplyAt = "2026-09-09T00:01:00Z";
const answeredAt = "2026-09-09T00:05:00Z";
const rootAt = "2026-09-10T00:00:00Z";
const replyAt = "2026-09-10T00:01:00Z";
const messageAt = "2026-09-11T00:00:00Z";
const messageReplyAt = "2026-09-11T00:01:00Z";

const getReferences = spyOn(api, "getReferences").mockResolvedValue({
  edges: [],
  node: { id: "ask-1", kind: "ask" },
});

afterAll(() => {
  getReferences.mockRestore();
});

/** Where the time drawn for `at` links, or null when it is plain text. */
function linkOfTime(at: string): string | null {
  const time = document.querySelector(`time[datetime="${at}"]`);
  if (time === null) throw new Error(`no time ${at} on the page`);
  return time.closest("a")?.getAttribute("href") ?? null;
}

function renderWith(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>{node}</KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function comment(id: string, createdAt: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Body of ${id}`,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
    ...overrides,
  };
}

function ask(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: askedAt,
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which option should ship?",
    state: "open",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

function thread(input: Ask, replies: Comment[]): () => Promise<AskRead> {
  return async () => ({
    answers: [],
    ask: input,
    claimant: null,
    edits: [],
    followers: [],
    replies,
    route: null,
    route_holder: null,
    route_status: null,
  });
}

function threadCard(
  owner: { key: string; kind: "issue" } | MarginDocument,
  issueKey: string | null
) {
  const root = comment("root-1", rootAt, { issue_key: issueKey });
  const reply = comment("reply-1", replyAt, { issue_key: issueKey, reply_to: root.id });
  return (
    <ThreadCard
      actionFailure={undefined}
      artifactSlug="spec"
      editingCommentId={undefined}
      expanded
      hovered={false}
      isClosed={false}
      onAction={() => {}}
      onEdit={async () => undefined}
      onEditingChange={() => {}}
      onRetryAction={() => {}}
      onToggle={() => {}}
      owner={owner}
      pendingAction={false}
      savingCommentEditId={undefined}
      thread={{
        anchor: null,
        key: root.id,
        lastReplyAt: reply.created_at,
        replies: [reply],
        resolved: false,
        root: { comment: root, kind: "comment" },
      }}
      viewerLogin="alice"
    />
  );
}

type MarginDocument = { artifactId: string; kind: "document"; project: string; slug: string };
const notes: MarginDocument = {
  artifactId: "artifact-notes",
  kind: "document",
  project: "CORE",
  slug: "notes",
};

test("a comment's and its reply's times in a margin thread link to each comment", async () => {
  const view = renderWith(threadCard({ key: "CORE-1", kind: "issue" }, "CORE-1"));
  try {
    await screen.findByText("Body of reply-1");
    expect(linkOfTime(rootAt)).toBe("/issues/CORE-1/comments/root-1");
    expect(linkOfTime(replyAt)).toBe("/issues/CORE-1/comments/reply-1");
  } finally {
    view.unmount();
  }
});

test("a project document's comment times link to the comment on its document", async () => {
  const view = renderWith(threadCard(notes, null));
  try {
    await screen.findByText("Body of reply-1");
    expect(linkOfTime(rootAt)).toBe("/projects/CORE/documents/notes?comment=root-1");
    expect(linkOfTime(replyAt)).toBe("/projects/CORE/documents/notes?comment=reply-1");
  } finally {
    view.unmount();
  }
});

test("a message's and its reply's times on the Conversation tab link to each message", async () => {
  const author = { id: "alice", kind: "user" } as const;
  const root: Message = {
    author,
    body: "Status please",
    created_at: messageAt,
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
  };
  const reply: Message = {
    ...root,
    body: "On it",
    created_at: messageReplyAt,
    id: "message-2",
    in_reply_to: root.id,
  };
  const messages: MessageRead[] = [{ message: root, replies: [reply] }];
  const spies = [
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue(messages),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
  ];
  const view = renderWith(
    <ConversationTab
      issueArtifacts={new Map()}
      isClosed={false}
      issueKey="CORE-1"
      state={{ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } }}
      visible
    />
  );
  try {
    await screen.findByText("On it");
    expect(linkOfTime(messageAt)).toBe("/issues/CORE-1/messages/message-1");
    expect(linkOfTime(messageReplyAt)).toBe("/issues/CORE-1/messages/message-2");
  } finally {
    view.unmount();
    for (const spy of spies) spy.mockRestore();
  }
});

test("an ask's and its reply's times link to the ask and to the reply", async () => {
  const input = ask();
  const reply = comment("comment-1", askReplyAt, { ask_id: input.id });
  const view = renderWith(<AskCard ask={input} getAskThread={thread(input, [reply])} />);
  try {
    await screen.findByText("Body of comment-1");
    expect(linkOfTime(askedAt)).toBe("/issues/CORE-1/asks/ask-1");
    expect(linkOfTime(askReplyAt)).toBe("/issues/CORE-1/comments/comment-1");
  } finally {
    view.unmount();
  }
});

test("a project document's ask and its reply link to themselves on that document", async () => {
  const input = ask({ issue_key: null });
  const reply = comment("comment-1", askReplyAt, { ask_id: input.id, issue_key: null });
  const view = renderWith(
    <AskCard
      ask={input}
      getAskThread={thread(input, [reply])}
      owner={{ project: "CORE", slug: "notes" }}
    />
  );
  try {
    await screen.findByText("Body of comment-1");
    expect(linkOfTime(askedAt)).toBe("/projects/CORE/documents/notes?ask=ask-1");
    expect(linkOfTime(askReplyAt)).toBe("/projects/CORE/documents/notes?comment=comment-1");
  } finally {
    view.unmount();
  }
});

test("an answered ask's asked and answered times both link to the ask", async () => {
  const input = ask({
    answer: { at: answeredAt, selected: ["Ship"], text: null, user: "alice" },
    state: "answered",
  });
  const view = renderWith(<AskCard ask={input} getAskThread={thread(input, [])} />);
  try {
    await waitFor(() => expect(linkOfTime(answeredAt)).toBe("/issues/CORE-1/asks/ask-1"));
    expect(linkOfTime(askedAt)).toBe("/issues/CORE-1/asks/ask-1");
  } finally {
    view.unmount();
  }
});

test("a reply with no address of its own, such as a direct message's, shows a plain time", () => {
  const view = renderWith(
    <ol>
      <ReplyTurn
        at={messageReplyAt}
        author={{ id: "alice", kind: "user" }}
        body="A direct reply"
        turnID="message:direct-1"
      />
    </ol>
  );
  try {
    expect(linkOfTime(messageReplyAt)).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a claim's age is about no one item, so it stays plain text", () => {
  const claim: IssueClaim = {
    actor: { id: "alice", kind: "user" },
    at: "2026-09-24T06:00:00Z",
  };
  const view = renderWith(<ClaimChip claim={claim} />);
  try {
    expect(linkOfTime(claim.at)).toBeNull();
  } finally {
    view.unmount();
  }
});
