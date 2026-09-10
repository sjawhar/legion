import { expect, test } from "bun:test";
import type { Event } from "../api/types";
import { buildLogItems, dismissedEventIds } from "../features/issue/log-model";

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

test("log model reverses server events and folds events that resolve their item", () => {
  const items = buildLogItems(
    [
      event({ id: 1, seq: 1, type: "ask.opened" }),
      event({ id: 2, seq: 2, type: "ask.answered" }),
      event({ id: 3, seq: 3, type: "comment.resolved" }),
      event({ id: 4, seq: 4, type: "suggestion.accepted" }),
      event({ id: 5, seq: 5, type: "suggestion.rejected" }),
    ],
    [],
    5
  );

  expect(items).toEqual([
    { event: event({ id: 5, seq: 5, type: "suggestion.rejected" }), folded: true, kind: "event" },
    { event: event({ id: 4, seq: 4, type: "suggestion.accepted" }), folded: true, kind: "event" },
    { event: event({ id: 3, seq: 3, type: "comment.resolved" }), folded: true, kind: "event" },
    { event: event({ id: 2, seq: 2, type: "ask.answered" }), folded: true, kind: "event" },
    { event: event({ id: 1, seq: 1, type: "ask.opened" }), folded: false, kind: "event" },
  ]);
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
