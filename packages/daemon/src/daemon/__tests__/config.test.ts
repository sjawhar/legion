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

  describe("githubApps", () => {
    it("is undefined when no github app env vars are set", () => {
      const config = loadConfig({});
      expect(config.githubApps).toBeUndefined();
    });

    it("loads a single configured role when all role vars are present", () => {
      const config = loadConfig({
        LEGION_GITHUB_APP_IMPL_ID: "12345",
        LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/tmp/impl.pem",
        LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "777",
      });

      expect(config.githubApps).toEqual({
        impl: {
          appId: "12345",
          privateKeyPath: "/tmp/impl.pem",
          installationId: "777",
        },
      });
    });

    it("loads all configured roles simultaneously", () => {
      const config = loadConfig({
        LEGION_GITHUB_APP_IMPL_ID: "111",
        LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/tmp/impl.pem",
        LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "222",
        LEGION_GITHUB_APP_REVIEW_ID: "333",
        LEGION_GITHUB_APP_REVIEW_PRIVATE_KEY_PATH: "/tmp/review.pem",
        LEGION_GITHUB_APP_REVIEW_INSTALLATION_ID: "444",
      });

      expect(config.githubApps).toEqual({
        impl: {
          appId: "111",
          privateKeyPath: "/tmp/impl.pem",
          installationId: "222",
        },
        review: {
          appId: "333",
          privateKeyPath: "/tmp/review.pem",
          installationId: "444",
        },
      });
    });

    it("does not load partially configured role", () => {
      const config = loadConfig({
        LEGION_GITHUB_APP_IMPL_ID: "12345",
      });

      expect(config.githubApps).toBeUndefined();
    });

    it("loads only fully configured roles from mixed env", () => {
      const config = loadConfig({
        LEGION_GITHUB_APP_IMPL_ID: "111",
        LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/tmp/impl.pem",
        LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "222",
        LEGION_GITHUB_APP_REVIEW_ID: "333",
        // review missing PRIVATE_KEY_PATH and INSTALLATION_ID
      });

      expect(config.githubApps).toEqual({
        impl: {
          appId: "111",
          privateKeyPath: "/tmp/impl.pem",
          installationId: "222",
        },
      });
    });
  });

  describe("LEGION_EXTRA_PROJECTS parsing", () => {
    it("returns undefined when env var not set", () => {
      const config = loadConfig({});

      expect(config.extraProjects).toBeUndefined();
    });

    it("returns undefined when env var is empty string", () => {
      const config = loadConfig({ LEGION_EXTRA_PROJECTS: "" });

      expect(config.extraProjects).toBeUndefined();
    });

    it("parses single valid entry", () => {
      const config = loadConfig({ LEGION_EXTRA_PROJECTS: "acme/12" });

      expect(config.extraProjects).toEqual(["acme/12"]);
    });

    it("parses multiple comma-separated entries", () => {
      const config = loadConfig({ LEGION_EXTRA_PROJECTS: "acme/12,globex/34,initech/56" });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34", "initech/56"]);
    });

    it("trims whitespace from entries", () => {
      const config = loadConfig({ LEGION_EXTRA_PROJECTS: "  acme/12 ,\tglobex/34\n" });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34"]);
    });

    it("ignores empty segments from trailing and double commas", () => {
      const config = loadConfig({ LEGION_EXTRA_PROJECTS: "acme/12,,globex/34," });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34"]);
    });

    it("deduplicates entries preserving order", () => {
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "acme/12,globex/34,acme/12,initech/56,globex/34",
      });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34", "initech/56"]);
    });

    it("throws on malformed entry with too many slashes", () => {
      expect(() => loadConfig({ LEGION_EXTRA_PROJECTS: "acme/team/12" })).toThrow(
        "LEGION_EXTRA_PROJECTS"
      );
    });

    it("throws on malformed entry with non-numeric project number", () => {
      expect(() => loadConfig({ LEGION_EXTRA_PROJECTS: "acme/not-a-number" })).toThrow(
        "LEGION_EXTRA_PROJECTS"
      );
    });

    it("throws on malformed entry with missing owner", () => {
      expect(() => loadConfig({ LEGION_EXTRA_PROJECTS: "/12" })).toThrow("LEGION_EXTRA_PROJECTS");
    });

    it("throws on malformed entry with missing number", () => {
      expect(() => loadConfig({ LEGION_EXTRA_PROJECTS: "acme/" })).toThrow("LEGION_EXTRA_PROJECTS");
    });

    it("throws on first invalid entry even when others are valid", () => {
      expect(() => loadConfig({ LEGION_EXTRA_PROJECTS: "acme/12,bad,globex/34" })).toThrow(
        "LEGION_EXTRA_PROJECTS"
      );
    });
  });

  describe("CLI args precedence", () => {
    it("prefers CLI args over env vars", () => {
      const config = loadConfig(
        {
          LEGION_ID: "env-primary",
          LEGION_EXTRA_PROJECTS: "env-owner/22",
          LEGION_DAEMON_PORT: "14000",
          LEGION_CONTROLLER_SESSION_ID: "ses_env",
          LEGION_ISSUE_BACKEND: "linear",
          LEGION_RUNTIME: "opencode",
          ENVOY_URL: "http://env.example:9020",
        },
        {
          projects: "cli-owner/11,cli-owner/22",
          port: "15000",
          controllerSession: "ses_cli",
          backend: "github",
          runtime: "claude-code",
          envoyUrl: "http://cli.example:9123",
        }
      );

      expect(config.legionId).toBe("cli-owner/11");
      expect(config.extraProjects).toEqual(["cli-owner/22"]);
      expect(config.daemonPort).toBe(15000);
      expect(config.controllerSessionId).toBe("ses_cli");
      expect(config.issueBackend).toBe("github");
      expect(config.runtime).toBe("claude-code");
      expect(config.envoyUrl).toBe("http://cli.example:9123");
    });

    it("falls back to env vars when CLI args are omitted", () => {
      const config = loadConfig(
        {
          LEGION_ID: "env-primary",
          LEGION_EXTRA_PROJECTS: "env-owner/22,env-owner/33",
          LEGION_DAEMON_PORT: "14000",
          LEGION_CONTROLLER_SESSION_ID: "ses_env",
          LEGION_ISSUE_BACKEND: "github",
          LEGION_RUNTIME: "claude-code",
          ENVOY_URL: "http://env.example:9020",
        },
        {}
      );

      expect(config.legionId).toBe("env-primary");
      expect(config.extraProjects).toEqual(["env-owner/22", "env-owner/33"]);
      expect(config.daemonPort).toBe(14000);
      expect(config.controllerSessionId).toBe("ses_env");
      expect(config.issueBackend).toBe("github");
      expect(config.runtime).toBe("claude-code");
      expect(config.envoyUrl).toBe("http://env.example:9020");
    });

    it("defaults envoyUrl when neither CLI nor env provides one", () => {
      const config = loadConfig({}, {});

      expect(config.envoyUrl).toBe("http://127.0.0.1:9020");
    });

    it("rejects malformed --projects entries", () => {
      expect(() => loadConfig({}, { projects: "cli-owner/not-a-number" })).toThrow("--projects");
    });

    it("parses --projects with first as legionId and rest as extraProjects", () => {
      const config = loadConfig({}, { projects: "acme/1,beta/2,gamma/3" });

      expect(config.legionId).toBe("acme/1");
      expect(config.extraProjects).toEqual(["beta/2", "gamma/3"]);
    });

    it("deduplicates --projects entries", () => {
      const config = loadConfig({}, { projects: "acme/1,beta/2,acme/1" });

      expect(config.legionId).toBe("acme/1");
      expect(config.extraProjects).toEqual(["beta/2"]);
    });

    it("CLI --projects overrides both LEGION_ID and LEGION_EXTRA_PROJECTS", () => {
      const config = loadConfig(
        {
          LEGION_ID: "env-primary",
          LEGION_EXTRA_PROJECTS: "env-owner/22,env-owner/33",
        },
        { projects: "cli-owner/11,cli-owner/22" }
      );

      expect(config.legionId).toBe("cli-owner/11");
      expect(config.extraProjects).toEqual(["cli-owner/22"]);
    });
  });

  describe("RSS monitoring config", () => {
    it("defaults maxRssBytes to 20GB", () => {
      const config = loadConfig({});
      expect(config.maxRssBytes).toBe(20 * 1024 * 1024 * 1024);
    });

    it("defaults rssCheckIntervalMs to 60s", () => {
      const config = loadConfig({});
      expect(config.rssCheckIntervalMs).toBe(60_000);
    });

    it("parses OPENCODE_MAX_RSS_GB", () => {
      const config = loadConfig({ OPENCODE_MAX_RSS_GB: "10" });
      expect(config.maxRssBytes).toBe(10 * 1024 * 1024 * 1024);
    });

    it("disables RSS check when OPENCODE_MAX_RSS_GB=0", () => {
      const config = loadConfig({ OPENCODE_MAX_RSS_GB: "0" });
      expect(config.maxRssBytes).toBe(0);
    });

    it("parses OPENCODE_RSS_CHECK_INTERVAL", () => {
      const config = loadConfig({ OPENCODE_RSS_CHECK_INTERVAL: "120" });
      expect(config.rssCheckIntervalMs).toBe(120_000);
    });

    it("falls back to default for invalid OPENCODE_MAX_RSS_GB", () => {
      const config = loadConfig({ OPENCODE_MAX_RSS_GB: "not-a-number" });
      expect(config.maxRssBytes).toBe(20 * 1024 * 1024 * 1024);
    });

    it("falls back to default for invalid OPENCODE_RSS_CHECK_INTERVAL", () => {
      const config = loadConfig({ OPENCODE_RSS_CHECK_INTERVAL: "abc" });
      expect(config.rssCheckIntervalMs).toBe(60_000);
    });
  });
});
