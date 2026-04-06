import { describe, expect, test } from "bun:test";
import { EnvelopeSchema } from "./envelope";
import {
  agentSubject,
  GHOSTWISPR_TOPIC_PREFIX,
  ghostWisprSubject,
  githubResourceSubject,
  githubSubject,
  slackSubject,
  slackThreadSubject,
  whatsappSubject,
} from "./subject";

const buildEnvelope = (overrides: Record<string, unknown> = {}) => ({
  event_id: "evt-1",
  source: "agent",
  source_event_id: "src-1",
  topic: agentSubject("ses_123"),
  dedupe_key: "agent.ses_123.src-1",
  issued_at: Date.now(),
  payload_summary: "hello",
  trace_id: "trace-1",
  ...overrides,
});

const expectInvalidFields = (overrides: Record<string, unknown>, expectedFields: string[]) => {
  const result = EnvelopeSchema.safeParse(buildEnvelope(overrides));

  expect(result.success).toBe(false);
  if (result.success) {
    throw new Error("expected parse to fail");
  }

  expect(result.error.issues.map((issue) => issue.path.join("."))).toEqual(
    expect.arrayContaining(expectedFields)
  );
};

describe("EnvelopeSchema", () => {
  test("accepts the envoy envelope shape", () => {
    const item = EnvelopeSchema.parse(buildEnvelope());

    expect(item).toMatchObject({
      source: "agent",
      topic: agentSubject("ses_123"),
      dedupe_key: "agent.ses_123.src-1",
    });
  });

  test("accepts optional envelope fields", () => {
    const item = EnvelopeSchema.parse(
      buildEnvelope({
        source_session: "ses_456",
        expires_at: Date.now() + 60_000,
        payload_ref: "nats://payloads/evt-1",
      })
    );

    expect(item).toMatchObject({
      source_session: "ses_456",
      payload_ref: "nats://payloads/evt-1",
    });
    expect(typeof item.expires_at).toBe("number");
  });

  test("accepts ghostwispr source", () => {
    const item = EnvelopeSchema.parse(
      buildEnvelope({
        event_id: "evt-2",
        source: "ghostwispr",
        source_event_id: "gw-delivery-1",
        topic: ghostWisprSubject("20260326041405", "session.ended"),
        dedupe_key: "ghostwispr.gw-delivery-1",
        payload_summary: '{"event_type":"session_ended","session_id":"20260326041405"}',
        trace_id: "trace-2",
      })
    );

    expect(item).toMatchObject({
      source: "ghostwispr",
      topic: ghostWisprSubject("20260326041405", "session.ended"),
    });
  });

  test("rejects unknown source with a source validation issue", () => {
    expectInvalidFields({ source: "unknown" }, ["source"]);
  });

  test("rejects empty required string fields", () => {
    expectInvalidFields({ topic: "" }, ["topic"]);
  });

  test("rejects non-integer timestamps", () => {
    expectInvalidFields({ issued_at: 1.5, expires_at: 2.5 }, ["issued_at", "expires_at"]);
  });
});

describe("githubResourceSubject", () => {
  test("returns base resource subject for PR", () => {
    expect(githubResourceSubject("acme", "widgets", "pr", 42)).toBe(
      "notifications.github.acme.widgets.pr.42"
    );
  });

  test("returns base resource subject for issue", () => {
    expect(githubResourceSubject("sjawhar", "legion", "issue", 185)).toBe(
      "notifications.github.sjawhar.legion.issue.185"
    );
  });

  test("accepts string resource number", () => {
    expect(githubResourceSubject("acme", "widgets", "pr", "99")).toBe(
      "notifications.github.acme.widgets.pr.99"
    );
  });

  test("is consistent with githubSubject prefix", () => {
    const resource = githubResourceSubject("acme", "widgets", "pr", 7);
    const repoLevel = githubSubject("acme", "widgets", "pr");
    expect(resource.startsWith(`${repoLevel}.`)).toBe(true);
  });
});

describe("ghostWisprSubject", () => {
  test("returns session.ended topic", () => {
    expect(ghostWisprSubject("20260326041405", "session.ended")).toBe(
      "notifications.ghostwispr.20260326041405.session.ended"
    );
  });

  test("returns summary.ready topic", () => {
    expect(ghostWisprSubject("20260326041629", "summary.ready")).toBe(
      "notifications.ghostwispr.20260326041629.summary.ready"
    );
  });

  test("returns session.started topic", () => {
    expect(ghostWisprSubject("20260326041405", "session.started")).toBe(
      "notifications.ghostwispr.20260326041405.session.started"
    );
  });

  test("starts with GHOSTWISPR_TOPIC_PREFIX", () => {
    const topic = ghostWisprSubject("20260326041405", "session.ended");
    expect(topic.startsWith(GHOSTWISPR_TOPIC_PREFIX)).toBe(true);
  });
});

describe("whatsappSubject", () => {
  test("returns message topic", () => {
    expect(whatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message")).toBe(
      "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message"
    );
  });

  test("returns different kind", () => {
    expect(whatsappSubject("15551234567", "5551234567@s.whatsapp.net", "status")).toBe(
      "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.status"
    );
  });

  test("accepts whatsapp source in envelope", () => {
    const item = EnvelopeSchema.parse(
      buildEnvelope({
        event_id: "evt-wa",
        source: "whatsapp",
        source_event_id: "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
        topic: whatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message"),
        dedupe_key: "whatsapp.15551234567.5551234567@s.whatsapp.net.1712345678000",
        payload_summary: "WhatsApp message in chat 5551234567@s.whatsapp.net",
        trace_id: "trace-wa",
      })
    );

    expect(item.source).toBe("whatsapp");
  });
});

describe("slackThreadSubject", () => {
  test("normalizes thread_ts dot to underscore", () => {
    expect(slackThreadSubject("T123", "C456", "1234567890.123456", "message")).toBe(
      "notifications.slack.T123.C456.thread.1234567890_123456.message"
    );
  });

  test("returns mention kind for app_mention threads", () => {
    expect(slackThreadSubject("T123", "C456", "1234567890.123456", "mention")).toBe(
      "notifications.slack.T123.C456.thread.1234567890_123456.mention"
    );
  });

  test("is consistent with slackSubject prefix", () => {
    const thread = slackThreadSubject("T123", "C456", "1234567890.123456", "message");
    const channel = slackSubject("T123", "C456", "thread");
    expect(thread.startsWith(`${channel}.`)).toBe(true);
  });
});
