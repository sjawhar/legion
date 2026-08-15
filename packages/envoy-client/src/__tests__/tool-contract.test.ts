import { describe, expect, test } from "bun:test";
import { envoyToolSpecs } from "../tool-contract";

describe("envoyToolSpecs", () => {
  test("defines the eight canonical Envoy tool names", () => {
    expect(envoyToolSpecs.map((spec) => spec.name)).toEqual([
      "envoy_subscribe",
      "envoy_unsubscribe",
      "envoy_list",
      "envoy_send",
      "envoy_publish",
      "envoy_role_set",
      "envoy_whoami",
      "envoy_sessions",
    ]);
  });

  test("keeps subscription operations capability-backed rather than registered by the core", () => {
    const subscribe = envoyToolSpecs.find((spec) => spec.name === "envoy_subscribe");

    expect(subscribe?.operation).toBe("subscribe");
    expect(subscribe?.requiresSubscriptionCapability).toBe(true);
  });

  test("maps every tool descriptor to its harness-neutral operation", () => {
    expect(envoyToolSpecs.map((spec) => spec.operation)).toEqual([
      "subscribe",
      "unsubscribe",
      "listInterests",
      "send",
      "publish",
      "setRole",
      "whoami",
      "listSessions",
    ]);
  });
});
