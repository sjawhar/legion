import { expect, test } from "bun:test";

import { eventRefreshTargets } from "../../api/event-refresh";
import type { Event } from "../../api/types";

for (const type of [
  "comment.created",
  "comment.edited",
  "comment.delivery",
  "message.created",
  "ask.answered",
  "issue.updated",
  "user_thread_state.updated",
] as const) {
  test(`My threads refreshes on ${type} even before the person has an authorship row`, () => {
    const event = {
      actor: { id: "bob", kind: "user" },
      created_at: "2026-11-04T00:00:00Z",
      id: 12,
      issue_key: "CORE-1",
      notify: true,
      payload: {},
      project: "CORE",
      seq: 4,
      type,
    } as Event;
    expect(
      eventRefreshTargets(event, "alice").some((target) => target.queryKey[0] === "my-threads")
    ).toBe(true);
  });
}
