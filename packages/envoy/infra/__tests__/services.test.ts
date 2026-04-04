import { describe, expect, test } from "bun:test";
import { computeNatsUrls } from "../services";

describe("computeNatsUrls", () => {
  const machines = [
    { name: "sami-agents-mx", tailscaleIp: "100.64.0.1", nats: true },
    { name: "sami", tailscaleIp: "100.64.0.2", nats: true },
    { name: "sami-claude", tailscaleIp: "100.64.0.3", nats: true },
    { name: "ghost-wispr", tailscaleIp: "100.64.0.4", nats: false },
  ];

  test("machine with local NATS peer gets 127.0.0.1 first, then remote peers", () => {
    const urls = computeNatsUrls("sami-agents-mx", true, machines);
    expect(urls).toBe("nats://127.0.0.1:4222,nats://100.64.0.2:4222,nats://100.64.0.3:4222");
  });

  test("machine without local NATS peer gets all remote peers", () => {
    const urls = computeNatsUrls("ghost-wispr", false, machines);
    expect(urls).toBe("nats://100.64.0.1:4222,nats://100.64.0.2:4222,nats://100.64.0.3:4222");
  });
});
