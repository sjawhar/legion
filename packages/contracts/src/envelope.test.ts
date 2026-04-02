import { describe, expect, test } from "bun:test";
import { EnvelopeSchema } from "./envelope";

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
