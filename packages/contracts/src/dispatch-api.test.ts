import { expect, test } from "bun:test";
import {
  type Actor,
  type Anchor,
  type AnchorInput,
  AskEditedEventPayloadSchema,
  AskEventPayloadSchema,
  type Comment,
  CommentEventPayloadSchema,
  type CreateProjectInput,
  type DispatchEvent,
  DispatchEventSchema,
  type EditCommentInput,
  IssueEventPayloadSchema,
  MessageEventPayloadSchema,
} from "./dispatch-api";

test("accepts the typed artifact version event payload", () => {
  const event = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-09T00:00:00Z",
    id: 42,
    issue_key: "DSP-1",
    notify: true,
    payload: {
      artifact_id: "artifact-1",
      name: "spec.md",
      version: { number: 2, summary: null },
    },
    seq: 7,
    type: "artifact.version",
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({ success: true });
});

test("preserves labels on an issue update event payload", () => {
  expect(IssueEventPayloadSchema.parse({ labels: ["frontend", "urgent"] })).toEqual({
    labels: ["frontend", "urgent"],
  });
});

test("requires a positive opened event id on an ask event payload", () => {
  expect(AskEventPayloadSchema.safeParse({ question: "Q" }).success).toBe(false);
  expect(AskEventPayloadSchema.safeParse({ opened_event_id: 0, question: "Q" }).success).toBe(
    false
  );
  expect(AskEventPayloadSchema.safeParse({ opened_event_id: 12, question: "Q" }).success).toBe(
    true
  );
});

