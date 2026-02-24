import { describe, expect, it } from "bun:test";
import fs from "node:fs";
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

  describe("vcs", () => {
    it("defaults to git when no env var or legionDir", () => {
      const config = loadConfig({});
      expect(config.vcs).toBe("git");
    });

    it("reads LEGION_VCS=jj from env", () => {
      const config = loadConfig({ LEGION_VCS: "jj" });
      expect(config.vcs).toBe("jj");
    });

    it("reads LEGION_VCS=git from env", () => {
      const config = loadConfig({ LEGION_VCS: "git" });
      expect(config.vcs).toBe("git");
    });

    it("throws for invalid VCS value", () => {
      expect(() => loadConfig({ LEGION_VCS: "svn" })).toThrow("LEGION_VCS");
    });

    it("auto-detects jj from .jj directory", () => {
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-vcs-"));
      try {
        fs.mkdirSync(path.join(tmpDir, ".jj"));
        const config = loadConfig({ LEGION_DIR: tmpDir });
        expect(config.vcs).toBe("jj");
      } finally {
        fs.rmSync(tmpDir, { recursive: true, force: true });
      }
    });

    it("defaults to git when legionDir has no .jj", () => {
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-vcs-"));
      try {
        const config = loadConfig({ LEGION_DIR: tmpDir });
        expect(config.vcs).toBe("git");
      } finally {
        fs.rmSync(tmpDir, { recursive: true, force: true });
      }
    });

    it("env var takes precedence over auto-detection", () => {
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-vcs-"));
      try {
        fs.mkdirSync(path.join(tmpDir, ".jj"));
        const config = loadConfig({ LEGION_DIR: tmpDir, LEGION_VCS: "git" });
        expect(config.vcs).toBe("git");
      } finally {
        fs.rmSync(tmpDir, { recursive: true, force: true });
      }
    });
  });
});
