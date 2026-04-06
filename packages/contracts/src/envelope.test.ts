import { describe, expect, test } from "bun:test";
import { EnvelopeSchema } from "./envelope";
import {
  GHOSTWISPR_TOPIC_PREFIX,
  ghostWisprSubject,
  githubResourceSubject,
  githubSubject,
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