test("accepts an artifact-owned ask edit event", () => {
  const event: DispatchEvent = {
    id: 72,
    issue_key: null,
    artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
    project: "CORE",
    seq: 4,
    type: "ask.edited",
    actor: { kind: "session", id: "session-1" },
    notify: false,
    created_at: "2026-09-11T04:05:29Z",
    payload: {
      id: "ask-1",
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      author: { kind: "session", id: "session-1" },
      kind: "question",
      question: "Publish?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 3,
      created_at: "2026-09-11T04:00:00Z",
      edited_at: "2026-09-11T04:05:29Z",
      previous: {
        question: "Draft?",
        options: [{ label: "Yes" }],
        multiple: false,
        urgency: "med",
      },
      edited_by: { kind: "session", id: "session-1" },
    },
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({
    data: {
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      issue_key: null,
      type: "ask.edited",
    },
    success: true,
  });
});

test("rejects edit history on non-edit ask events", () => {
  expect(
    AskEventPayloadSchema.safeParse({
      opened_event_id: 7,
      question: "Which API?",
      previous: {
        question: "Which transport?",
        options: [{ label: "HTTP" }],
        multiple: false,
        urgency: "med",
      },
    }).success
  ).toBe(false);
});

test("permits the actor supplied with an agent project creation request", () => {
  const input: CreateProjectInput = {
    actor: { id: "session-1", kind: "session" },
    key: "DSP",
    name: "Dispatch",
  };

  expect(input).toEqual({
    actor: { id: "session-1", kind: "session" },
    key: "DSP",
    name: "Dispatch",
  });
});

test("models returned anchors and input selectors by mark id", () => {
  const anchor: Anchor = {
    artifact_id: "artifact-1",
    mark_id: "mark-1",
    version: 2,
    quote: "selected text",
    orphaned: false,
  };
  const quoteInput: AnchorInput = {
    artifact: "spec",
    quote: "selected text",
    occurrence: 1,
  };
  const markInput: AnchorInput = { artifact: "spec", mark_id: "mark-1" };

  expect({ anchor, quoteInput, markInput }).toEqual({
    anchor: {
      artifact_id: "artifact-1",
      mark_id: "mark-1",
      version: 2,
      quote: "selected text",
      orphaned: false,
    },
    quoteInput: { artifact: "spec", quote: "selected text", occurrence: 1 },
    markInput: { artifact: "spec", mark_id: "mark-1" },
  });
});

test("models comment thread lifecycle events and author edits", () => {
  const actor: Actor = { id: "alice", kind: "user" };
  const comment: Comment = {
    anchor: null,
    author: actor,
    body: "Edited discussion",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: "2026-09-10T00:02:00Z",
    id: "comment-1",
    issue_key: "DSP-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ask_id: null,
  };
  const input: EditCommentInput = { body: "Edited discussion" };
  const reopened: DispatchEvent = {
    actor,
    created_at: "2026-09-10T00:03:00Z",
    id: 43,
    issue_key: "DSP-1",
    notify: true,
    payload: { ...comment, artifact_name: "" },
    seq: 8,
    type: "comment.reopened",
  };
  const edited: DispatchEvent = {
    actor,
    created_at: "2026-09-10T00:04:00Z",
    id: 44,
    issue_key: "DSP-1",
    notify: true,
    payload: { ...comment, artifact_name: "" },
    seq: 9,
    type: "comment.edited",
  };

  expect(DispatchEventSchema.safeParse(reopened)).toMatchObject({ success: true });
  expect(DispatchEventSchema.safeParse(edited)).toMatchObject({ success: true });
  expect(input).toEqual({ body: "Edited discussion" });
});

test("keeps the comment id, author, and message id when parsing event payloads", () => {
  const actor: Actor = { id: "alice", kind: "user" };
  const comment: Comment = {
    anchor: null,
    author: actor,
    body: "Please update this.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "DSP-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ask_id: null,
  };

  expect(CommentEventPayloadSchema.parse({ ...comment, artifact_name: "spec.md" })).toMatchObject({
    id: "comment-1",
    author: actor,
    created_at: "2026-09-10T00:00:00Z",
  });
  expect(
    MessageEventPayloadSchema.parse({
      id: "message-1",
      issue_key: "DSP-1",
      author: actor,
      body: "The build is green.",
      created_at: "2026-09-10T00:00:00Z",
    })
  ).toMatchObject({ id: "message-1", author: actor });
});

test("parses a message reply's reply_to and reply_body preview", () => {
  const actor: Actor = { id: "bob", kind: "user" };
  expect(
    MessageEventPayloadSchema.parse({
      id: "message-2",
      issue_key: "DSP-1",
      author: actor,
      body: "Sounds good.",
      reply_to: "message-1",
      reply_body: "The build is green.",
      created_at: "2026-09-10T00:00:01Z",
    })
  ).toMatchObject({ reply_to: "message-1", reply_body: "The build is green." });
  expect(
    MessageEventPayloadSchema.parse({ id: "message-1", author: actor, body: "Root message." })
      .reply_to
  ).toBeUndefined();
});

test("preserves ask edit history in the event payload", () => {
  const payload = {
    opened_event_id: 7,
    question: "Which transport should we implement?",
    options: [{ label: "REST" }, { label: "gRPC" }],
    multiple: true,
    urgency: "high",
    answer: null,
    edited_at: "2026-09-11T03:26:00Z",
    previous: {
      question: "Which implementation?",
      options: [{ label: "HTTP" }, { label: "MCP" }],
      multiple: false,
      urgency: "low",
    },
    edited_by: { kind: "session", id: "session-1" },
  };

  expect(AskEditedEventPayloadSchema.parse(payload)).toEqual(payload);
});

test("requires previous fields on ask edit events", () => {
  expect(
    AskEditedEventPayloadSchema.safeParse({
      opened_event_id: 7,
      question: "Which API?",
      options: [],
      multiple: false,
      urgency: "med",
      edited_at: "2026-09-11T03:26:00Z",
      edited_by: { kind: "session", id: "session-1" },
    }).success
  ).toBe(false);
});

test("accepts a document event with an unlinked owner", () => {
  expect(
    DispatchEventSchema.safeParse({
      id: 12,
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      project: "CORE",
      seq: 4,
      type: "artifact.version",
      actor: { kind: "session", id: "session-1" },
      notify: true,
      created_at: "2026-09-11T04:05:29Z",
      payload: {},
    }).success
  ).toBe(true);
});
