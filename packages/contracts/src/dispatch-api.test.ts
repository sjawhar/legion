import { expect, test } from "bun:test";
import {
  type Actor,
  type Anchor,
  type AnchorInput,
  type Comment,
  type CreateProjectInput,
  type DispatchEvent,
  DispatchEventSchema,
  type EditCommentInput,
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
