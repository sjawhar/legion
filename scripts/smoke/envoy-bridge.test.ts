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
  topic: "notifications.github.trajectory-labs-pbc.legion-smoke.issue.1",
  dedupe_key: "github.delivery-1",
  issued_at: 1_700_000_000_000,
  payload_summary: "issue #1 labeled",
  payload: '{"action":"labeled"}',
  trace_id: "trace-1",
  ...overrides,
});

describe("bridgeConfigFromEnvironment", () => {
  test("limits its upstream subscription to exactly the configured repository", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "trajectory-labs-pbc/legion-smoke",
      SMOKE_RIG_NATS: "nats://127.0.0.1:14222",
    });

    expect(config).toEqual({
      repository: "trajectory-labs-pbc/legion-smoke",
      subject: "notifications.github.trajectory-labs-pbc.legion-smoke.>",
      upstreamUrl: DEFAULT_UPSTREAM_NATS_URL,
      downstreamUrl: "nats://127.0.0.1:14222",
    });
  });

  test("rejects repository input that could broaden the NATS allowlist", () => {
    expect(() =>
      bridgeConfigFromEnvironment({
        SMOKE_REPO: "trajectory-labs-pbc/>",
        SMOKE_RIG_NATS: "nats://127.0.0.1:14222",
      })
    ).toThrow("SMOKE_REPO must be a literal <owner>/<repo> repository");
  });
});

describe("envelopeValidation", () => {
  test("marks the current daemon envelope shape healthy", () => {
    expect(envelopeValidation(JSON.stringify(currentEnvelope()))).toEqual({
      valid: true,
      shape: "current",
    });
  });

  test("reports every missing and extra field before rejecting a legacy envelope", () => {
    const legacy = currentEnvelope({
      event_id: undefined,
      payload_summary: undefined,
      legacy_payload: "old envelope field",
    });
    delete legacy.event_id;
    delete legacy.payload_summary;

    const result = envelopeValidation(JSON.stringify(legacy));

    expect(result).toMatchObject({
      valid: false,
      missing: ["event_id", "payload_summary"],
      extra: ["legacy_payload"],
    });
  });

  test("rejects malformed data before it can reach the daemon", () => {
    expect(envelopeValidation("not-json")).toMatchObject({
      valid: false,
      missing: [],
      extra: [],
      errors: [expect.stringContaining("invalid JSON")],
    });
  });
});
