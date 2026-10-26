import { expect, test } from "bun:test";
import type {
  Actor,
  Ask,
  Comment,
  Event,
  IssueAskThreadRow,
  Message,
  MessageRead,
} from "../../api/types";
import { activityDescription } from "../activity/activity-model";
import { buildConversations, focusConversation, matchesFilter } from "./conversation-model";

const session: Actor = { id: "session-1", kind: "session" };
const bob: Actor = { id: "bob", kind: "user" };

const baseAsk: Ask = {
  anchor: null,
  answer: null,
  author: session,
  created_at: "2026-09-10T09:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  opened_event_id: 2,
  multiple: false,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Ship it?",
  state: "open",
  urgency: "med",
};

function askRow(ask: Partial<Ask> = {}, replies: Comment[] = []): IssueAskThreadRow {
  return {
    ...baseAsk,
    ...ask,
    last_reply:
      replies.length === 0
        ? null
        : { author: replies[replies.length - 1]?.author ?? bob, created_at: "" },
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies,
      route: null,
      route_holder: null,
    },
  };
}

function comment(overrides: Partial<Comment> & Pick<Comment, "id" | "created_at">): Comment {
  return {
    anchor: null,
    ask_id: null,
    author: bob,
    body: `body of ${overrides.id}`,
    deliveries: [],
    edited_at: null,
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

function message(overrides: Partial<Message> & Pick<Message, "id" | "created_at">): Message {
  return {
    author: bob,
    body: `body of ${overrides.id}`,
    deliveries: [],
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
    ...overrides,
  };
}

function thread(root: Message, replies: Message[] = []): MessageRead {
  return { message: root, replies };
}

const build = (input: {
  asks?: IssueAskThreadRow[];
  comments?: Comment[];
  messages?: MessageRead[];
  unreadFrom?: string;
}) =>
  buildConversations({
    asks: input.asks ?? [],
    comments: input.comments ?? [],
    messages: input.messages ?? [],
    ...(input.unreadFrom === undefined ? {} : { unreadFrom: input.unreadFrom }),
  });

test("an ask answered after a later comment thread sorts above it", () => {
  const conversations = build({
    asks: [
      askRow({
        answer: { at: "2026-09-12T09:00:00Z", selected: ["Ship"], text: null, user: "bob" },
        created_at: "2026-09-10T09:00:00Z",
        state: "answered",
      }),
    ],
    comments: [comment({ created_at: "2026-09-11T09:00:00Z", id: "comment-1" })],
  });

  expect(conversations.map((item) => item.id)).toEqual(["ask:ask-1", "comment:comment-1"]);
  expect(conversations[0]?.at).toBe("2026-09-12T09:00:00Z");
});

test("a reply carries its thread above an older conversation, and an edit moves nothing", () => {
  const root = comment({ created_at: "2026-09-10T09:00:00Z", id: "comment-1" });
  const reply = comment({
    created_at: "2026-09-12T09:00:00Z",
    id: "comment-2",
    reply_to: "comment-1",
  });
  const edited = message({
    created_at: "2026-09-11T09:00:00Z",
    id: "message-1",
  });
  const conversations = build({ comments: [root, reply], messages: [thread(edited)] });
  expect(conversations.map((item) => item.id)).toEqual(["comment:comment-1", "message:message-1"]);

  // The edit is newer than every reply and still moves nothing: an edit is not activity.
  const afterEdit = build({
    comments: [{ ...root, edited_at: "2026-09-13T09:00:00Z" }, reply],
    messages: [thread(edited)],
  });
  expect(afterEdit.map((item) => item.at)).toEqual(conversations.map((item) => item.at));
});

test("conversations at the same moment break their tie on the card's id", () => {
  const at = "2026-09-10T09:00:00Z";
  const conversations = build({
    comments: [
      comment({ created_at: at, id: "comment-b" }),
      comment({ created_at: at, id: "comment-a" }),
    ],
    messages: [thread(message({ created_at: at, id: "message-1" }))],
  });

  expect(conversations.map((item) => item.id)).toEqual([
    "comment:comment-a",
    "comment:comment-b",
    "message:message-1",
  ]);
});

test("an open ask is Open; an answered one and a retracted one are Closed and still listed", () => {
  const conversations = build({
    asks: [
      askRow({ id: "ask-open" }),
      askRow({
        answer: { at: "2026-09-10T10:00:00Z", selected: ["Ship"], text: null, user: "bob" },
        id: "ask-answered",
        state: "answered",
      }),
      askRow({
        id: "ask-retracted",
        resolution: {
          actor: session,
          at: "2026-09-10T11:00:00Z",
          kind: "retracted",
          reason: "Duplicate.",
        },
        state: "resolved",
      }),
    ],
  });

  expect(
    conversations.filter((item) => matchesFilter(item, "open")).map((item) => item.id)
  ).toEqual(["ask:ask-open"]);
  expect(
    conversations.filter((item) => matchesFilter(item, "closed")).map((item) => item.id)
  ).toEqual(["ask:ask-retracted", "ask:ask-answered"]);
  expect(conversations.filter((item) => matchesFilter(item, "all"))).toHaveLength(3);
});

test("a comment thread closes when it is resolved or its suggestion is decided", () => {
  const suggestion = { replace_with: "Ship it" };
  const conversations = build({
    comments: [
      comment({ created_at: "2026-09-10T09:00:00Z", id: "open" }),
      comment({ created_at: "2026-09-10T09:01:00Z", id: "resolved", resolved: true }),
      comment({
        created_at: "2026-09-10T09:02:00Z",
        id: "accepted",
        suggestion: { ...suggestion, accepted: true },
      }),
      comment({
        created_at: "2026-09-10T09:03:00Z",
        id: "rejected",
        suggestion: { ...suggestion, accepted: false },
      }),
      comment({
        created_at: "2026-09-10T09:04:00Z",
        id: "undecided",
        suggestion: { ...suggestion, accepted: null },
      }),
    ],
  });

  expect(
    conversations.filter((item) => matchesFilter(item, "open")).map((item) => item.id)
  ).toEqual(["comment:undecided", "comment:open"]);
  expect(
    conversations.filter((item) => matchesFilter(item, "closed")).map((item) => item.id)
  ).toEqual(["comment:rejected", "comment:accepted", "comment:resolved"]);
});

test("a root comment nobody replied to is a conversation of its own", () => {
  const conversations = build({
    comments: [comment({ created_at: "2026-09-10T09:00:00Z", id: "lonely" })],
  });

  expect(conversations).toHaveLength(1);
  expect(conversations[0]?.kind).toBe("comment");
  expect(conversations[0]?.kind === "comment" && conversations[0].replies).toEqual([]);
});

test("a reply under an ask belongs to the ask's card, not a comment card of its own", () => {
  const askReply = comment({ ask_id: "ask-1", created_at: "2026-09-10T09:05:00Z", id: "reply-1" });
  const nested = comment({
    ask_id: "ask-1",
    created_at: "2026-09-10T09:06:00Z",
    id: "reply-2",
    reply_to: "reply-1",
  });
  const conversations = build({
    asks: [askRow({}, [askReply, nested])],
    comments: [askReply, nested],
  });

  expect(conversations.map((item) => item.id)).toEqual(["ask:ask-1"]);
  expect(conversations[0]?.at).toBe("2026-09-10T09:06:00Z");
});

test("a targeted message and its replies are one card, answered by the first session reply", () => {
  const root = message({
    created_at: "2026-09-10T09:00:00Z",
    id: "message-1",
    target: "session:s1",
  });
  const human = message({
    author: bob,
    created_at: "2026-09-10T09:01:00Z",
    id: "message-2",
    in_reply_to: "message-1",
  });
  const agent = message({
    author: session,
    created_at: "2026-09-10T09:02:00Z",
    id: "message-3",
    in_reply_to: "message-1",
  });
  const conversations = build({ messages: [thread(root, [agent, human])] });

  expect(conversations).toHaveLength(1);
  const card = conversations[0];
  if (card?.kind !== "targeted-message") throw new Error("expected one targeted-message card");
  expect(card.replies.map((reply) => reply.id)).toEqual(["message-2", "message-3"]);
  expect(card.answer?.id).toBe("message-3");
  expect(card.at).toBe("2026-09-10T09:02:00Z");
  // Never closed: a message thread is listed under All and under Open.
  expect(conversations.filter((item) => matchesFilter(item, "open"))).toHaveLength(1);
  expect(conversations.filter((item) => matchesFilter(item, "closed"))).toHaveLength(0);
});

test("a conversation is unread from the boundary on, and read below it", () => {
  const conversations = build({
    comments: [
      comment({ created_at: "2026-09-10T09:00:00Z", id: "read" }),
      comment({ created_at: "2026-09-10T09:05:00Z", id: "boundary" }),
      comment({ created_at: "2026-09-10T09:06:00Z", id: "unread" }),
    ],
    unreadFrom: "2026-09-10T09:05:00Z",
  });

  expect(conversations.filter((item) => item.unread).map((item) => item.id)).toEqual([
    "comment:unread",
    "comment:boundary",
  ]);
  // Without a boundary the reader has read the issue through its head: nothing is unread.
  expect(
    build({ comments: [comment({ created_at: "2026-09-10T09:06:00Z", id: "c" })] })[0]?.unread
  ).toBe(false);
});

test("an item id resolves to the conversation that holds it", () => {
  const askReply = comment({ ask_id: "ask-1", created_at: "2026-09-10T09:05:00Z", id: "reply-1" });
  const root = comment({ created_at: "2026-09-10T09:00:00Z", id: "comment-1" });
  const threadReply = comment({
    created_at: "2026-09-10T09:01:00Z",
    id: "comment-2",
    reply_to: "comment-1",
  });
  const messageRoot = message({ created_at: "2026-09-10T09:00:00Z", id: "message-1" });
  const messageReply = message({
    created_at: "2026-09-10T09:02:00Z",
    id: "message-2",
    in_reply_to: "message-1",
  });
  const lists = {
    asks: [askRow({}, [askReply])],
    comments: [askReply, root, threadReply],
    messages: [thread(messageRoot, [messageReply])],
  };

  expect(focusConversation("ask-1", lists)).toEqual({ id: "ask:ask-1" });
  expect(focusConversation("reply-1", lists)).toEqual({ id: "ask:ask-1", replyId: "reply-1" });
  expect(focusConversation("comment-1", lists)).toEqual({ id: "comment:comment-1" });
  expect(focusConversation("comment-2", lists)).toEqual({
    id: "comment:comment-1",
    replyId: "comment-2",
  });
  expect(focusConversation("message-2", lists)).toEqual({
    id: "message:message-1",
    replyId: "message-2",
  });
  expect(focusConversation("nothing", lists)).toBeUndefined();
});

// The activity line is the Activity tab's (`features/activity/activity-model.ts`); these cases
// moved here with the Conversation's own and still describe the one writer of that text.
test("describes a malformed decision block", () => {
  const event: Extract<Event, { type: "block.invalid" }> = {
    actor: session,
    created_at: "2026-09-13T17:00:00Z",
    id: 9,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      block_id: "ask-block",
      version: 3,
      reason: 'ask block "ask-block" has an option without a label',
      disturbed_by: session,
    },
    seq: 9,
    type: "block.invalid",
  };

  expect(activityDescription(event)).toBe(
    'marked decision ask-block malformed: ask block "ask-block" has an option without a label'
  );
});

test("describes a status change out of Done as reopening the issue", () => {
  const reopened = {
    actor: bob,
    created_at: "2026-09-10T10:01:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: { status: "backlog" },
    seq: 2,
    type: "issue.updated",
  } as Event;

  expect(activityDescription(reopened, "done")).toBe("reopened the issue into backlog");
});

// Five ways a claim moves, each of which must read as a distinct, true line: a session
// releasing its own claim, another session's release, a human's release of an agent's claim,
// a close, and a takeover.
const claimOf = (id: string, title: string) => ({
  actor: { id, kind: "session" as const, origin: { session_title: title } },
  at: "2026-09-24T06:00:00Z",
});

type ClaimEvent = Extract<Event, { type: "issue.claimed" | "issue.released" }>;

function claimEvent(
  type: ClaimEvent["type"],
  actor: Actor,
  payload: Partial<ClaimEvent["payload"]>
): ClaimEvent {
  return {
    actor,
    created_at: "2026-09-24T07:00:00Z",
    id: 9,
    issue_key: "CORE-1",
    notify: false,
    project: "CORE",
    seq: 9,
    type,
    payload: { key: "CORE-1", status: "in_progress", claim: null, ...payload },
  } as ClaimEvent;
}

const releaseCases: Array<[string, Event, string]> = [
  [
    "a session releasing its own claim",
    claimEvent("issue.released", claimOf("session-one", "Implementer").actor, {
      previous_claim: claimOf("session-one", "Implementer"),
      reason: "released",
    }),
    "released the claim",
  ],
  [
    "another session's release",
    claimEvent("issue.released", claimOf("session-two", "Second agent").actor, {
      previous_claim: claimOf("session-one", "Implementer"),
      reason: "released",
    }),
    "released Implementer's claim",
  ],
  [
    "a human releasing an agent's claim",
    claimEvent(
      "issue.released",
      { id: "alice", kind: "user" },
      {
        previous_claim: claimOf("session-one", "Implementer"),
        reason: "released",
      }
    ),
    "released Implementer's claim",
  ],
  [
    "a close",
    claimEvent(
      "issue.released",
      { id: "alice", kind: "user" },
      {
        previous_claim: claimOf("session-one", "Implementer"),
        reason: "closed",
      }
    ),
    "released the claim with the close",
  ],
];

test.each(releaseCases)("the activity line for %s", (_name, event, expected) => {
  expect(activityDescription(event)).toBe(expected);
});

test("a first claim and a takeover read differently in the activity line", () => {
  expect(
    activityDescription(
      claimEvent("issue.claimed", claimOf("session-one", "Implementer").actor, {
        claim: claimOf("session-one", "Implementer"),
        reason: "claimed",
      })
    )
  ).toBe("claimed the issue");
  expect(
    activityDescription(
      claimEvent("issue.claimed", claimOf("session-two", "Second agent").actor, {
        claim: claimOf("session-two", "Second agent"),
        previous_claim: claimOf("session-one", "Implementer"),
        reason: "takeover",
      })
    )
  ).toBe("took the claim from Implementer");
});

test("an answer that replaced an earlier one reads as a change in the activity line", () => {
  const answer = { at: "2026-09-10T09:05:00Z", selected: ["Hold"], text: null, user: "bob" };
  const answered = {
    actor: session,
    created_at: answer.at,
    id: 5,
    issue_key: "CORE-1",
    notify: false,
    payload: { ...baseAsk, answer, state: "answered" },
    seq: 5,
    type: "ask.answered",
  } as Event;
  expect(activityDescription(answered)).toBe("answered “Ship it?”");
  expect(
    activityDescription({
      ...answered,
      payload: {
        ...baseAsk,
        answer,
        previous_answer: { ...answer, at: "2026-09-10T09:01:00Z", selected: ["Ship"] },
        state: "answered",
      },
    } as Event)
  ).toBe("changed the answer to “Ship it?”");
});
