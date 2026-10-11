import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  dispatchToolSchema,
  dispatchToolSpecs,
  type Envelope,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import { renderInbound } from "../delivery";
import { executeDispatchTool } from "../dispatch-execute";
import { dispatchFollowNotice } from "../dispatch-subscribe";

// Agents reach Dispatch through the `dispatch` command, so no text an agent reads may name one of
// the native tools it replaced.
const toolNames = new RegExp(`\\b(${dispatchToolSpecs.map((spec) => spec.name).join("|")})\\b`);

const reader = "01a01111-2222-7333-4444-555555555555";
const config = { enabled: true, url: "http://dispatch.test", token: "secret", error: null };
const noNetwork = (async () => {
  throw new Error("the refusal must come before any request");
}) as unknown as typeof fetch;

function requiredFields(spec: (typeof dispatchToolSpecs)[number]): string[] {
  const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as {
    required?: string[];
  };
  return schema.required ?? [];
}

function dispatchFrame(type: string, payload: object, extra: Partial<Envelope> = {}): string {
  return JSON.stringify({
    event_id: "dispatch-1",
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.dispatch.issue.DSP-1.>",
    dedupe_key: "dispatch.1",
    issued_at: 1,
    payload_summary: "Dispatch update",
    trace_id: "trace-1",
    ...extra,
    payload: JSON.stringify({
      id: 1,
      issue_key: "DSP-1",
      seq: 7,
      type,
      actor: { kind: "user", id: "alice" },
      notify: true,
      created_at: "2026-09-09T00:00:00Z",
      payload,
    }),
  } satisfies Envelope);
}

const comment = {
  id: "comment-1",
  issue_key: "DSP-1",
  author: { kind: "session", id: "session-1" },
  body: "Please update this.",
  anchor: { artifact_id: "artifact-1", mark_id: "m-1", quote: "old line", orphaned: false },
  reply_to: "comment-0",
  resolved: false,
  suggestion: null,
  created_at: "2026-09-09T00:00:00Z",
  artifact_name: "spec.md",
};

describe("no text an agent reads names a native Dispatch tool", () => {
  test("every spec's description and argument schema", () => {
    for (const spec of dispatchToolSpecs) {
      const schema = JSON.stringify(z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))));
      expect(spec.description, spec.name).not.toMatch(toolNames);
      expect(schema, spec.name).not.toMatch(toolNames);
    }
  });

  test("every refusal of a spec's example with one required field removed", async () => {
    let refusals = 0;
    for (const spec of dispatchToolSpecs) {
      for (const field of requiredFields(spec)) {
        const { [field]: _removed, ...args } = spec.example as Record<string, unknown>;
        const refusal = await executeDispatchTool({
          tool: spec.name,
          args,
          cwd: "/workspace",
          host: "omp",
          sessionId: "session-1",
          config,
          env: {},
          fetchImpl: noNetwork,
        }).then(
          () => undefined,
          (error: unknown) => (error instanceof Error ? error.message : String(error))
        );
        expect(refusal, `${spec.name} without ${field}`).toBeDefined();
        expect(refusal, `${spec.name} without ${field}`).not.toMatch(toolNames);
        refusals++;
      }
    }
    expect(refusals).toBeGreaterThan(20);
  });

  test("the follow notice, for an issue and for a project document", () => {
    for (const details of [
      { issue: "DSP-1", follows: { ask: "ask-1" } },
      { document: "CORE/design-notes", follows: { ask: "ask-1" } },
    ]) {
      const notice = dispatchFollowNotice(details);
      expect(notice).not.toBeNull();
      expect(notice?.text).not.toMatch(toolNames);
    }
  });

  test("every delivery notice and reply hint", () => {
    const targetedMessage = readFileSync(
      new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
      "utf8"
    );
    const frames = [
      // A human's targeted message, with the reply hint that answers it.
      JSON.stringify({
        event_id: "event-1",
        source: "dispatch",
        source_event_id: "agent.ses_sender.event-1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-1",
        issued_at: 1,
        payload_summary: "message",
        trace_id: "trace-1",
        payload: targetedMessage,
      } satisfies Envelope),
      // A reply to an ask, an ordinary comment, and a comment on a project document.
      dispatchFrame(
        "comment.created",
        { ...comment, ask_id: "ask-1", ask_question: "Which approach?" },
        {
          in_reply_to: "ask-1",
        }
      ),
      dispatchFrame("comment.created", { ...comment, ask_id: null }),
      dispatchFrame(
        "comment.created",
        {
          ...comment,
          issue_key: null,
          ask_id: "ask-1",
          project_key: "CORE",
          artifact_slug: "notes",
        },
        { in_reply_to: "ask-1" }
      ),
      // A human adding this session to an ask's followers.
      dispatchFrame("ask.follower_added", {
        ask_id: "ask-1",
        session_id: reader,
        by: { kind: "user", id: "alice" },
      }),
    ];
    for (const frame of frames) {
      const rendered = renderInbound(frame, reader);
      expect(rendered.skip, frame).toBe(false);
      expect(rendered.content).not.toMatch(toolNames);
    }
  });
});
