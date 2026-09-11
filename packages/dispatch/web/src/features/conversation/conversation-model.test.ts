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

function message(id: number, at: string, actor: Actor = session, body = `m${id}`): Event {
  return {
    actor,
    created_at: at,
    id,
    issue_key: "CORE-1",
    notify: false,
    seq: id,
    type: "message.created",
    payload: { author: actor, body, created_at: at, id: `message-${id}`, issue_key: "CORE-1" },
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
  opened_event_id: 2,
  multiple: false,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Ship it?",
  state: "open",
  urgency: "med",
};

const build = (events: Event[], lastReadSeq = 0) =>
  buildConversationItems({ events, lastReadSeq, today: "2026-09-10" });

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

test("anchored comments, replies and system events are activity lines described as verb phrases", () => {
  const comment = (overrides: object) => ({
    anchor: null,
    artifact_name: "spec",
    ask_id: null,
    author: session,
    body: "hi",
    created_at: "2026-09-10T10:00:00Z",
    id: "c1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    suggestion: null,
    ...overrides,
  });
  const at = "2026-09-10T10:00:00Z";
  const events = [
    { ...message(1, at), type: "comment.created", payload: comment({}) },
    {
      ...message(2, at),
      type: "comment.created",
      payload: comment({
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
      ...message(3, at),
      type: "comment.created",
      payload: comment({ ask_id: "ask-1", ask_question: "Ship it?" }),
    },
    { ...message(4, at), type: "comment.created", payload: comment({ reply_to: "c1" }) },
    {
      ...message(5, at),
      type: "artifact.version",
      payload: { artifact_id: "a", name: "spec", version: { number: 3 } },
    },
    {
      ...message(6, at),
      type: "child.status",
      payload: { child_key: "CORE-2", from: "todo", to: "in_progress" },
    },
    { ...message(7, at), type: "issue.updated", payload: { status: "testing" } },
  ] as Event[];
  const items = build(events);
  expect(items.map((item) => item.kind)).toEqual([
    "day-divider",
    "activity",
    "activity",
    "activity",
    "activity",
    "activity",
    "activity",
    "comment",
  ]);
  expect(
    items
      .filter((item) => item.kind === "activity")
      .map((item) => item.kind === "activity" && item.description)
  ).toEqual([
    "updated the issue",
    "moved CORE-2 from todo to in_progress",
    "saved spec v3",
    "replied to a comment",
    "replied to “Ship it?”",
    "commented on spec: “brown fox”",
  ]);
  expect(
    activityDescription({ ...message(8, at), type: "issue.closed", payload: {} } as Event)
  ).toBe("closed the issue");
  const resolved = events.find((event) => event.type === "comment.created");
  if (resolved === undefined || resolved.type !== "comment.created") throw new Error("fixture");
  expect(activityDescription({ ...resolved, type: "comment.reopened" })).toBe(
    "reopened a comment on spec"
  );
  expect(activityDescription({ ...resolved, type: "comment.edited" })).toBe(
    "edited a comment on spec"
  );
});

test("hiding activity drops the lines and any day that would be left empty", () => {
  const items = build([
    { ...message(1, "2026-09-09T10:00:00Z"), type: "issue.created", payload: {} } as Event,
    message(2, "2026-09-10T10:00:00Z"),
  ]);
  expect(visibleConversationItems(items, true).map((item) => item.kind)).toEqual([
    "day-divider",
    "message",
    "day-divider",
    "activity",
  ]);
  expect(visibleConversationItems(items, false).map((item) => item.kind)).toEqual([
    "day-divider",
    "message",
  ]);
  expect(visibleConversationItems(items, false)[1]).toBe(items[1]);
});
