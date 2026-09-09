import { expect, test } from "bun:test";
import type { Event } from "../api/types";
import {
  buildLogItems,
  dismissEvent,
  isPinnedEvent,
  pinnedEventIds,
  setEventPinned,
} from "../features/issue/log-model";

function event(overrides: Partial<Event> = {}): Event {
  return {
    actor: { kind: "session", id: "session-1" },
    created_at: "2026-09-09T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {},
    seq: 1,
    type: "message.created",
    ...overrides,
  };
}

test("log model reverses server events and folds answered asks and resolved comments", () => {
  const items = buildLogItems(
    [
      event({ id: 1, seq: 1, type: "ask.opened" }),
      event({ id: 2, seq: 2, type: "ask.answered" }),
      event({ id: 3, seq: 3, type: "comment.resolved" }),
    ],
    [],
    3
  );

  expect(items).toEqual([
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

test("log model stores pinned event ids alongside dismissed ids", () => {
  const source = event({ id: 7 });
  const pinned = setEventPinned(["event:2"], source, true);

  expect(isPinnedEvent(pinned, source)).toBe(true);
  expect(pinnedEventIds(pinned)).toEqual(["event:7"]);
  expect(dismissEvent(pinned, source)).toEqual(["event:2", "pinned_items:event:7", "event:7"]);
});
