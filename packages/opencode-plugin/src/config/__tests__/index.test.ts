import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { loadPluginConfig } from "../index";

function createTempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "opencode-config-test-"));
}

function removeTempDir(dir: string): void {
  if (fs.existsSync(dir)) {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function writeConfigFile(filePath: string, data: unknown): void {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, JSON.stringify(data, null, 2), "utf-8");
}

describe("PluginConfig", () => {
  let tempDir: string;
  let tempHomeDir: string;

  beforeEach(() => {
    tempDir = createTempDir();
    tempHomeDir = createTempDir();
  });

  afterEach(() => {
    removeTempDir(tempDir);
    removeTempDir(tempHomeDir);
  });

  it("applies defaults when no config files exist", async () => {
    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.concurrency?.perModel).toBe(5);
    expect(config.concurrency?.global).toBe(15);
    expect(config.inactivityAlertMs).toBe(600000);
    expect(config.retry?.maxRetries).toBe(1);
    expect(config.retry?.delayMs).toBe(2000);
    expect(config.retry?.fallbackModel).toBeUndefined();
    expect(config.taskRetentionMs).toBe(3600000);
  });

  it("parses user config file correctly", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      concurrency: {
        perModel: 10,
        global: 20,
      },
      inactivityAlertMs: 300000,
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.concurrency?.perModel).toBe(10);
    expect(config.concurrency?.global).toBe(20);
    expect(config.inactivityAlertMs).toBe(300000);
    expect(config.retry?.maxRetries).toBe(1);
    expect(config.taskRetentionMs).toBe(3600000);
  });

  it("parses repo config file correctly", async () => {
    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      retry: {
        maxRetries: 3,
        delayMs: 5000,
        fallbackModel: "anthropic/claude-sonnet-4-20250514",
      },
      taskRetentionMs: 7200000,
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.retry?.maxRetries).toBe(3);
    expect(config.retry?.delayMs).toBe(5000);
    expect(config.retry?.fallbackModel).toBe("anthropic/claude-sonnet-4-20250514");
    expect(config.taskRetentionMs).toBe(7200000);
    expect(config.concurrency?.perModel).toBe(5);
  });

  it("merges user and repo configs with repo taking precedence", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      concurrency: {
        perModel: 10,
        global: 20,
      },
      inactivityAlertMs: 300000,
      retry: {
        maxRetries: 2,
      },
    });

    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      concurrency: {
        perModel: 8,
      },
      taskRetentionMs: 7200000,
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.concurrency?.perModel).toBe(8);
    expect(config.concurrency?.global).toBe(20);
    expect(config.inactivityAlertMs).toBe(300000);
    expect(config.retry?.maxRetries).toBe(2);
    expect(config.retry?.delayMs).toBe(2000);
    expect(config.taskRetentionMs).toBe(7200000);
  });

  it("handles partial config with some fields set", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      concurrency: {
        perModel: 12,
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.concurrency?.perModel).toBe(12);
    expect(config.concurrency?.global).toBe(15);
    expect(config.inactivityAlertMs).toBe(600000);
    expect(config.retry?.maxRetries).toBe(1);
    expect(config.taskRetentionMs).toBe(3600000);
  });

  it("handles partial retry config", async () => {
    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      retry: {
        maxRetries: 5,
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.retry?.maxRetries).toBe(5);
    expect(config.retry?.delayMs).toBe(2000);
    expect(config.retry?.fallbackModel).toBeUndefined();
  });

  it("preserves other config fields while applying defaults", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      agents: {
        orchestrator: {
          model: "anthropic/claude-opus-4-6",
        },
      },
      permission: "allow",
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.agents?.orchestrator?.model).toBe("anthropic/claude-opus-4-6");
    expect(config.permission).toBe("allow");
    expect(config.concurrency?.perModel).toBe(5);
    expect(config.inactivityAlertMs).toBe(600000);
  });

  it("handles invalid config files gracefully", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    fs.mkdirSync(path.dirname(userConfigPath), { recursive: true });
    fs.writeFileSync(userConfigPath, "invalid json {", "utf-8");

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.concurrency?.perModel).toBe(5);
    expect(config.inactivityAlertMs).toBe(600000);
  });

  it("handles missing optional fields in config", async () => {
    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      agents: {
        executor: {
          model: "anthropic/claude-sonnet-4-20250514",
        },
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.agents?.executor?.model).toBe("anthropic/claude-sonnet-4-20250514");
    expect(config.concurrency?.perModel).toBe(5);
    expect(config.concurrency?.global).toBe(15);
    expect(config.inactivityAlertMs).toBe(600000);
    expect(config.retry?.maxRetries).toBe(1);
    expect(config.taskRetentionMs).toBe(3600000);
  });

  it("merges outputCompression excludeTools with union semantics", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      outputCompression: {
        enabled: true,
        excludeTools: ["edit", "write"],
      },
    });

    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      outputCompression: {
        excludeTools: ["bash", "write"],
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.outputCompression?.excludeTools).toEqual(["edit", "write", "bash"]);
  });

  it("preserves base excludeTools when override has none", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      outputCompression: {
        enabled: true,
        excludeTools: ["edit"],
      },
    });

    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      outputCompression: {
        enabled: false,
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.outputCompression?.excludeTools).toEqual(["edit"]);
  });

  it("uses override excludeTools when base has none", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      outputCompression: {
        enabled: true,
      },
    });

    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      outputCompression: {
        excludeTools: ["bash"],
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.outputCompression?.excludeTools).toEqual(["bash"]);
  });

  it("returns undefined excludeTools when neither base nor override has it", async () => {
    const userConfigPath = path.join(tempHomeDir, ".config", "opencode", "opencode-legion.json");
    writeConfigFile(userConfigPath, {
      outputCompression: {
        enabled: true,
      },
    });

    const repoConfigPath = path.join(tempDir, ".opencode", "opencode-legion.json");
    writeConfigFile(repoConfigPath, {
      outputCompression: {
        thresholdBytes: 1000,
      },
    });

    const config = await loadPluginConfig(tempDir, { homeDir: tempHomeDir });

    expect(config.outputCompression?.excludeTools).toBeUndefined();
  });
});
