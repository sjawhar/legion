import { describe, expect, it } from "bun:test";
import { loadPluginConfig } from "../index";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

describe("spawnLimits config", () => {
  it("applies default spawnLimits when not configured", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
    try {
      const config = await loadPluginConfig(dir);
      expect(config.spawnLimits?.maxDepth).toBe(5);
      expect(config.spawnLimits?.maxDescendants).toBe(20);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("respects custom spawnLimits from config file", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
    try {
      fs.mkdirSync(path.join(dir, ".opencode"), { recursive: true });
      fs.writeFileSync(
        path.join(dir, ".opencode", "opencode-legion.json"),
        JSON.stringify({ spawnLimits: { maxDepth: 3, maxDescendants: 10 } })
      );
      const config = await loadPluginConfig(dir);
      expect(config.spawnLimits?.maxDepth).toBe(3);
      expect(config.spawnLimits?.maxDescendants).toBe(10);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("merges partial spawnLimits override (only maxDepth)", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "config-test-"));
    try {
      fs.mkdirSync(path.join(dir, ".opencode"), { recursive: true });
      fs.writeFileSync(
        path.join(dir, ".opencode", "opencode-legion.json"),
        JSON.stringify({ spawnLimits: { maxDepth: 2 } })
      );
      const config = await loadPluginConfig(dir);
      expect(config.spawnLimits?.maxDepth).toBe(2);
      expect(config.spawnLimits?.maxDescendants).toBe(20); // default preserved
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
