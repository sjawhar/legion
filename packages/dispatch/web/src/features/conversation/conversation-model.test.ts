import { expect, test } from "bun:test";
import type { Actor, Ask, Event } from "../../api/types";
import {
  activityDescription,
  buildConversationItems,
  dayLabel,
  visibleConversationItems,
} from "./conversation-model";

const session = { id: "session-1", kind: "session" as const };
const bob = { id: "bob", kind: "user" as const };

function message(
  id: number,
  at: string,
  actor: Actor = session,
  body = `m${id}`,
  target: string | null = null
): Event {
  return {
    actor,
    created_at: at,
    id,
    issue_key: "CORE-1",
    notify: false,
    seq: id,
    type: "message.created",
    payload: {
      author: actor,
      body,
      created_at: at,
      deliveries: [],
      id: `message-${id}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target,
    },
  } as Event;
}

function askEvent(id: number, at: string, type: "ask.opened" | "ask.answered", ask: Ask): Event {
  const event = {
    actor: session,
    created_at: at,
    id,
    issue_key: "CORE-1",
    notify: false,
    seq: id,
  };
  if (type === "ask.opened") {
    return { ...event, payload: ask, type: "ask.opened" };
  }
  return { ...event, payload: ask, type: "ask.answered" };
}

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

const build = (events: Event[], lastReadSeq = 0) =>
  buildConversationItems({ events, lastReadSeq, today: "2026-09-10" });

test("coalesces a targeted message, its delivery attempts, and an answer into one card", () => {
  const asked = {
    actor: bob,
    created_at: "2026-09-10T09:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: bob,
      body: "Can this ship?",
      created_at: "2026-09-10T09:00:00Z",
      deliveries: [],
      id: "message-1",
      in_reply_to: null,
      issue_key: "CORE-1",
      target: "session:planner",
    },
    seq: 1,
    type: "message.created" as const,
  };
  const delivered = {
    actor: bob,
    created_at: "2026-09-10T09:00:01Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      attempt: 1,
      delivery: "btw" as const,
      message_id: "message-1",
      session_id: "planner",
      state: "sent" as const,
      title: "Planner",
    },
    seq: 2,
    type: "message.delivery" as const,
  };
  const answered = {
    actor: session,
    created_at: "2026-09-10T09:00:02Z",
    id: 3,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: session,
      body: "Yes.",
      created_at: "2026-09-10T09:00:02Z",
      deliveries: [],
      id: "message-2",
      in_reply_to: "message-1",
      issue_key: "CORE-1",
      target: null,
    },
    seq: 3,
    type: "message.answered" as const,
  };
  const item = build([asked, delivered, answered] as Event[]).find(
    (candidate) => candidate.kind === "targeted-message"
  );

  if (item === undefined || item.kind !== "targeted-message") {
    throw new Error("targeted message card was not built");
  }
  expect(item.event.payload.body).toBe("Can this ship?");
  expect(item.deliveries).toEqual([delivered]);
  expect(item.answer?.event).toEqual(answered);
  expect(item.replies.map((reply) => reply.event)).toEqual([answered]);
  expect(item.lastSeq).toBe(3);
});

function reply(
  id: number,
  at: string,
  inReplyTo: string,
  actor: Actor = session,
  overrides: Partial<{ body: string; target: string | null }> = {}
): Event {
  return {
    actor,
    created_at: at,
    id,
    issue_key: "CORE-1",
    notify: false,
    seq: id,
    type: "message.answered",
    payload: {
      author: actor,
      body: overrides.body ?? `r${id}`,
      created_at: at,
      deliveries: [],
      id: `message-${id}`,
      in_reply_to: inReplyTo,
      issue_key: "CORE-1",
      reply_body: "parent",
      target: overrides.target ?? null,
    },
  } as Event;
}

function delivery(id: number, at: string, messageId: string): Event {
  return {
    actor: bob,
    created_at: at,
    id,
    issue_key: "CORE-1",
    notify: false,
    seq: id,
    type: "message.delivery",
    payload: {
      attempt: 1,
      delivery: "btw",
      message_id: messageId,
      session_id: "planner",
      state: "sent",
      title: "Planner",
    },
  } as Event;
}

test("a reply nests under its thread root, which sits at the thread's latest activity", () => {
  const items = build([
    message(1, "2026-09-10T09:00:00Z", bob),
    message(2, "2026-09-10T09:05:00Z", bob),
    reply(3, "2026-09-10T09:10:00Z", "message-1"),
  ]);
  const turns = items.filter((item) => item.kind === "message");
  expect(turns.map((item) => item.kind === "message" && item.seq)).toEqual([1, 2]);
  const thread = turns[0];
  if (thread?.kind !== "message") throw new Error("thread root was not built");
  expect(thread.replies.map((item) => item.id)).toEqual(["message:message-3"]);
  expect(thread.replies[0]?.event.seq).toBe(3);
  expect(thread.lastSeq).toBe(3);
  // A thread with replies is a boundary, never a continuation of the message above it.
  expect(turns.map((item) => item.kind === "message" && item.continued)).toEqual([false, false]);
});

test("a follow-up on a targeted thread nests with its own delivery; the session's first reply is the answer", () => {
  const asked = message(1, "2026-09-10T09:00:00Z", bob, "Can this ship?", "session:planner");
  const items = build([
    asked,
    delivery(2, "2026-09-10T09:00:01Z", "message-1"),
    reply(3, "2026-09-10T09:01:00Z", "message-1", session, { body: "Once it is green." }),
    reply(4, "2026-09-10T09:02:00Z", "message-3", bob, {
      body: "It is green.",
      target: "session:planner",
    }),
    delivery(5, "2026-09-10T09:02:01Z", "message-4"),
    reply(6, "2026-09-10T09:03:00Z", "message-4", session, { body: "Shipping." }),
  ]);
  const turns = items.filter((item) => item.kind !== "day-divider");
  expect(turns.map((item) => item.kind)).toEqual(["targeted-message"]);
  const card = turns[0];
  if (card?.kind !== "targeted-message") throw new Error("card was not built");
  expect(card.answer?.event.seq).toBe(3);
  expect(card.replies.map((item) => item.event.seq)).toEqual([3, 4, 6]);
  expect(card.replies[1]?.deliveries.map((item) => item.seq)).toEqual([5]);
  expect(card.lastSeq).toBe(6);
});

test("a reply whose parent is not loaded stays a top-level turn", () => {
  const items = build([reply(3, "2026-09-10T09:10:00Z", "message-1")]);
  expect(items.map((item) => item.kind)).toEqual(["day-divider", "message"]);
});

test("a thread's day and unread placement follow its latest reply, not its root", () => {
  const items = build(
    [
      message(1, "2026-09-09T09:00:00Z", bob),
      message(2, "2026-09-10T09:00:00Z", bob),
      reply(3, "2026-09-10T09:10:00Z", "message-1"),
    ],
    2
  );
  expect(
    items.map((item) =>
      item.kind === "day-divider" ? item.label : item.kind === "message" ? item.seq : item.kind
    )
  ).toEqual(["Today", 1, "unread-divider", 2]);
});

test("an ask edit updates its existing card and remains a question-edit activity line", () => {
  const edited: Extract<Event, { type: "ask.edited" }> = {
    actor: session,
    created_at: "2026-09-10T09:05:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      ...baseAsk,
      edited_at: "2026-09-10T09:05:00Z",
      edited_by: session,
      options: [{ label: "SSE" }, { label: "Polling" }],
      previous: {
        multiple: false,
        options: [{ label: "Ship" }, { label: "Hold" }],
        question: "Ship it?",
        urgency: "med",
      },
      question: "Which transport should we use?",
    },
    seq: 3,
    type: "ask.edited",
  };
  const items = build([askEvent(2, "2026-09-10T09:00:00Z", "ask.opened", baseAsk), edited]);

  expect(items.map((item) => item.kind)).toEqual(["day-divider", "activity", "ask"]);
  const ask = items.find((item) => item.kind === "ask");
  const activity = items.find((item) => item.kind === "activity");
  if (
    ask === undefined ||
    ask.kind !== "ask" ||
    activity === undefined ||
    activity.kind !== "activity"
  ) {
    throw new Error("edited ask should retain its card and add one activity line");
  }
  expect(ask.ask.question).toBe("Which transport should we use?");
  expect(activity.description).toBe('edited the question "Which transport should we use?"');
});

test("an ask anchor refresh updates its folded anchor state", () => {
  const opened = {
    ...baseAsk,
    anchor: {
      artifact_id: "artifact-secondary",
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "the quoted text",
      version: 1,
    },
  };
  const refreshed: Extract<Event, { type: "ask.anchor_refreshed" }> = {
    actor: session,
    created_at: "2026-09-10T09:05:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      ...opened,
      anchor: { ...opened.anchor, orphaned: true },
    },
    seq: 3,
    type: "ask.anchor_refreshed",
  };

  const items = build([askEvent(2, "2026-09-10T09:00:00Z", "ask.opened", opened), refreshed]);
  const ask = items.find((item) => item.kind === "ask");
  if (ask === undefined || ask.kind !== "ask") throw new Error("expected refreshed ask");

  expect(ask.ask.anchor?.orphaned).toBe(true);
  expect(ask.lastSeq).toBe(3);
  expect(items.filter((item) => item.kind === "activity")).toHaveLength(0);
});

test("an ask's opened, answered and resolved events coalesce into one item placed where it was asked", () => {
  const items = build([
    message(1, "2026-09-10T08:59:00Z"),
    askEvent(2, "2026-09-10T09:00:00Z", "ask.opened", baseAsk),
    message(3, "2026-09-10T09:01:00Z", bob),
    askEvent(4, "2026-09-10T09:05:00Z", "ask.answered", {
      ...baseAsk,
      state: "answered",
      answer: {
        at: "2026-09-10T09:05:00Z",
        selected: ["Ship"],
        text: null,
        user: "bob",
      },
    }),
  ]);
  const kinds = items.map((item) => item.kind);
  expect(kinds).toEqual(["day-divider", "message", "ask", "message"]);
  const ask = items[2];
  if (ask.kind !== "ask") throw new Error("expected ask");
  expect(ask.seq).toBe(2);
  expect(ask.lastSeq).toBe(4);
  expect(ask.pinEventId).toBe(2);
  expect(ask.ask.state).toBe("answered");
  expect(ask.ask.answer?.selected).toEqual(["Ship"]);
});

test("an ask pin keeps its opened event id when older events are loaded", () => {
  const answered = askEvent(44, "2026-09-10T09:05:00Z", "ask.answered", {
    ...baseAsk,
    answer: {
      at: "2026-09-10T09:05:00Z",
      selected: ["Ship"],
      text: null,
      user: "bob",
    },
    opened_event_id: 12,
    state: "answered",
  });
  const loadedLater = build([answered]).find((item) => item.kind === "ask");
  const loadedTogether = build([
    askEvent(12, "2026-09-10T09:00:00Z", "ask.opened", {
      ...baseAsk,
      opened_event_id: 12,
    }),
    answered,
  ]).find((item) => item.kind === "ask");

  if (loadedLater === undefined || loadedTogether === undefined) {
    throw new Error("expected both fixtures to produce an ask");
  }
  expect(loadedLater.pinEventId).toBe(12);
  expect(loadedTogether.pinEventId).toBe(12);
});

test("an ask whose opened event is not loaded still renders from its later events", () => {
  const items = build([
    askEvent(9, "2026-09-10T09:05:00Z", "ask.answered", {
      ...baseAsk,
      opened_event_id: 9,
      state: "answered",
      answer: {
        at: "2026-09-10T09:05:00Z",
        selected: ["Hold"],
        text: null,
        user: "bob",
      },
    }),
  ]);
  expect(items.map((item) => item.kind)).toEqual(["day-divider", "ask"]);
  expect(items[1]).toMatchObject({
    seq: 9,
    lastSeq: 9,
    at: "2026-09-10T09:05:00Z",
  });
});

test("turns run newest first and same-author messages within five minutes continue the group", () => {
  const items = build([
    message(3, "2026-09-10T10:09:00Z"),
    message(1, "2026-09-10T10:00:00Z"),
    message(2, "2026-09-10T10:03:00Z"),
    message(4, "2026-09-10T10:10:00Z", bob),
    message(5, "2026-09-10T10:11:00Z"),
  ]);
  expect(
    items.map((item) => (item.kind === "message" ? [item.seq, item.continued] : item.kind))
  ).toEqual(["day-divider", [5, false], [4, false], [3, false], [2, false], [1, true]]);
});

test("an ask or a divider breaks message grouping; an activity line does not", () => {
  const closed = {
    ...message(2, "2026-09-10T10:01:00Z"),
    type: "issue.closed",
    payload: {},
  } as Event;
  const items = build([
    message(1, "2026-09-10T10:00:00Z"),
    closed,
    message(3, "2026-09-10T10:02:00Z"),
    askEvent(4, "2026-09-10T10:03:00Z", "ask.opened", baseAsk),
    message(5, "2026-09-10T10:04:00Z"),
  ]);
  expect(
    items.map((item) => (item.kind === "message" ? [item.seq, item.continued] : item.kind))
  ).toEqual(["day-divider", [5, false], "ask", [3, false], "activity", [1, true]]);
});

test("day dividers land on local calendar boundaries and name Today and Yesterday", () => {
  const yesterday = new Date("2026-09-09T12:00:00Z");
  const items = build([message(1, yesterday.toISOString()), message(2, "2026-09-10T12:00:00Z")]);
  const dividers = items.filter((item) => item.kind === "day-divider");
  expect(dividers.map((item) => item.kind === "day-divider" && item.label)).toEqual([
    "Today",
    "Yesterday",
  ]);
  expect(dayLabel("2025-01-03", "2026-09-10")).toBe(
    new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "long",
      weekday: "long",
      year: "numeric",
    }).format(new Date(2025, 0, 3))
  );
});

test("the unread divider follows the newest unread turns and precedes the read turns below them", () => {
  const events = [
    message(1, "2026-09-10T10:00:00Z"),
    message(2, "2026-09-10T10:01:00Z"),
    message(3, "2026-09-10T10:02:00Z"),
  ];
  expect(build(events, 2).map((item) => item.kind)).toEqual([
    "day-divider",
    "message",
    "unread-divider",
    "message",
    "message",
  ]);
  expect(build(events, 0).some((item) => item.kind === "unread-divider")).toBe(false);
  expect(build(events, 3).some((item) => item.kind === "unread-divider")).toBe(false);
  expect(
    build(events, 2)
      .filter((item) => item.kind === "message")
      .map((item) => item.kind === "message" && item.continued)
  ).toEqual([false, false, true]);
});

test("Conversation owns anchored and unanchored comment threads with delivery state", () => {
  const comment = (id: string, overrides: object = {}) => ({
    anchor: null,
    artifact_name: "spec",
    ask_id: null,
    author: session,
    body: "hi",
    created_at: "2026-09-10T10:00:00Z",
    deliveries: [],
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    suggestion: null,
    ...overrides,
  });
  const at = "2026-09-10T10:00:00Z";
  const events = [
    {
      ...message(1, at),
      type: "comment.created",
      payload: comment("c1", {
        anchor: {
          artifact_id: "a",
          mark_id: "m",
          orphaned: false,
          quote: "brown fox",
          version: 1,
        },
      }),
    },
    {
      ...message(2, at),
      type: "comment.created",
      payload: comment("c2", { body: "reply", reply_to: "c1" }),
    },
    {
      ...message(3, at),
      type: "comment.created",
      payload: comment("c3", { body: "plain" }),
    },
    {
      ...message(4, at),
      type: "comment.created",
      payload: comment("c4", { ask_id: "ask-1", ask_question: "Ship it?" }),
    },
    {
      ...message(5, at),
      type: "comment.delivery",
      payload: {
        attempt: 1,
        comment_id: "c1",
        delivery: "steer",
        error: undefined,
        reply_id: null,
        session_id: "s1",
        state: "sent",
        target: "session:s1",
      },
    },
  ] as Event[];
  const comments = build(events).filter((item) => item.kind === "comment");

  expect(comments).toHaveLength(2);
  const anchored = comments.find(
    (item) => item.kind === "comment" && item.event.payload.id === "c1"
  );
  if (anchored === undefined || anchored.kind !== "comment")
    throw new Error("anchored comment missing");
  expect(anchored.event.payload.anchor?.quote).toBe("brown fox");
  expect(anchored.replies.map((reply) => reply.event.payload.id)).toEqual(["c2"]);
  expect(anchored.deliveries.map((delivery) => delivery.target)).toEqual(["session:s1"]);
  expect(anchored.lastSeq).toBe(5);

  expect(
    build(events)
      .filter((item) => item.kind === "activity")
      .map((item) => item.kind === "activity" && item.description)
  ).toEqual(["replied to “Ship it?”"]);
  expect(
    activityDescription({
      ...message(6, at),
      type: "comment.delivery",
      payload: {
        attempt: 1,
        comment_id: "c1",
        delivery: "steer",
        reply_id: null,
        session_id: "s1",
        state: "sent",
        target: "session:s1",
      },
    } as Event)
  ).toBe("delivered a comment mention to session:s1");
});

test("folds comment lifecycle into the final comment turn across day and read boundaries", () => {
  const createdAt = "2026-09-10T10:00:00Z";
  const reopenedAt = "2026-09-11T12:00:00Z";
  const editedAt = "2026-09-11T12:05:00Z";
  const refreshedAt = "2026-09-11T12:10:00Z";
  const comment = (overrides: object = {}) => ({
    anchor: {
      artifact_id: "artifact-secondary",
      mark_id: "mark-1",
      orphaned: false,
      quote: "the quoted text",
      version: 1,
    },
    artifact_name: "secondary",
    ask_id: null,
    author: bob,
    body: "Original",
    created_at: createdAt,
    deliveries: [],
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ...overrides,
  });
  const events = [
    {
      ...message(1, createdAt, bob),
      payload: comment(),
      type: "comment.created",
    },
    message(2, "2026-09-10T11:00:00Z"),
    {
      ...message(3, "2026-09-10T12:00:00Z", session),
      payload: comment({
        resolved: true,
        resolved_at: "2026-09-10T12:00:00Z",
        resolved_by: session,
      }),
      type: "comment.resolved",
    },
    {
      ...message(4, reopenedAt, bob),
      payload: comment(),
      type: "comment.reopened",
    },
    {
      ...message(5, editedAt, bob),
      payload: comment({ body: "Final edit", edited_at: editedAt }),
      type: "comment.edited",
    },
    {
      ...message(6, refreshedAt, session),
      payload: comment({
        body: "Final edit",
        edited_at: editedAt,
        anchor: {
          artifact_id: "artifact-secondary",
          mark_id: "mark-1",
          orphaned: true,
          quote: "the quoted text",
          version: 1,
        },
      }),
      type: "comment.anchor_refreshed",
    },
  ] as Event[];

  const items = build(events, 2);
  const comments = items.filter((item) => item.kind === "comment");
  if (comments.length !== 1 || comments[0]?.kind !== "comment") {
    throw new Error("expected one folded comment turn");
  }

  expect(comments[0].event.payload.body).toBe("Final edit");
  expect(comments[0].event.payload.created_at).toBe(createdAt);
  expect(comments[0].event.payload.anchor?.orphaned).toBe(true);
  expect(comments[0].lastAt).toBe(refreshedAt);
  expect(comments[0].lastSeq).toBe(6);
  expect(items.map((item) => item.kind)).toEqual([
    "day-divider",
    "comment",
    "day-divider",
    "unread-divider",
    "message",
  ]);
  expect(
    items
      .filter((item) => item.kind === "activity")
      .map((item) => (item.kind === "activity" ? item.event.type : undefined))
  ).toEqual([]);
});

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
  const closed = {
    ...message(1, "2026-09-10T10:00:00Z"),
    payload: { status: "done" },
    type: "issue.closed",
  } as Event;
  const reopened = {
    ...message(2, "2026-09-10T10:01:00Z"),
    payload: { status: "backlog" },
    type: "issue.updated",
  } as Event;
  const activity = build([closed, reopened]).find(
    (item) => item.kind === "activity" && item.event.id === 2
  );

  if (activity === undefined || activity.kind !== "activity") {
    throw new Error("reopened issue activity was not built");
  }
  expect(activity.description).toBe("reopened the issue into backlog");
});

test("hiding activity drops the lines and any day that would be left empty", () => {
  const items = build([
    { ...message(1, "2026-09-09T10:00:00Z"), type: "issue.created", payload: {} } as Event,
    message(2, "2026-09-10T10:00:00Z"),
  ]);
  expect(
    visibleConversationItems(items, { showActivity: true, showRetracted: true }).map(
      (item) => item.kind
    )
  ).toEqual(["day-divider", "message", "day-divider", "activity"]);
  expect(
    visibleConversationItems(items, { showActivity: false, showRetracted: false }).map(
      (item) => item.kind
    )
  ).toEqual(["day-divider", "message"]);
  expect(visibleConversationItems(items, { showActivity: false, showRetracted: false })[1]).toBe(
    items[1]
  );
});

test("a retracted ask is hidden with the activity by default and shown when activity is shown", () => {
  const retracted: Ask = {
    ...baseAsk,
    state: "resolved",
    resolution: {
      actor: session,
      at: "2026-09-10T09:10:00Z",
      kind: "retracted",
      reason: "Duplicate of the spec's own decision.",
    },
  };
  const events = [
    askEvent(2, "2026-09-10T09:00:00Z", "ask.opened", baseAsk),
    {
      ...askEvent(3, "2026-09-10T09:10:00Z", "ask.answered", retracted),
      type: "ask.resolved",
    } as Event,
    message(4, "2026-09-11T09:00:00Z", bob),
  ];
  const items = build(events);
  // Hidden even when activity is shown: retraction is its own toggle, off by default.
  for (const showActivity of [false, true]) {
    expect(
      visibleConversationItems(items, { showActivity, showRetracted: false }).map(
        (item) => item.kind
      )
    ).toEqual(["day-divider", "message"]);
  }
  expect(
    visibleConversationItems(items, { showActivity: false, showRetracted: true }).map(
      (item) => item.kind
    )
  ).toEqual(["day-divider", "message", "day-divider", "ask"]);
});
