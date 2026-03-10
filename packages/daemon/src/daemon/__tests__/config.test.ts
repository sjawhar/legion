import { describe, expect, it } from "bun:test";
import os from "node:os";
import path from "node:path";
import { loadConfig } from "../config";

describe("daemon config", () => {
  it("loads defaults when env missing", () => {
    const config = loadConfig({});

    expect(config.daemonPort).toBe(13370);
    expect(config.legionId).toBeUndefined();
    expect(config.legionDir).toBeUndefined();
    expect(config.checkIntervalMs).toBe(60_000);
    expect(config.baseWorkerPort).toBe(13381);
    expect(config.stateFilePath).toBe(
      path.join(os.homedir(), ".local", "state", "legion", "daemon", "workers.json")
    );
    expect(config.logDir).toBe(
      path.join(os.homedir(), ".local", "state", "legion", "daemon", "logs")
    );
    expect(config.paths).toEqual({
      dataDir: path.join(os.homedir(), ".local", "share", "legion"),
      stateDir: path.join(os.homedir(), ".local", "state", "legion"),
      reposDir: path.join(os.homedir(), ".local", "share", "legion", "repos"),
      workspacesDir: path.join(os.homedir(), ".local", "share", "legion", "workspaces"),
      legionsFile: path.join(os.homedir(), ".local", "state", "legion", "legions.json"),
      forLegion: expect.any(Function),
      repoClonePath: expect.any(Function),
    });
  });

  it("reads values from env vars", () => {
    const config = loadConfig({
      LEGION_DAEMON_PORT: "14000",
      LEGION_ID: "team-123",
      LEGION_DIR: "/tmp/legion",
    });

    expect(config.daemonPort).toBe(14000);
    expect(config.legionId).toBe("team-123");
    expect(config.legionDir).toBe("/tmp/legion");
    expect(config.stateFilePath).toBe(
      path.join(os.homedir(), ".local", "state", "legion", "legions", "team-123", "workers.json")
    );
    expect(config.logDir).toBe(
      path.join(os.homedir(), ".local", "state", "legion", "legions", "team-123", "logs")
    );
  });

  it("falls back to daemon state path when legionId is not set", () => {
    const config = loadConfig({ LEGION_DIR: "/tmp/legion" });

    expect(config.legionDir).toBe("/tmp/legion");
    expect(config.legionId).toBeUndefined();
    expect(config.stateFilePath).toBe(
      path.join(os.homedir(), ".local", "state", "legion", "daemon", "workers.json")
    );
  });

  it("falls back when daemon port is invalid", () => {
    const config = loadConfig({ LEGION_DAEMON_PORT: "nope" });
    expect(config.daemonPort).toBe(13370);
  });

  it("reads controllerSessionId from env", () => {
    const config = loadConfig({ LEGION_CONTROLLER_SESSION_ID: "ses_abc123" });
    expect(config.controllerSessionId).toBe("ses_abc123");
  });

  it("controllerSessionId is undefined when env var missing", () => {
    const config = loadConfig({});
    expect(config.controllerSessionId).toBeUndefined();
  });

  it("throws when controllerSessionId has invalid format", () => {
    expect(() => {
      loadConfig({ LEGION_CONTROLLER_SESSION_ID: "bad_value" });
    }).toThrow("LEGION_CONTROLLER_SESSION_ID must start with 'ses_' (got: bad_value)");
  });

  describe("issueBackend", () => {
    it("defaults to linear", () => {
      const config = loadConfig({});
      expect(config.issueBackend).toBe("linear");
    });

    it("reads LEGION_ISSUE_BACKEND env var", () => {
      const config = loadConfig({ LEGION_ISSUE_BACKEND: "github" });
      expect(config.issueBackend).toBe("github");
    });

    it("throws for invalid backend", () => {
      expect(() => loadConfig({ LEGION_ISSUE_BACKEND: "jira" })).toThrow("LEGION_ISSUE_BACKEND");
    });

    it("throws for empty string backend", () => {
      expect(() => loadConfig({ LEGION_ISSUE_BACKEND: "" })).toThrow("LEGION_ISSUE_BACKEND");
    });
  });

  describe("runtime config", () => {
    it("defaults to opencode", () => {
      const config = loadConfig({ LEGION_ID: "test" });
      expect(config.runtime).toBe("opencode");
    });

    it("reads from LEGION_RUNTIME env var", () => {
      const config = loadConfig({ LEGION_ID: "test", LEGION_RUNTIME: "claude-code" });
      expect(config.runtime).toBe("claude-code");
    });

    it("rejects invalid runtime values", () => {
      expect(() => loadConfig({ LEGION_ID: "test", LEGION_RUNTIME: "invalid" })).toThrow();
    });
  });
});
