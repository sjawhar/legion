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

function editedAskEvent(): Extract<Event, { type: "ask.edited" }> {
  return {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: "2026-09-11T00:00:00Z",
      edited_by: { id: "session-1", kind: "session" },
      id: "ask-1",
      issue_key: "CORE-1",
      multiple: false,
      opened_event_id: 1,
      options: [],
      previous: { multiple: false, options: [], question: "Draft?", urgency: "med" },
      question: "Publish?",
      state: "open",
      urgency: "med",
    },
    seq: 2,
    type: "ask.edited",
  };
}

test("eventDescription labels a reopened comment", () => {
  expect(eventDescription(commentEvent("comment.reopened"))).toBe("Comment reopened");
});

test("eventDescription labels an edited comment", () => {
  expect(eventDescription(commentEvent("comment.edited"))).toBe("Comment edited");
});

test("eventDescription labels an edited ask with its new question", () => {
  expect(eventDescription(editedAskEvent())).toBe("Ask edited: Publish?");
});
