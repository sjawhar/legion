import { expect, test } from "bun:test";
import type { Event } from "../api/types";
import { buildLogItems, dismissedEventIds, eventDescription } from "../features/issue/log-model";

function event(overrides: Partial<Event> = {}): Event {
  return {
    actor: { kind: "session", id: "session-1" },
    created_at: "2026-09-09T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "session-1", kind: "session" },
      body: "Update",
      created_at: "2026-09-09T00:00:00Z",
      id: "message-1",
      issue_key: "CORE-1",
    },
    seq: 1,
    type: "message.created",
    ...overrides,
  } as Event;
}

function askEvent(
  type: "ask.opened" | "ask.answered",
  id: number,
  seq: number,
  answer: { at: string; selected: string[]; text: string | null; user: string } | null = null
): Extract<Event, { type: "ask.opened" | "ask.answered" | "ask.resolved" }> {
  return {
    actor: { kind: "session", id: "session-1" },
    created_at: answer?.at ?? "2026-09-09T00:00:00Z",
    id,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      answer,
      author: { kind: "session", id: "session-1" },
      created_at: "2026-09-09T00:00:00Z",
      id: "ask-1",
      issue_key: "CORE-1",
      multiple: false,
      options: [{ description: "Ship immediately", label: "Ship" }, { label: "Hold" }],
      question: "Which option should ship?",
      state: answer === null ? "open" : "answered",
      urgency: "med",
    },
    seq,
    type,
  };
}

test("log model coalesces an opened and answered ask with both timestamps", () => {
  const opened = askEvent("ask.opened", 1, 1);
  const answered = askEvent("ask.answered", 2, 2, {
    at: "2026-09-09T00:05:00Z",
    selected: ["Ship"],
    text: "Proceed.",
    user: "alice",
  });

  const items = buildLogItems([opened, answered], [], 2);

  expect(items).toHaveLength(1);
  expect(items[0]).toEqual({ event: answered, folded: false, kind: "ask" });
  const item = items[0];
  if (item === undefined || item.kind !== "ask") {
    throw new Error("Expected a coalesced ask item");
  }
  expect(item.event.payload.created_at).toBe("2026-09-09T00:00:00Z");
  expect(item.event.payload.answer?.at).toBe("2026-09-09T00:05:00Z");
});

test("log model omits dismissed events and places a new divider at the read boundary", () => {
  const items = buildLogItems(
    [event({ id: 1, seq: 1 }), event({ id: 2, seq: 2 }), event({ id: 3, seq: 3 })],
    ["event:2"],
    2
  );

  expect(items).toEqual([
    { event: event({ id: 3, seq: 3 }), folded: false, kind: "event" },
    { kind: "new-divider" },
    { event: event({ id: 1, seq: 1 }), folded: false, kind: "event" },
  ]);
});

test("dismissedEventIds extracts dismissed log events without pinned markers", () => {
  expect(dismissedEventIds(["event:2", "pinned_items:event:5", "event:9"])).toEqual(["2", "9"]);
});

test("log model labels a resolved ask with the resolution actor and reason", () => {
  const resolved = event({
    payload: {
      anchor: null,
      answer: null,
      author: { kind: "session", id: "session-1" },
      created_at: "2026-09-10T00:00:00Z",
      id: "ask-1",
      issue_key: "CORE-1",
      multiple: false,
      options: [],
      question: "Ship the change?",
      resolution: {
        actor: { kind: "session", id: "session-1" },
        at: "2026-09-10T00:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
      urgency: "med",
    },
    type: "ask.resolved",
  }) as Extract<Event, { type: "ask.resolved" }>;

  expect(eventDescription(resolved)).toBe(
    "Retracted by session-1 - A newer question supersedes this one."
  );
  expect(buildLogItems([resolved], [], 1)).toEqual([
    { event: resolved, folded: false, kind: "ask" },
  ]);
});
