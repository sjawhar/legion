import { describe, expect, test } from "bun:test";
import { toEnvelopeDisplay } from "../display";

describe("toEnvelopeDisplay", () => {
  test("normalizes a valid wire envelope without choosing harness prompt text", () => {
    const display = toEnvelopeDisplay({
      event_id: "event-1",
      source: "github",
      source_event_id: "github-1",
      source_session: "ses_sender",
      topic: "notifications.github.legion.legion.issue.1.comment",
      dedupe_key: "github-1",
      issued_at: 1,
      payload_summary: "A comment arrived",
      trace_id: "trace-1",
    });

    expect(display).toEqual({
      eventID: "event-1",
      source: "github",
      sourceSessionID: "ses_sender",
      topic: "notifications.github.legion.legion.issue.1.comment",
      summary: "A comment arrived",
      dedupeKey: "github-1",
      issuedAt: 1,
      traceID: "trace-1",
    });
  });
});
