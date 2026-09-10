import { expect, test } from "bun:test";

import type { Event } from "../api/types";
import { fetchPinnedEvents } from "../features/issue/BoardStrip";

function event(id: string): Event {
  const seq = Number(id);
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: "Update",
      created_at: "2026-09-09T00:00:00Z",
      id: `message-${id}`,
      issue_key: "CORE-1",
    },
    seq,
    type: "message.created",
  };
}

test("pinned lookup chunks 51 ids and combines results by sequence", async () => {
  const ids = Array.from({ length: 51 }, (_, index) => String(index + 1));
  const requests: string[][] = [];

  const events = await fetchPinnedEvents(
    async (_issueKey, options) => {
      const requestedIds = options.ids ?? [];
      requests.push(requestedIds);
      return requestedIds.map(event);
    },
    "CORE-1",
    ids
  );

  expect(requests).toEqual([ids.slice(0, 50), ids.slice(50)]);
  expect(events.map(({ seq }) => seq)).toEqual(ids.map(Number));
});
