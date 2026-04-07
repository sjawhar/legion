import { describe, expect, test } from "bun:test";
import { computeNatsUrls } from "../services";

describe("computeNatsUrls", () => {
  const machines = [
    { name: "sami-agents-mx", nats: true },
    { name: "sami", nats: true },
    { name: "sami-claude", nats: true },
    { name: "ghost-wispr", nats: false },
  ];

  test("machine with local NATS peer gets 127.0.0.1 first, then remote peers", () => {
    const urls = computeNatsUrls("sami-agents-mx", true, machines);
    expect(urls).toBe("nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222");
  });

  test("machine without local NATS peer gets all remote peers", () => {
    const urls = computeNatsUrls("ghost-wispr", false, machines);
    expect(urls).toBe("nats://sami-agents-mx:4222,nats://sami:4222,nats://sami-claude:4222");
  });
});
