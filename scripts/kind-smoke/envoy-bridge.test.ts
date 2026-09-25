import { describe, expect, test } from "bun:test";
import { bridgeConfigFromEnvironment, envelopeValidation, redactUpstream } from "./envoy-bridge";

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
      SMOKE_UPSTREAM_NATS: "nats://envoy-nats.tailnet.example:4222",
    });
    expect(config).toEqual({
      repository: "sjawhar/legion-smoke",
      subjects: ["notifications.github.sjawhar.legion-smoke.>"],
      upstreamUrl: "nats://envoy-nats.tailnet.example:4222",
      downstreamUrl: "nats://172.30.0.1:31000",
    });
  });

  test("never subscribes to Dispatch issue subjects (two rigs on one project cross-admit)", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: "nats://x:1",
      SMOKE_UPSTREAM_NATS: "nats://y.example:1",
    });
    expect(config.subjects.some((s) => s.startsWith("notifications.dispatch"))).toBe(false);
  });

  test("refuses a repository that could broaden the NATS subscription", () => {
    for (const repo of ["example-org/>", "a/*", "a.b", "", "a/b/c"]) {
      expect(() =>
        bridgeConfigFromEnvironment({
          SMOKE_REPO: repo,
          SMOKE_RIG_NATS: "nats://x:1",
          SMOKE_UPSTREAM_NATS: "nats://y.example:1",
        })
      ).toThrow(/SMOKE_REPO/);
    }
  });

  test("requires SMOKE_RIG_NATS", () => {
    expect(() =>
      bridgeConfigFromEnvironment({ SMOKE_REPO: "a/b", SMOKE_UPSTREAM_NATS: "nats://y.example:1" })
    ).toThrow(/SMOKE_RIG_NATS/);
  });

  test("requires SMOKE_UPSTREAM_NATS: the operator names the production NATS, it has no default", () => {
    for (const upstream of [undefined, "", "  "]) {
      expect(() =>
        bridgeConfigFromEnvironment({
          SMOKE_REPO: "a/b",
          SMOKE_RIG_NATS: "nats://x:1",
          SMOKE_UPSTREAM_NATS: upstream,
        })
      ).toThrow("SMOKE_UPSTREAM_NATS is required");
    }
  });

  test("refuses an upstream that is not one NATS URL naming a fully-qualified host, never echoing it", () => {
    for (const [upstream, canary] of [
      ["nats://canary-a:4222", "canary-a"],
      ["canary-b:4222", "canary-b"],
      ["nats://user:canary-c@canary-d", "canary-"],
      // the client dials what follows the last "://", so nothing may follow the authority
      ["nats://ok.example/nats://canary-e:4222", "canary-e"],
      ["nats://ok.example:4222?x=nats://canary-f:4222", "canary-f"],
      ["nats://ok.example:4222#nats://canary-g:4222", "canary-g"],
      ["nats://canary-h.example:4222,nats://canary-i.example:4222", "canary-"],
      ["nats://canary%zz.example:4222", "canary"],
      ["::", "::"],
    ] as const) {
      const refusal = (() => {
        try {
          bridgeConfigFromEnvironment({
            SMOKE_REPO: "a/b",
            SMOKE_RIG_NATS: "nats://x:1",
            SMOKE_UPSTREAM_NATS: upstream,
          });
        } catch (error) {
          return error instanceof Error ? error.message : String(error);
        }
        return "accepted";
      })();
      expect(refusal).toStartWith(
        "SMOKE_UPSTREAM_NATS is not one NATS URL naming a fully-qualified host"
      );
      expect(refusal).not.toContain(canary);
    }
  });

  test("accepts one fully-qualified upstream, trimmed, with or without a scheme", () => {
    for (const [upstream, want] of [
      [" nats://envoy-nats.tailnet.example:4222 ", "nats://envoy-nats.tailnet.example:4222"],
      ["\tnats://envoy-nats.tailnet.example:4222/\n", "nats://envoy-nats.tailnet.example:4222/"],
      ["envoy-nats.tailnet.example:4222", "envoy-nats.tailnet.example:4222"],
      ["tls://user:pw@envoy-nats.tailnet.example", "tls://user:pw@envoy-nats.tailnet.example"],
      ["nats://127.0.0.1:4222", "nats://127.0.0.1:4222"],
    ]) {
      const config = bridgeConfigFromEnvironment({
        SMOKE_REPO: "a/b",
        SMOKE_RIG_NATS: "nats://x:1",
        SMOKE_UPSTREAM_NATS: upstream,
      });
      expect(config.upstreamUrl).toBe(want);
    }
  });
});

describe("redactUpstream", () => {
  test("puts the variable's name in place of the value, its user info, and its host", () => {
    const upstream = "nats://user:canary-pw@Canary-Host.tailnet.example:4222";
    for (const message of [
      `"http://user:canary-pw@canary-host.tailnet.example:4222" cannot be parsed as a URL.`,
      "getaddrinfo ENOTFOUND canary-host.tailnet.example",
      `connect to ${upstream} failed`,
    ]) {
      const redacted = redactUpstream(message, upstream);
      expect(redacted).toContain("SMOKE_UPSTREAM_NATS");
      expect(redacted.toLowerCase()).not.toContain("canary");
    }
  });

  test("leaves a message that does not quote the upstream as it is", () => {
    expect(redactUpstream("CONNECTION_REFUSED", "nats://envoy-nats.tailnet.example:4222")).toBe(
      "CONNECTION_REFUSED"
    );
  });
});

describe("envelopeValidation", () => {
  test("accepts a current envelope as bytes or text", () => {
    const text = JSON.stringify(currentEnvelope());
    expect(envelopeValidation(new TextEncoder().encode(text))).toEqual({
      ok: true,
      shape: "current",
    });
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
