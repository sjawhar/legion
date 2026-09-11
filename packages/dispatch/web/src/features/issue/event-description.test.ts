import { expect, test } from "bun:test";

import type { Event } from "../../api/types";
import { eventDescription } from "./event-description";

function commentEvent(type: "comment.reopened" | "comment.edited"): Event {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Updated wording",
      created_at: "2026-09-11T00:00:00Z",
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
    },
    seq: 1,
    type,
  } as Event;
}

test("eventDescription labels a reopened comment", () => {
  expect(eventDescription(commentEvent("comment.reopened"))).toBe("Comment reopened");
});

test("eventDescription labels an edited comment", () => {
  expect(eventDescription(commentEvent("comment.edited"))).toBe("Comment edited");
});
