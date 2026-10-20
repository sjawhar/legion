import { expect, test } from "bun:test";

import type { Event } from "../../api/types";
import { buildActivityItems, dateKey } from "./activity-model";

function message(seq: number, at: string): Event {
  const actor = { id: "alice", kind: "user" as const };
  return {
    actor,
    created_at: at,
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: actor,
      body: `Message ${seq}`,
      created_at: at,
      deliveries: [],
      id: `message-${seq}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq,
    type: "message.created",
  };
}

// The day a line falls on is its local calendar day, so the fixtures are built from local
// components rather than from UTC instants that could land on either side of midnight.
test("day dividers name Today and Yesterday and spell an older day in full", () => {
  const today = new Date(2026, 8, 10, 12, 0, 0);
  const yesterday = new Date(2026, 8, 9, 12, 0, 0);
  const older = new Date(2025, 0, 3, 12, 0, 0);
  const items = buildActivityItems({
    events: [
      message(1, older.toISOString()),
      message(2, yesterday.toISOString()),
      message(3, today.toISOString()),
    ],
    lastReadSeq: 3,
    today: dateKey(today.toISOString()),
  });

  expect(items.flatMap((item) => (item.kind === "day-divider" ? [item.label] : []))).toEqual([
    "Today",
    "Yesterday",
    // A day in another year carries that year; the two days above carry none.
    new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "long",
      weekday: "long",
      year: "numeric",
    }).format(older),
  ]);
});
