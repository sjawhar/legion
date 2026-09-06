import { describe, expect, test } from "bun:test";
import type { Envelope } from "@legion/contracts";
import { isOwnDispatchEcho, replyWith, senderLabel } from "../delivery";

function envelope(overrides: Partial<Envelope> = {}): Envelope {
  return {
    event_id: "event-1",
    source: "agent",
    source_event_id: "agent.ses_sender.event-1",
    topic: "notifications.agent.ses_target",
    dedupe_key: "agent.ses_target.event-1",
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-1",
    ...overrides,
  };
}

describe("inbound delivery policy", () => {
  test("labels agent, human, and other envelopes by session before source", () => {
    expect(senderLabel(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      "ses_agent"
    );
    expect(senderLabel(envelope({ source: "agent" }))).toBe("agent");
    expect(senderLabel(envelope({ source: "human", source_session: "ses_human" }))).toBe(
      "ses_human"
    );
    expect(senderLabel(envelope({ source: "human" }))).toBe("human");
    expect(senderLabel(envelope({ source: "github" }))).toBe("github");
  });

  test("offers a reply hint only for an agent envelope with a source session", () => {
    expect(replyWith(envelope({ source: "agent", source_session: "ses_agent" }))).toBe(
      'envoy_send(session_id="ses_agent", message="...")'
    );
    expect(replyWith(envelope({ source: "agent" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human", source_session: "ses_human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "human" }))).toBeUndefined();
    expect(replyWith(envelope({ source: "github", source_session: "ses_github" }))).toBeUndefined();
  });

  test("recognizes only a GitHub dispatch echo for the matching session", () => {
    const matching = envelope({
      source: "github",
      payload: JSON.stringify({ dispatch_session: "ses_reader" }),
    });

    expect(isOwnDispatchEcho(matching, "ses_reader")).toBe(true);
    expect(isOwnDispatchEcho(matching, "ses_other")).toBe(false);
    expect(
      isOwnDispatchEcho(
        envelope({ source: "github", payload: JSON.stringify({ kind: "comment" }) }),
        "ses_reader"
      )
    ).toBe(false);
    expect(
      isOwnDispatchEcho(
        envelope({ source: "agent", payload: JSON.stringify({ dispatch_session: "ses_reader" }) }),
        "ses_reader"
      )
    ).toBe(false);
  });
});
