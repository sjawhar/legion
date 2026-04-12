import { describe, expect, it } from "bun:test";
import os from "node:os";
import path from "node:path";
import { loadConfig, loadConfigFromFile, resolveDaemonConfig } from "../config";

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
    it("is undefined when no github app config is set", () => {
      const config = loadConfig({});
      expect(config.githubApps).toBeUndefined();
    });

    it("ignores legacy github app env vars because github apps are config-file-only", () => {
      const config = loadConfig({
        LEGION_GITHUB_APP_IMPL_ID: "12345",
        LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/tmp/impl.pem",
        LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "777",
      });

      expect(config.githubApps).toBeUndefined();
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
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "acme/12",
        LEGION_ISSUE_BACKEND: "github",
      });

      expect(config.extraProjects).toEqual(["acme/12"]);
    });

    it("parses multiple comma-separated entries", () => {
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "acme/12,globex/34,initech/56",
        LEGION_ISSUE_BACKEND: "github",
      });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34", "initech/56"]);
    });

    it("trims whitespace from entries", () => {
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "  acme/12 ,\tglobex/34\n",
        LEGION_ISSUE_BACKEND: "github",
      });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34"]);
    });

    it("ignores empty segments from trailing and double commas", () => {
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "acme/12,,globex/34,",
        LEGION_ISSUE_BACKEND: "github",
      });

      expect(config.extraProjects).toEqual(["acme/12", "globex/34"]);
    });

    it("deduplicates entries preserving order", () => {
      const config = loadConfig({
        LEGION_EXTRA_PROJECTS: "acme/12,globex/34,acme/12,initech/56,globex/34",
        LEGION_ISSUE_BACKEND: "github",
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

  describe("loadConfigFromFile", () => {
    it("maps YAML fields to flattened config fields", () => {
      const result = loadConfigFromFile(
        [
          "project: team-123",
          "workspace: ./workspace",
          "port: 14000",
          "backend: github",
          "runtime: claude-code",
          "controller:",
          "  session_id: ses_controller",
          "  prompt: keep going",
          "memory:",
          "  max_rss_gb: 10",
          "  rss_check_interval_seconds: 120",
          "envoy_url: http://127.0.0.1:9999",
          "feedback:",
          "  disabled: true",
          "  max_bytes: 2048",
        ].join("\n"),
        "/tmp/x"
      );

      expect(result.fields).toMatchObject({
        legionId: "team-123",
        legionDir: "/tmp/x/workspace",
        daemonPort: 14000,
        controllerSessionId: "ses_controller",
        controllerPrompt: "keep going",
        maxRssBytes: 10 * 1024 * 1024 * 1024,
        rssCheckIntervalMs: 120_000,
        envoyUrl: "http://127.0.0.1:9999",
        feedbackDisabled: true,
        feedbackMaxBytes: 2048,
        issueBackend: "github",
        runtime: "claude-code",
      });
      expect(result.warnings).toEqual([]);
    });

    it("maps GitHub App fields for each role", () => {
      const result = loadConfigFromFile(
        [
          "github_apps:",
          "  implement:",
          "    app_id: impl-app",
          "    private_key: |",
          "      -----BEGIN PRIVATE KEY-----",
          "      impl-key",
          "      -----END PRIVATE KEY-----",
          "    installations:",
          "      acme: impl-install",
          "      globex: impl-install-2",
          "  review:",
          "    app_id: review-app",
          '    private_key: "review-inline-key"',
          "    installations:",
          "      acme: review-install",
        ].join("\n"),
        "/tmp/legion"
      );

      expect(result.fields).toMatchObject({
        githubApps: {
          implement: {
            appId: "impl-app",
            privateKey: "-----BEGIN PRIVATE KEY-----\nimpl-key\n-----END PRIVATE KEY-----\n",
            installations: {
              acme: "impl-install",
              globex: "impl-install-2",
            },
          },
          review: {
            appId: "review-app",
            privateKey: "review-inline-key",
            installations: {
              acme: "review-install",
            },
          },
        },
      });
    });

    it("accepts arbitrary installation owner keys without unknown-key warnings", () => {
      const result = loadConfigFromFile(
        [
          "github_apps:",
          "  implement:",
          "    app_id: impl-app",
          '    private_key: "inline-key"',
          "    installations:",
          "      acme-inc: install-1",
          "      globex-labs: install-2",
        ].join("\n"),
        "/tmp/x"
      );

      expect(result.warnings).toEqual([]);
    });

    it("normalizes relative paths against configDir and leaves absolute paths unchanged", () => {
      const relativeWorkspace = loadConfigFromFile("workspace: ./workspace\n", "/tmp/x");
      const absoluteWorkspace = loadConfigFromFile("workspace: /srv/legion\n", "/tmp/x");

      expect(relativeWorkspace.fields.legionDir).toBe("/tmp/x/workspace");
      expect(absoluteWorkspace.fields.legionDir).toBe("/srv/legion");
    });

    it("maps extra_projects arrays", () => {
      const result = loadConfigFromFile(
        ["backend: github", "extra_projects:", "  - acme/12", "  - globex/34"].join("\n"),
        "/tmp/x"
      );

      expect(result.fields.extraProjects).toEqual(["acme/12", "globex/34"]);
    });

    it("throws with parse context for invalid YAML syntax", () => {
      expect(() => loadConfigFromFile("controller: [unterminated\n", "/tmp/x")).toThrow(
        /YAML|Parse/
      );
    });

    it("throws for invalid backend values", () => {
      expect(() => loadConfigFromFile("backend: jira\n", "/tmp/x")).toThrow(/backend/i);
    });

    it("throws for invalid runtime values", () => {
      expect(() => loadConfigFromFile("runtime: invalid\n", "/tmp/x")).toThrow(/runtime/i);
    });

    it("throws for invalid controller.session_id values", () => {
      expect(() => loadConfigFromFile("controller:\n  session_id: bad_value\n", "/tmp/x")).toThrow(
        /session_id.*ses_/
      );
    });

    it("throws for prompts longer than 10000 chars", () => {
      const prompt = "a".repeat(10001);
      expect(() => loadConfigFromFile(`controller:\n  prompt: ${prompt}\n`, "/tmp/x")).toThrow(
        /10000/
      );
    });

    it("throws for prompts with control characters", () => {
      expect(() =>
        loadConfigFromFile('controller:\n  prompt: "bad\u0007prompt"\n', "/tmp/x")
      ).toThrow(/control characters/i);
    });

    it("throws when a GitHub App role is partial and lists missing fields", () => {
      expect(() =>
        loadConfigFromFile(
          ["github_apps:", "  implement:", "    app_id: impl-app"].join("\n"),
          "/tmp/x"
        )
      ).toThrow(/github_apps\.implement.*private_key.*installations/i);
    });

    it("throws when github app installations values are not strings", () => {
      expect(() =>
        loadConfigFromFile(
          [
            "github_apps:",
            "  implement:",
            "    app_id: impl-app",
            '    private_key: "inline-key"',
            "    installations:",
            "      acme:",
            "        nested: nope",
          ].join("\n"),
          "/tmp/x"
        )
      ).toThrow(/github_apps\.implement\.installations\.acme must be a string/i);
    });

    it("throws when extra_projects is set for linear backend", () => {
      expect(() =>
        loadConfigFromFile(
          ["backend: linear", "extra_projects:", "  - acme/12"].join("\n"),
          "/tmp/x"
        )
      ).toThrow(/extra_projects.*github/i);
    });

    it("throws when extra_projects entries do not match owner/number", () => {
      expect(() =>
        loadConfigFromFile(
          ["backend: github", "extra_projects:", "  - bad-entry"].join("\n"),
          "/tmp/x"
        )
      ).toThrow(/owner\/number/i);
    });

    it("warns on root unknown keys", () => {
      const result = loadConfigFromFile("project: team-123\nextra_key: value\n", "/tmp/x");

      expect(result.warnings).toContainEqual(expect.stringMatching(/extra_key/));
    });

    it("warns on nested unknown keys with full dotted paths", () => {
      const result = loadConfigFromFile(
        [
          "github_apps:",
          "  implement:",
          "    app_id: impl-app",
          '    private_key: "inline-key"',
          "    installations:",
          "      acme: impl-install",
          "    extra_field: value",
        ].join("\n"),
        "/tmp/x"
      );

      expect(result.warnings).toContainEqual(
        expect.stringMatching(/github_apps\.implement\.extra_field/)
      );
    });

    it("keeps known sibling values when warnings are emitted", () => {
      const result = loadConfigFromFile(
        ["project: team-123", "feedback:", "  disabled: true", "  extra_field: value"].join("\n"),
        "/tmp/x"
      );

      expect(result.fields).toMatchObject({
        legionId: "team-123",
        feedbackDisabled: true,
      });
      expect(result.warnings).toContainEqual(expect.stringMatching(/feedback\.extra_field/));
    });
  });

  describe("resolveDaemonConfig", () => {
    it("prefers config file values over env vars for the same field", () => {
      const result = resolveDaemonConfig({
        env: { LEGION_RUNTIME: "opencode" },
        configFile: { fields: { runtime: "claude-code" }, warnings: [] },
      });

      expect(result.config.runtime).toBe("claude-code");
    });

    it("prefers CLI overrides over config file values", () => {
      const result = resolveDaemonConfig({
        configFile: { fields: { daemonPort: 14000 }, warnings: [] },
        cliOverrides: { daemonPort: 15000 },
      });

      expect(result.config.daemonPort).toBe(15000);
    });

    it("fills defaults for absent fields", () => {
      const result = resolveDaemonConfig({});

      expect(result.config).toMatchObject({
        daemonPort: 13370,
        issueBackend: "linear",
        runtime: "opencode",
        envoyUrl: "http://127.0.0.1:9020",
        feedbackDisabled: false,
        feedbackMaxBytes: 50 * 1024 * 1024,
        maxRssBytes: 20 * 1024 * 1024 * 1024,
        rssCheckIntervalMs: 60_000,
      });
    });

    it("sets daemonPortExplicit true for CLI or config values and false for defaults", () => {
      expect(
        resolveDaemonConfig({ cliOverrides: { daemonPort: 15000 } }).config.daemonPortExplicit
      ).toBe(true);
      expect(
        resolveDaemonConfig({ configFile: { fields: { daemonPort: 14000 }, warnings: [] } }).config
          .daemonPortExplicit
      ).toBe(true);
      expect(resolveDaemonConfig({}).config.daemonPortExplicit).toBe(false);
      expect(
        resolveDaemonConfig({ env: { LEGION_DAEMON_PORT: "16000" } }).config.daemonPortExplicit
      ).toBe(false);
    });

    it.each([
      ["ENVOY_URL", "envoyUrl", "http://127.0.0.1:9999"],
      ["LEGION_FEEDBACK_DISABLED", "feedbackDisabled", "true"],
      ["LEGION_FEEDBACK_MAX_BYTES", "feedbackMaxBytes", "1234"],
      ["LEGION_ISSUE_BACKEND", "issueBackend", "github"],
      ["LEGION_RUNTIME", "runtime", "claude-code"],
      ["LEGION_DAEMON_PORT", "daemonPort", "14444"],
    ])("emits deprecation warning when %s is the effective value", (envVar, fieldName, value) => {
      const result = resolveDaemonConfig({
        env: { [envVar]: value },
      });

      expect(result.warnings).toContainEqual(expect.stringMatching(new RegExp(envVar)));
      expect(result.warnings).toContainEqual(expect.stringMatching(/deprecated/i));
      expect(result.config).toHaveProperty(fieldName);
    });

    it.each([
      ["ENVOY_URL", "envoyUrl", "http://127.0.0.1:9999", "http://127.0.0.1:7777"],
      ["LEGION_FEEDBACK_DISABLED", "feedbackDisabled", "true", false],
      ["LEGION_FEEDBACK_MAX_BYTES", "feedbackMaxBytes", "1234", 4321],
      ["LEGION_ISSUE_BACKEND", "issueBackend", "github", "linear"],
      ["LEGION_RUNTIME", "runtime", "claude-code", "opencode"],
      ["LEGION_DAEMON_PORT", "daemonPort", "14444", 15555],
    ])("does not emit deprecation warning when config file overrides %s", (envVar, fieldName, envValue, configValue) => {
      const result = resolveDaemonConfig({
        env: { [envVar]: envValue },
        configFile: { fields: { [fieldName]: configValue }, warnings: [] },
      });

      expect(result.warnings.some((warning) => warning.includes(envVar))).toBe(false);
      expect(result.config).toHaveProperty(fieldName, configValue);
    });

    it("ignores github app env vars without emitting deprecation warnings", () => {
      const result = resolveDaemonConfig({
        env: {
          LEGION_GITHUB_APP_IMPL_ID: "111",
          LEGION_GITHUB_APP_IMPL_PRIVATE_KEY_PATH: "/tmp/impl.pem",
          LEGION_GITHUB_APP_IMPL_INSTALLATION_ID: "222",
        },
      });

      expect(result.config.githubApps).toBeUndefined();
      expect(result.warnings.some((warning) => warning.includes("LEGION_GITHUB_APP_"))).toBe(false);
    });
  });

  describe("env compatibility for new fields", () => {
    it("reads ENVOY_URL and defaults when unset", () => {
      expect(loadConfig({ ENVOY_URL: "http://127.0.0.1:9999" }).envoyUrl).toBe(
        "http://127.0.0.1:9999"
      );
      expect(loadConfig({}).envoyUrl).toBe("http://127.0.0.1:9020");
    });

    it("reads LEGION_FEEDBACK_DISABLED and defaults when unset", () => {
      expect(loadConfig({ LEGION_FEEDBACK_DISABLED: "true" }).feedbackDisabled).toBe(true);
      expect(loadConfig({}).feedbackDisabled).toBe(false);
    });

    it("reads LEGION_FEEDBACK_MAX_BYTES and defaults when unset", () => {
      expect(loadConfig({ LEGION_FEEDBACK_MAX_BYTES: "1234" }).feedbackMaxBytes).toBe(1234);
      expect(loadConfig({}).feedbackMaxBytes).toBe(50 * 1024 * 1024);
    });
  });
});
