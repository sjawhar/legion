import { describe, expect, test } from "bun:test";
import {
  DEFAULT_UPSTREAM_NATS_URL,
  bridgeConfigFromEnvironment,
  envelopeValidation,
} from "./envoy-bridge";

const currentEnvelope = (overrides: Record<string, unknown> = {}) => ({
  event_id: "evt-1",
  source: "github",
  source_event_id: "delivery-1",
  topic: "notifications.github.sjawhar.legion-smoke.pr.1",
  dedupe_key: "github.delivery-1",
  issued_at: 1_700_000_000_000,
  payload_summary: "pull request #1 opened",
  payload: '{"action":"opened"}',
  trace_id: "trace-1",
  ...overrides,
});

describe("bridgeConfigFromEnvironment", () => {
  test("relays only the sandbox repository's GitHub subjects", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "sjawhar/legion-smoke",
      SMOKE_RIG_NATS: "nats://172.30.0.1:31000",
    });
    expect(config).toEqual({
      repository: "sjawhar/legion-smoke",
      subjects: ["notifications.github.sjawhar.legion-smoke.>"],
      upstreamUrl: DEFAULT_UPSTREAM_NATS_URL,
      downstreamUrl: "nats://172.30.0.1:31000",
    });
  });

  test("never subscribes to Dispatch issue subjects (two rigs on one project cross-admit)", () => {
    const config = bridgeConfigFromEnvironment({ SMOKE_REPO: "a/b", SMOKE_RIG_NATS: "nats://x:1" });
    expect(config.subjects.some((s) => s.startsWith("notifications.dispatch"))).toBe(false);
  });

  test("honours SMOKE_UPSTREAM_NATS", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: "nats://x:1",
      SMOKE_UPSTREAM_NATS: "nats://upstream.example:4222",
    });
    expect(config.upstreamUrl).toBe("nats://upstream.example:4222");
  });

  test("refuses a repository that could broaden the NATS subscription", () => {
    for (const repo of ["example-org/>", "a/*", "a.b", "", "a/b/c"]) {
      expect(() => bridgeConfigFromEnvironment({ SMOKE_REPO: repo, SMOKE_RIG_NATS: "nats://x:1" })).toThrow(
        /SMOKE_REPO/
      );
    }
  });

  test("requires SMOKE_RIG_NATS", () => {
    expect(() => bridgeConfigFromEnvironment({ SMOKE_REPO: "a/b" })).toThrow(/SMOKE_RIG_NATS/);
  });
});

describe("envelopeValidation", () => {
  test("accepts a current envelope as bytes or text", () => {
    const text = JSON.stringify(currentEnvelope());
    expect(envelopeValidation(new TextEncoder().encode(text))).toEqual({ ok: true, shape: "current" });
    expect(envelopeValidation(text)).toEqual({ ok: true, shape: "current" });
  });

  test("names every missing and extra field of a legacy envelope", () => {
    const legacy: Record<string, unknown> = currentEnvelope({ legacy_payload: "old field" });
    delete legacy.event_id;
    delete legacy.payload_summary;
    expect(envelopeValidation(JSON.stringify(legacy))).toMatchObject({
      ok: false,
      missing: ["event_id", "payload_summary"],
      extra: ["legacy_payload"],
    });
  });

  test("rejects malformed JSON", () => {
    expect(envelopeValidation(new TextEncoder().encode("{not json"))).toMatchObject({
      ok: false,
      missing: [],
      extra: [],
      errors: [expect.stringContaining("invalid JSON")],
    });
  });
});
