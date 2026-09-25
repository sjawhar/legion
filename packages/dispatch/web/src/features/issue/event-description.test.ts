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
      kind: "question",
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

test("eventDescription labels a malformed decision", () => {
  const event: Extract<Event, { type: "block.invalid" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-13T17:00:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      block_id: "ask-block",
      version: 3,
      reason: 'ask block "ask-block" has an option without a label',
      disturbed_by: { id: "session-1", kind: "session" },
    },
    seq: 3,
    type: "block.invalid",
  };

  expect(eventDescription(event)).toBe(
    'marked decision ask-block malformed: ask block "ask-block" has an option without a label'
  );
});

test("eventDescription names an added artifact", () => {
  const event: Extract<Event, { type: "artifact.created" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-15T00:00:00Z",
    id: 4,
    issue_key: "OPS-52",
    notify: false,
    payload: {
      artifact: {
        created_at: "2026-09-15T00:00:00Z",
        created_by: { id: "session-1", kind: "session" },
        id: "artifact-1",
        issue_key: "OPS-52",
        kind: "doc",
        name: "cu-update-2026-09-15.md",
        primary: false,
        project: "OPS",
        slug: "cu-update-2026-09-15-md",
        versions: [],
      },
    },
    seq: 4,
    type: "artifact.created",
  };

  expect(eventDescription(event)).toBe("Added cu-update-2026-09-15.md");
});

test("eventDescription names a saved artifact version", () => {
  const event: Extract<Event, { type: "artifact.version" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-15T00:00:00Z",
    id: 5,
    issue_key: "OPS-52",
    notify: false,
    payload: {
      artifact_id: "artifact-1",
      name: "cu-update-2026-09-15.md",
      version: {
        authors: [{ id: "session-1", kind: "session" }],
        created_at: "2026-09-15T00:00:00Z",
        named: false,
        number: 2,
        summary: null,
      },
    },
    seq: 5,
    type: "artifact.version",
  };

  expect(eventDescription(event)).toBe("Saved cu-update-2026-09-15.md v2");
});

test("eventDescription names the child a reparent adds or removes", () => {
  const base = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-16T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: true,
    payload: { child_key: "CORE-12" },
    seq: 2,
  };
  expect(eventDescription({ ...base, type: "child.added" } as Event)).toBe("Added child CORE-12");
  expect(eventDescription({ ...base, type: "child.removed" } as Event)).toBe(
    "Removed child CORE-12"
  );
});

// The issue log names the same five movements, in its own voice: it has no actor column beside
// the line, so a release says what happened and a takeover names who lost the claim.
type ClaimLogEvent = Extract<Event, { type: "issue.claimed" | "issue.released" }>;

const claimLogEvent = (
  type: ClaimLogEvent["type"],
  payload: Partial<ClaimLogEvent["payload"]>
): ClaimLogEvent =>
  ({
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-24T07:00:00Z",
    id: 9,
    issue_key: "CORE-1",
    notify: false,
    payload: { key: "CORE-1", status: "in_progress", claim: null, ...payload },
    project: "CORE",
    seq: 9,
    type,
  }) as ClaimLogEvent;

const held = {
  actor: { id: "session-one", kind: "session" as const, origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

test.each([
  [
    "a first claim",
    claimLogEvent("issue.claimed", { claim: held, reason: "claimed" }),
    "Issue claimed",
  ],
  [
    "a takeover",
    claimLogEvent("issue.claimed", { claim: held, previous_claim: held, reason: "takeover" }),
    "Claim taken from Implementer",
  ],
  [
    "a release",
    claimLogEvent("issue.released", { previous_claim: held, reason: "released" }),
    "Claim released",
  ],
  [
    "a close",
    claimLogEvent("issue.released", { previous_claim: held, reason: "closed" }),
    "Claim released on close",
  ],
])("the issue log line for %s", (_name, event, expected) => {
  expect(eventDescription(event)).toBe(expected);
});
