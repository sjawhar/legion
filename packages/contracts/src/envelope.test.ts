import { describe, expect, test } from "bun:test";
import { EnvelopeSchema } from "./envelope";
import { githubResourceSubject, githubSubject } from "./subject";

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
