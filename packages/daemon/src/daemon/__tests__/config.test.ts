import { describe, expect, it } from "bun:test";
import os from "node:os";
import path from "node:path";
import { loadConfig } from "../config";

describe("daemon config", () => {
  it("loads defaults when env missing", () => {
    const config = loadConfig({});

    expect(config.daemonPort).toBe(13370);
    expect(config.teamId).toBeUndefined();
    expect(config.legionDir).toBeUndefined();
    expect(config.checkIntervalMs).toBe(60_000);
    expect(config.baseWorkerPort).toBe(13381);
    expect(config.stateFilePath).toBe(path.join(os.homedir(), ".legion", "daemon", "workers.json"));
  });

  it("reads values from env vars", () => {
    const config = loadConfig({
      LEGION_DAEMON_PORT: "14000",
      LEGION_TEAM_ID: "team-123",
      LEGION_DIR: "/tmp/legion",
    });

    expect(config.daemonPort).toBe(14000);
    expect(config.teamId).toBe("team-123");
    expect(config.legionDir).toBe("/tmp/legion");
    expect(config.stateFilePath).toBe(
      path.join("/tmp/legion", ".legion", "daemon", "workers.json")
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
      const config = loadConfig({ LEGION_TEAM_ID: "test" });
      expect(config.runtime).toBe("opencode");
    });

    it("reads from LEGION_RUNTIME env var", () => {
      const config = loadConfig({ LEGION_TEAM_ID: "test", LEGION_RUNTIME: "claude-code" });
      expect(config.runtime).toBe("claude-code");
    });

    it("rejects invalid runtime values", () => {
      expect(() => loadConfig({ LEGION_TEAM_ID: "test", LEGION_RUNTIME: "invalid" })).toThrow();
    });
  });
});
