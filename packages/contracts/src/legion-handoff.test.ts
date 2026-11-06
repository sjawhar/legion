import { describe, expect, test } from "bun:test";
import { LegionAgentHandoffSchema, legionAgentHandoffSubject } from "./legion-handoff";
import { agentSubject } from "./subject";

const dispatchFrame = JSON.stringify({
  event: {
    id: 41,
    issue_key: "LEGION-9",
    artifact_id: null,
    project: "LEGION",
    seq: 7,
    type: "message.created",
    actor: { kind: "user", id: "person@example.com" },
    notify: true,
    created_at: "2026-10-26T12:00:00Z",
    payload: {
      id: "0b8f1c1e-1111-4222-8333-944455556666",
      body: "Why plum?",
      target: "role:tester",
    },
  },
  delivery: { attempt: 1, mode: "btw" },
});

const handoff = (overrides: Record<string, unknown> = {}) => ({
  original_topic: agentSubject("ses_tester"),
  target_session: "ses_tester",
  role_token: "legion-legion-legion-9-tester",
  project: "legion",
  envelope: {
    event_id: "evt-1",
    source: "dispatch",
    source_event_id: "src-1",
    topic: agentSubject("ses_tester"),
    dedupe_key: "agent.ses_tester.0b8f1c1e-1111-4222-8333-944455556666:btw",
    issued_at: 1_790_000_000_000,
    payload_summary: "Why plum?",
    payload: dispatchFrame,
    trace_id: "trace-1",
  },
  ...overrides,
});

describe("LegionAgentHandoffSchema", () => {
  test("accepts a hand-off carrying the envelope as it would have been delivered", () => {
    const parsed = LegionAgentHandoffSchema.parse(handoff());
    expect(parsed.target_session).toBe("ses_tester");
    expect(JSON.parse(parsed.envelope.payload ?? "{}").event.type).toBe("message.created");
  });

  test("refuses a hand-off missing its target, its role token or its envelope", () => {
    for (const field of ["target_session", "role_token", "project", "envelope"]) {
      const value = handoff();
      delete (value as Record<string, unknown>)[field];
      expect(LegionAgentHandoffSchema.safeParse(value).success).toBe(false);
    }
  });

  test("refuses an envelope Envoy would not deliver and a field the contract does not name", () => {
    expect(
      LegionAgentHandoffSchema.safeParse(handoff({ envelope: { event_id: "evt-1" } })).success
    ).toBe(false);
    expect(LegionAgentHandoffSchema.safeParse(handoff({ extra: true })).success).toBe(false);
  });
});

describe("legionAgentHandoffSubject", () => {
  test("is the project's agents subject the daemon's consumer reads", () => {
    expect(legionAgentHandoffSubject("legion", "ses_tester")).toBe(
      "notifications.legion.legion.agents.ses_tester"
    );
  });
});
