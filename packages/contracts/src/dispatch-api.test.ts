import { expect, test } from "bun:test";
import {
  type Anchor,
  type AnchorInput,
  type CreateProjectInput,
  DispatchEventSchema,
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
