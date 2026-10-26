import { expect, test } from "bun:test";

import type { Event } from "../../api/types";
import { activityDescription, buildActivityItems, dateKey } from "./activity-model";

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

const projectEvent = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-10-25T00:00:00Z",
  id: 1,
  issue_key: null,
  notify: false,
  project: "CORE",
  seq: 1,
} as const;

test("an architecture import names the repository it came from; a retained event without one reads as before", () => {
  const commit = "3be61df5be90aa11bb22";
  const error = "duplicate component id api";
  expect(
    activityDescription({
      ...projectEvent,
      payload: { branch: "main", commit, components: 3, repo: "acme/infra" },
      type: "architecture.synced",
    })
  ).toBe("synced the architecture model from acme/infra at 3be61df5be90");
  expect(
    activityDescription({
      ...projectEvent,
      payload: { branch: "main", error, repo: "acme/infra" },
      type: "architecture.sync_failed",
    })
  ).toBe("failed to sync the architecture model from acme/infra: duplicate component id api");
  expect(
    activityDescription({
      ...projectEvent,
      payload: { commit, components: 3 },
      type: "architecture.synced",
    })
  ).toBe("synced the architecture model at 3be61df5be90");
  expect(
    activityDescription({ ...projectEvent, payload: { error }, type: "architecture.sync_failed" })
  ).toBe("failed to sync the architecture model: duplicate component id api");
});
