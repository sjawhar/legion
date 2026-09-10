import { describe, expect, it } from "bun:test";
import path from "node:path";
import { loadConfig, loadConfigFromFile, resolveDaemonConfig } from "../config";

const requiredEnv = {
  LEGION_ID: "Acme/42",
  ENVOY_NATS_URL: "nats://one:4222, nats://two:4222",
  LEGION_REPOS: "acme/widgets",
  DISPATCH_PROJECT: "ACME",
};

describe("daemon config", () => {
  it("derives the typed lifecycle config from environment", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_DAEMON_PORT: "14000",
        LEGION_BOARD_PROJECT_IDS: "PVT_alpha,PVT_beta",
        LEGION_APP_LOGINS: "legion-implement[bot],legion-review[bot]",
        LEGION_MAX_FIX_ATTEMPTS: "5",
        LEGION_ADMISSION_CAP: "7",
        LEGION_MAX_RECURSION_DEPTH: "11",
        LEGION_LINGER_HOURS: "48",
        LEGION_WORKER_CAP: "9",
        LEGION_WORKER_STOP_TIMEOUT_SECONDS: "15",
        LEGION_TREE_STOP_TIMEOUT_SECONDS: "90",
        LEGION_OMP_INVOCATION: "custom-omp-from-env",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });

    expect(config).toMatchObject({
      project: "acme42",
      legionId: "Acme/42",
      port: 14000,
      envoyUrl: "http://127.0.0.1:9020",
      natsUrls: ["nats://one:4222", "nats://two:4222"],
      dispatchProject: "ACME",
      boardProjectIds: ["PVT_alpha", "PVT_beta"],
      repos: ["acme/widgets"],
      appLogins: ["legion-implement[bot]", "legion-review[bot]"],
      maxFixAttempts: 5,
      admissionCap: 7,
      maxRecursionDepth: 11,
      lingerHours: 48,
      workerCap: 9,
      workerStopTimeoutSeconds: 15,
      treeStopTimeoutSeconds: 90,
      resyncIntervalMs: 600_000,
      gates: { design: "root-issues", merge: "human" },
      ompInvocation: "custom-omp-from-env",
    });
    expect(config.stateDir).toEndWith(path.join(".legion", "acme42"));
  });

  it("rejects DISPATCH_MCP_URL from the environment as a legacy key", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_MCP_URL: "http://127.0.0.1:18766/mcp",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow("DISPATCH_MCP_URL was replaced by DISPATCH_URL");
  });

  it("throws when dispatch_url is set and DISPATCH_TOKEN is not", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_URL: "http://127.0.0.1:18766",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(
      "dispatch_url is set but DISPATCH_TOKEN is not; the dispatch tools would not register"
    );
  });

  it("treats a whitespace-only DISPATCH_TOKEN as absent, same as fully unset", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_URL: "http://127.0.0.1:18766",
          DISPATCH_TOKEN: "   ",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(
      "dispatch_url is set but DISPATCH_TOKEN is not; the dispatch tools would not register"
    );
  });

  it("leaves dispatchToken undefined and unexported when DISPATCH_TOKEN is set without dispatch_url", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_x",
        DISPATCH_TOKEN: "some-token",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchToken).toBeUndefined();
  });

  it("defaults worker and tree stop timeouts to 10 and 60 seconds", () => {
    const { config } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        boardProjectIds: ["PVT_x"],
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.workerStopTimeoutSeconds).toBe(10);
    expect(config.treeStopTimeoutSeconds).toBe(60);
  });

  it("rejects the retired worker_budget key from the environment", () => {
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_BOARD_PROJECT_IDS: "PVT_x", LEGION_WORKER_BUDGET: "6" },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow("worker_budget was replaced by worker_cap");
  });

  it("rejects the retired worker_budget key from the YAML loader shape with a helpful message", () => {
    expect(() =>
      loadConfigFromFile(
        ["project: acme/7", "worker_budget: 6", "gates:", "  design: off", "  merge: off"].join(
          "\n"
        ),
        "/tmp/legion-config"
      )
    ).toThrow("worker_budget was replaced by worker_cap");
  });

  it("resolves the optional dispatch service base URL and bearer token from the environment", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_x",
        DISPATCH_URL: "http://127.0.0.1:18766",
        DISPATCH_TOKEN: "test-dispatch-token",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchUrl).toBe("http://127.0.0.1:18766");
    expect(config.dispatchToken).toBe("test-dispatch-token");
  });

  it("trims DISPATCH_TOKEN before storing it, so a copy-paste whitespace artifact never boots the daemon with a value Dispatch's own auth would reject", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_x",
        DISPATCH_URL: "http://127.0.0.1:18766",
        DISPATCH_TOKEN: "  test-dispatch-token  \n",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchToken).toBe("test-dispatch-token");
  });

  it("normalizes a trailing slash off the dispatch service base URL", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_x",
        DISPATCH_URL: "http://127.0.0.1:18766/",
        DISPATCH_TOKEN: "test-dispatch-token",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchUrl).toBe("http://127.0.0.1:18766");
  });

  it("rejects an invalid dispatch service URL", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_URL: "not a url",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(/DISPATCH_URL/);
  });

  it("rejects a dispatch service URL still carrying the /mcp alias", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_URL: "http://127.0.0.1:18766/mcp",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(/DISPATCH_URL must be the dispatch service base URL, not the \/mcp endpoint/);
  });

  it("resolves dispatch_project from the environment", () => {
    const { config } = resolveDaemonConfig({
      env: { ...requiredEnv, DISPATCH_PROJECT: "LEGION", LEGION_BOARD_PROJECT_IDS: "PVT_x" },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchProject).toBe("LEGION");
  });

  it("rejects a missing dispatch_project", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          LEGION_ID: "acme/7",
          ENVOY_NATS_URL: "nats://one:4222",
          LEGION_REPOS: "acme/widgets",
        },
      })
    ).toThrow("dispatch_project is required");
  });

  it("rejects a malformed dispatch_project", () => {
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, DISPATCH_PROJECT: "legion" } })
    ).toThrow("DISPATCH_PROJECT must match ^[A-Z][A-Z0-9]*$");
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, DISPATCH_PROJECT: "LEGION-1" } })
    ).toThrow("DISPATCH_PROJECT must match ^[A-Z][A-Z0-9]*$");
  });

  it("accepts dispatch_url from the YAML loader shape without an unknown-key warning", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "dispatch_url: http://127.0.0.1:18766",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({
      env: { ...requiredEnv, DISPATCH_TOKEN: "test-dispatch-token" },
      configFile: file,
    });

    expect(config.dispatchUrl).toBe("http://127.0.0.1:18766");
  });

  it("loads lifecycle settings from the existing YAML loader shape", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "port: 14001",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "board_project_ids:",
        "  - PVT_one",
        "repos:",
        "  - acme/widgets",
        "app_logins:",
        "  - legion-implement[bot]",
        "max_fix_attempts: 4",
        "admission_cap: 3",
        "max_recursion_depth: 6",
        "linger_hours: 24",
        "worker_cap: 2",
        "worker_stop_timeout_seconds: 20",
        "tree_stop_timeout_seconds: 45",
        "resync_interval_seconds: 120",
        "worker_boot_timeout_seconds: 90",
        "omp_invocation: mise x github:acme/oh-my-pi@18.0.3 -- omp",
        "state_dir: ./state",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({ configFile: file });

    expect(config).toMatchObject({
      project: "acme7",
      legionId: "acme/7",
      port: 14001,
      envoyUrl: "http://listener:9020",
      natsUrls: ["nats://one:4222"],
      boardProjectIds: ["PVT_one"],
      repos: ["acme/widgets"],
      appLogins: ["legion-implement[bot]"],
      maxFixAttempts: 4,
      admissionCap: 3,
      maxRecursionDepth: 6,
      lingerHours: 24,
      workerCap: 2,
      workerStopTimeoutSeconds: 20,
      treeStopTimeoutSeconds: 45,
      resyncIntervalMs: 120_000,
      workerBootTimeoutSeconds: 90,
      stateDir: "/tmp/legion-config/state",
      gates: { design: "off", merge: "off" },
      ompInvocation: "mise x github:acme/oh-my-pi@18.0.3 -- omp",
    });
  });

  it("resolves workerBootTimeoutSeconds: YAML beats env, env beats the 120 default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "board_project_ids:",
        "  - PVT_one",
        "repos:",
        "  - acme/widgets",
        "app_logins:",
        "  - legion-implement[bot]",
        "worker_boot_timeout_seconds: 90",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );

    const fromYaml = resolveDaemonConfig({ configFile: file });
    expect(fromYaml.config.workerBootTimeoutSeconds).toBe(90);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveDaemonConfig({
      configFile: file,
      env: { LEGION_WORKER_BOOT_TIMEOUT_SECONDS: "45" },
    });
    expect(fileBeatsEnv.config.workerBootTimeoutSeconds).toBe(90);

    const { config: fromEnvOnly } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_alpha",
        LEGION_WORKER_BOOT_TIMEOUT_SECONDS: "45",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.workerBootTimeoutSeconds).toBe(45);

    const { config: withoutEither } = resolveDaemonConfig({
      env: { ...requiredEnv, LEGION_BOARD_PROJECT_IDS: "PVT_alpha" },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.workerBootTimeoutSeconds).toBe(120);
  });

  it("resolves workerBootRegistrationDeadlineIntervals: YAML beats env, env beats the 3 default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "nats_urls:",
        "  - nats://one:4222",
        "dispatch_project: LEGION",
        "board_project_ids:",
        "  - PVT_one",
        "repos:",
        "  - acme/widgets",
        "app_logins:",
        "  - legion-implement[bot]",
        "worker_boot_registration_deadline_intervals: 5",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );

    const fromYaml = resolveDaemonConfig({ configFile: file });
    expect(fromYaml.config.workerBootRegistrationDeadlineIntervals).toBe(5);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveDaemonConfig({
      configFile: file,
      env: { LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS: "2" },
    });
    expect(fileBeatsEnv.config.workerBootRegistrationDeadlineIntervals).toBe(5);

    const { config: fromEnvOnly } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_alpha",
        LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS: "2",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.workerBootRegistrationDeadlineIntervals).toBe(2);

    const { config: withoutEither } = resolveDaemonConfig({
      env: { ...requiredEnv, LEGION_BOARD_PROJECT_IDS: "PVT_alpha" },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.workerBootRegistrationDeadlineIntervals).toBe(3);
  });

  it("rejects the retired dispatch_mcp_url key from the YAML loader shape with a helpful message", () => {
    expect(() =>
      loadConfigFromFile(
        [
          "project: acme/7",
          "dispatch_mcp_url: http://127.0.0.1:18766/mcp",
          "gates:",
          "  design: off",
          "  merge: off",
        ].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow("dispatch_mcp_url was replaced by dispatch_url (the service base URL, no /mcp)");
  });

  it("strips DISPATCH_TOKEN/DISPATCH_URL/DISPATCH_MCP_URL from a private_key_command child's environment", () => {
    const saved = {
      DISPATCH_TOKEN: process.env.DISPATCH_TOKEN,
      DISPATCH_URL: process.env.DISPATCH_URL,
      DISPATCH_MCP_URL: process.env.DISPATCH_MCP_URL,
    };
    // Set directly on process.env (not resolveDaemonConfig's env param): executePrivateKeyCommand
    // reads process.env for its spawnSync call, so its child must never see any of these three
    // keys from that environment.
    process.env.DISPATCH_TOKEN = "leaked-private-key-command-token";
    process.env.DISPATCH_URL = "http://leaked-private-key-command";
    process.env.DISPATCH_MCP_URL = "http://leaked-private-key-command/mcp";
    try {
      const file = loadConfigFromFile(
        [
          "project: acme/7",
          "dispatch_project: ACME",
          "repos:",
          "  - acme/widgets",
          "nats_urls:",
          "  - nats://one:4222",
          "github_apps:",
          "  implement:",
          '    app_id: "1"',
          '    private_key_command: "env"',
          "gates:",
          "  design: off",
          "  merge: off",
        ].join("\n"),
        "/tmp/legion-config"
      );
      const { config } = resolveDaemonConfig({ configFile: file });
      // `env`'s stdout (the operator's real private_key_command form) becomes the "private key"
      // here — a dump of the child's actual environment, one KEY=VALUE per line.
      const dump = config.githubApps.implement?.privateKey ?? "";

      expect(dump).not.toContain("DISPATCH_TOKEN=");
      expect(dump).not.toContain("DISPATCH_URL=");
      expect(dump).not.toContain("DISPATCH_MCP_URL=");
      expect(dump).toContain("PATH=");
    } finally {
      if (saved.DISPATCH_TOKEN === undefined) delete process.env.DISPATCH_TOKEN;
      else process.env.DISPATCH_TOKEN = saved.DISPATCH_TOKEN;
      if (saved.DISPATCH_URL === undefined) delete process.env.DISPATCH_URL;
      else process.env.DISPATCH_URL = saved.DISPATCH_URL;
      if (saved.DISPATCH_MCP_URL === undefined) delete process.env.DISPATCH_MCP_URL;
      else process.env.DISPATCH_MCP_URL = saved.DISPATCH_MCP_URL;
    }
  });

  it("rejects missing NATS configuration instead of inventing a transport", () => {
    expect(() => loadConfig({ LEGION_ID: "acme/7" })).toThrow("ENVOY_NATS_URL");
  });

  it("rejects an empty repos list", () => {
    expect(() => resolveDaemonConfig({ env: { ...requiredEnv, LEGION_REPOS: "" } })).toThrow(
      "repos is required"
    );
  });

  it("rejects a repos entry that is not owner/name", () => {
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, LEGION_REPOS: "not-a-slug" } })
    ).toThrow(/entries must be "owner\/name"/);
  });

  it("rejects invalid lifecycle numbers from either configuration source", () => {
    expect(() => loadConfig({ ...requiredEnv, LEGION_ADMISSION_CAP: "-1" })).toThrow(
      "LEGION_ADMISSION_CAP"
    );
    expect(() =>
      loadConfigFromFile("project: acme/7\nworker_cap: 1.5\n", "/tmp/legion-config")
    ).toThrow("worker_cap");
  });

  it("rejects a design gate left on without board_project_ids configured", () => {
    expect(() => resolveDaemonConfig({ env: requiredEnv })).toThrow(
      "board_project_ids is required when the design gate is on"
    );
  });

  it("rejects unknown config keys instead of silently tolerating drift", () => {
    expect(() =>
      loadConfigFromFile(["project: acme/7", "runtime: opencode"].join("\n"), "/tmp/legion-config")
    ).toThrow('Unknown config key "runtime"');
  });
});
