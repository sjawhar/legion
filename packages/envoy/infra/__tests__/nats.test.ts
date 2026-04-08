import { describe, expect, test } from "bun:test";
import { computeNatsRoutes, renderNatsConf } from "../nats";

describe("computeNatsRoutes", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("returns routes to all OTHER peers", () => {
    const routes = computeNatsRoutes("sami-agents-mx", machines);
    expect(routes).toEqual(["nats://sami:6222", "nats://sami-claude:6222"]);
  });

  test("excludes non-NATS machines", () => {
    const routes = computeNatsRoutes("sami", machines);
    expect(routes).toEqual(["nats://sami-agents-mx:6222", "nats://sami-claude:6222"]);
    expect(routes.some((r) => r.includes("ghost-wispr"))).toBe(false);
  });

  test("returns all peer routes for a non-peer machine", () => {
    const routes = computeNatsRoutes("ghost-wispr", machines);
    expect(routes).toEqual([
      "nats://sami-agents-mx:6222",
      "nats://sami:6222",
      "nats://sami-claude:6222",
    ]);
  });
});

describe("renderNatsConf", () => {
  test("renders valid nats.conf with client_advertise and cluster advertise", () => {
    const conf = renderNatsConf(
      "sami-agents-mx",
      ["nats://sami:6222", "nats://sami-claude:6222"],
      "sami-agents-mx"
    );

    expect(conf).toContain("server_name=sami-agents-mx");
    expect(conf).toContain("listen=0.0.0.0:4222");
    expect(conf).toContain("client_advertise=sami-agents-mx:4222");
    expect(conf).toContain("store_dir=/data");
    expect(conf).toContain("name: envoy");
    expect(conf).toContain("listen: 0.0.0.0:6222");
    expect(conf).toContain("advertise: sami-agents-mx:6222");
    expect(conf).toContain("nats://sami:6222");
    expect(conf).toContain("nats://sami-claude:6222");
    expect(conf).not.toContain("sami-agents-mx:6222\n    nats://");
  });

  test("handles single-route cluster", () => {
    const conf = renderNatsConf("node-a", ["nats://node-b:6222"], "node-a");
    expect(conf).toContain("server_name=node-a");
    expect(conf).toContain("client_advertise=node-a:4222");
    expect(conf).toContain("advertise: node-a:6222");
    expect(conf).toContain("nats://node-b:6222");
  });
});
