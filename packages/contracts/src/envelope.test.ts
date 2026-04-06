import { describe, expect, test } from "bun:test";
import { EnvelopeSchema } from "./envelope";
import {
  GHOSTWISPR_TOPIC_PREFIX,
  ghostWisprSubject,
  githubResourceSubject,
  githubSubject,
  slackSubject,
  slackThreadSubject,
  whatsappSubject,
} from "./subject";

describe("EnvelopeSchema", () => {
  test("accepts the envoy envelope shape", () => {
    const item = EnvelopeSchema.parse({
      event_id: "evt-1",
      source: "agent",
      source_event_id: "src-1",
      topic: "notifications.agent.ses_123",
      dedupe_key: "agent.ses_123.src-1",
      issued_at: Date.now(),
      payload_summary: "hello",
      trace_id: "trace-1",
    });

    expect(item.topic).toBe("notifications.agent.ses_123");
  });

  test("accepts ghostwispr source", () => {
    const item = EnvelopeSchema.parse({
      event_id: "evt-2",
      source: "ghostwispr",
      source_event_id: "rec-abc123",
      topic: "notifications.ghostwispr.rec-abc123.transcript",
      dedupe_key: "ghostwispr.rec-abc123",
      issued_at: Date.now(),
      payload_summary: '{"type":"transcript"}',
      trace_id: "trace-2",
    });

    expect(item.source).toBe("ghostwispr");
  });

  test("rejects unknown source", () => {
    expect(() =>
      EnvelopeSchema.parse({
        event_id: "evt-3",
        source: "unknown",
        source_event_id: "src-3",
        topic: "notifications.test.foo",
        dedupe_key: "test.src-3",
        issued_at: Date.now(),
        payload_summary: "hello",
        trace_id: "trace-3",
      })
    ).toThrow();
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
  test("returns transcript topic", () => {
    expect(ghostWisprSubject("rec-abc123", "transcript")).toBe(
      "notifications.ghostwispr.rec-abc123.transcript"
    );
  });

  test("returns summary topic", () => {
    expect(ghostWisprSubject("rec-abc123", "summary")).toBe(
      "notifications.ghostwispr.rec-abc123.summary"
    );
  });

  test("starts with GHOSTWISPR_TOPIC_PREFIX", () => {
    const topic = ghostWisprSubject("rec-1", "transcript");
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
    const item = EnvelopeSchema.parse({
      event_id: "evt-wa",
      source: "whatsapp",
      source_event_id: "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
      topic: whatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message"),
      dedupe_key: "whatsapp.15551234567.5551234567@s.whatsapp.net.1712345678000",
      issued_at: Date.now(),
      payload_summary: "WhatsApp message in chat 5551234567@s.whatsapp.net",
      trace_id: "trace-wa",
    });

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
