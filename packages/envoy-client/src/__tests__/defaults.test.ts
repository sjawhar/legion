import { describe, expect, test } from "bun:test";
import { envoyDefaultsFromEnvironment } from "../defaults";

describe("envoyDefaultsFromEnvironment", () => {
  test("parses Envoy endpoint, NATS URLs, and a bounded heartbeat from environment values", () => {
    const defaults = envoyDefaultsFromEnvironment({
      ENVOY_URL: "http://listener:9020/",
      ENVOY_NATS_URL: "nats://one:4222, nats://two:4222",
      ENVOY_HEARTBEAT_MS: "10",
    });

    expect(defaults).toEqual({
      envoyUrl: "http://listener:9020",
      natsUrls: ["nats://one:4222", "nats://two:4222"],
      heartbeatMs: 25,
    });
  });

  test("uses protocol defaults when environment values are absent or invalid", () => {
    expect(envoyDefaultsFromEnvironment({ ENVOY_HEARTBEAT_MS: "not-a-number" })).toEqual({
      envoyUrl: "http://127.0.0.1:9020",
      natsUrls: ["nats://127.0.0.1:4222"],
      heartbeatMs: 120_000,
    });
  });
});
