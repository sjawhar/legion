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
      maxFixAttempts: 5,
      admissionCap: 7,
      maxRecursionDepth: 11,
      lingerHours: 48,
      workerCap: 9,
      workerStopTimeoutSeconds: 15,
      treeStopTimeoutSeconds: 90,
      resyncIntervalMs: 600_000,
      gates: { design: "root-issues" },
      ompInvocation: "custom-omp-from-env",
    });
    expect(config.stateDir).toEndWith(path.join(".legion", "acme42"));
  });

  it("rejects DISPATCH_MCP_URL from the environment as a legacy key", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
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
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.workerStopTimeoutSeconds).toBe(10);
    expect(config.treeStopTimeoutSeconds).toBe(60);
  });

  it("rejects the retired worker_budget key from the environment", () => {
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_BUDGET: "6" },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow("worker_budget was replaced by worker_cap");
  });

  it("rejects the retired worker_budget key from the YAML loader shape with a helpful message", () => {
    expect(() =>
      loadConfigFromFile(
        ["project: acme/7", "worker_budget: 6", "gates:", "  design: off"].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow("worker_budget was replaced by worker_cap");
  });

  it("resolves the optional dispatch service base URL and bearer token from the environment", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
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
      env: { ...requiredEnv, DISPATCH_PROJECT: "LEGION" },
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
      ["project: acme/7", "dispatch_url: http://127.0.0.1:18766", "gates:", "  design: off"].join(
        "\n"
      ),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({
      env: { ...requiredEnv, DISPATCH_TOKEN: "test-dispatch-token" },
      configFile: file,
    });

    expect(config.dispatchUrl).toBe("http://127.0.0.1:18766");
  });

  it("loads a file that omits the gates block, resolving the design gate to its default", () => {
    const file = loadConfigFromFile(
      ["project: acme/7", "dispatch_project: ACME"].join("\n"),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({
      env: requiredEnv,
      configFile: file,
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });

    expect(config.gates).toEqual({ design: "root-issues" });
  });

  it("rejects a gates block that is present but not a mapping", () => {
    expect(() =>
      loadConfigFromFile(["project: acme/7", "gates: off"].join("\n"), "/tmp/legion-config")
    ).toThrow("gates must be a mapping");
  });

  it("rejects a gates.merge key: human approval is the repository's rule, not Legion's", () => {
    expect(() =>
      loadConfigFromFile(
        ["project: acme/7", "gates:", "  design: off", "  merge: human"].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow(
      "gates.merge is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes"
    );
  });

  it("rejects an app_logins key from the file and from LEGION_APP_LOGINS alike", () => {
    const message =
      "app_logins is not a Legion setting: human approval of a pull request is the repository's own branch protection or CODEOWNERS rule, which Legion never reads or writes";
    expect(() =>
      loadConfigFromFile(
        ["project: acme/7", "app_logins:", "  - legion-implement[bot]"].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow(message);
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_APP_LOGINS: "legion-implement[bot]" },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(message);
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
        "repos:",
        "  - acme/widgets",
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
      repos: ["acme/widgets"],
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
      gates: { design: "off" },
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
        "repos:",
        "  - acme/widgets",
        "worker_boot_timeout_seconds: 90",
        "gates:",
        "  design: off",
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
        LEGION_WORKER_BOOT_TIMEOUT_SECONDS: "45",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.workerBootTimeoutSeconds).toBe(45);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.workerBootTimeoutSeconds).toBe(120);
  });

  it("resolves workerRpcTimeoutSeconds: YAML beats env, env beats the 5 default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "repos:",
        "  - acme/widgets",
        "worker_rpc_timeout_seconds: 20",
        "gates:",
        "  design: off",
      ].join("\n"),
      "/tmp/legion-config"
    );

    const fromYaml = resolveDaemonConfig({ configFile: file });
    expect(fromYaml.config.workerRpcTimeoutSeconds).toBe(20);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveDaemonConfig({
      configFile: file,
      env: { LEGION_WORKER_RPC_TIMEOUT_SECONDS: "8" },
    });
    expect(fileBeatsEnv.config.workerRpcTimeoutSeconds).toBe(20);

    const { config: fromEnvOnly } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_WORKER_RPC_TIMEOUT_SECONDS: "8",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.workerRpcTimeoutSeconds).toBe(8);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.workerRpcTimeoutSeconds).toBe(5);
  });

  it("resolves workerStreamPort: YAML beats env, env beats the port + 1 default", () => {
    const cliOverrides = {
      githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
    };
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "port: 19370",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "repos:",
        "  - acme/widgets",
        "worker_stream_port: 19400",
        "gates:",
        "  design: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    expect(resolveDaemonConfig({ configFile: file }).config.workerStreamPort).toBe(19400);
    expect(
      resolveDaemonConfig({ configFile: file, env: { LEGION_WORKER_STREAM_PORT: "19500" } }).config
        .workerStreamPort
    ).toBe(19400);
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_STREAM_PORT: "19500" },
        cliOverrides,
      }).config.workerStreamPort
    ).toBe(19500);
    expect(
      resolveDaemonConfig({ env: { ...requiredEnv, LEGION_DAEMON_PORT: "14000" }, cliOverrides })
        .config.workerStreamPort
    ).toBe(14001);
    expect(resolveDaemonConfig({ env: requiredEnv, cliOverrides }).config.workerStreamPort).toBe(
      13371
    );
  });

  it("rejects a worker_stream_port that is not a TCP port or collides with port", () => {
    const cliOverrides = {
      githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
    };
    expect(() => loadConfigFromFile("worker_stream_port: 70000", "/tmp/legion-config")).toThrow(
      "worker_stream_port must be at most 65535"
    );
    expect(() => loadConfigFromFile("worker_stream_port: 0", "/tmp/legion-config")).toThrow(
      "worker_stream_port must be a positive integer"
    );
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_STREAM_PORT: "70000" },
        cliOverrides,
      })
    ).toThrow("LEGION_WORKER_STREAM_PORT must be a valid TCP port");
    expect(() =>
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: { ...cliOverrides, workerStreamPort: 70000 },
      })
    ).toThrow("workerStreamPort override must be a valid TCP port");
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_DAEMON_PORT: "14000", LEGION_WORKER_STREAM_PORT: "14000" },
        cliOverrides,
      })
    ).toThrow("worker_stream_port must differ from port (both 14000)");
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, LEGION_DAEMON_PORT: "65535" }, cliOverrides })
    ).toThrow(
      "worker_stream_port defaults to port + 1 (65536), which is not a valid TCP port; set worker_stream_port"
    );
  });

  it("resolves workerBootRegistrationDeadlineIntervals: YAML beats env, env beats the 3 default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "nats_urls:",
        "  - nats://one:4222",
        "dispatch_project: LEGION",
        "repos:",
        "  - acme/widgets",
        "worker_boot_registration_deadline_intervals: 5",
        "gates:",
        "  design: off",
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
        LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS: "2",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.workerBootRegistrationDeadlineIntervals).toBe(2);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.workerBootRegistrationDeadlineIntervals).toBe(3);
  });

  it("resolves ompLaunchPrefix: YAML beats env, env beats the empty default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "repos:",
        "  - acme/widgets",
        "omp_launch_prefix:",
        "  - secrets",
        "  - ANTHROPIC_API_KEY",
        "  - --",
        "gates:",
        "  design: off",
      ].join("\n"),
      "/tmp/legion-config"
    );

    const fromYaml = resolveDaemonConfig({ configFile: file });
    expect(fromYaml.config.ompLaunchPrefix).toEqual(["secrets", "ANTHROPIC_API_KEY", "--"]);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveDaemonConfig({
      configFile: file,
      env: { LEGION_OMP_LAUNCH_PREFIX: "other-wrapper --" },
    });
    expect(fileBeatsEnv.config.ompLaunchPrefix).toEqual(["secrets", "ANTHROPIC_API_KEY", "--"]);

    const { config: fromEnvOnly } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_OMP_LAUNCH_PREFIX: 'secrets ANTHROPIC_API_KEY GEMINI_API_KEY "OPENAI_API_KEY" --',
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(fromEnvOnly.ompLaunchPrefix).toEqual([
      "secrets",
      "ANTHROPIC_API_KEY",
      "GEMINI_API_KEY",
      "OPENAI_API_KEY",
      "--",
    ]);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(withoutEither.ompLaunchPrefix).toEqual([]);
  });

  it("rejects an omp_launch_prefix entry that is not a non-empty string, from either source", () => {
    expect(() =>
      loadConfigFromFile(
        ["project: acme/7", "omp_launch_prefix:", "  - secrets", "  - ''"].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow("omp_launch_prefix must be an array of non-empty strings");

    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_OMP_LAUNCH_PREFIX: "secrets ''" },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(
      "LEGION_OMP_LAUNCH_PREFIX must not contain an empty argument (e.g. a bare '' or \"\")"
    );
  });

  it("rejects a trailing unescaped backslash in LEGION_OMP_LAUNCH_PREFIX", () => {
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_OMP_LAUNCH_PREFIX: "secrets\\" },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow("LEGION_OMP_LAUNCH_PREFIX has a trailing unescaped backslash");
  });

  it("rejects the retired dispatch_mcp_url key from the YAML loader shape with a helpful message", () => {
    expect(() =>
      loadConfigFromFile(
        [
          "project: acme/7",
          "dispatch_mcp_url: http://127.0.0.1:18766/mcp",
          "gates:",
          "  design: off",
        ].join("\n"),
        "/tmp/legion-config"
      )
    ).toThrow("dispatch_mcp_url was replaced by dispatch_url (the service base URL, no /mcp)");
  });

  it("strips every pane-secret key (Dispatch, boot token, controller secret, and their *_FILE pointers) from a private_key_command child's environment", () => {
    const leaked: Record<string, string> = {
      DISPATCH_TOKEN: "leaked-private-key-command-token",
      DISPATCH_TOKEN_FILE: "/leaked/dispatch-token",
      DISPATCH_URL: "http://leaked-private-key-command",
      DISPATCH_MCP_URL: "http://leaked-private-key-command/mcp",
      LEGION_BOOT_TOKEN: "leaked-boot-token",
      LEGION_BOOT_TOKEN_FILE: "/leaked/legion-acme7-acme-7-architect",
      LEGION_CONTROLLER_SECRET: "leaked-controller-secret",
      LEGION_CONTROLLER_SECRET_FILE: "/leaked/legion-acme7-controller",
    };
    const saved = Object.fromEntries(Object.keys(leaked).map((key) => [key, process.env[key]]));
    // Set directly on process.env (not resolveDaemonConfig's env param): executePrivateKeyCommand
    // reads process.env for its spawnSync call, so its child must never see any of these keys
    // from that environment.
    for (const [key, value] of Object.entries(leaked)) process.env[key] = value;
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
        ].join("\n"),
        "/tmp/legion-config"
      );
      const { config } = resolveDaemonConfig({ configFile: file });
      // `env`'s stdout (the operator's real private_key_command form) becomes the "private key"
      // here — a dump of the child's actual environment, one KEY=VALUE per line.
      const dump = config.githubApps.implement?.privateKey ?? "";

      for (const key of Object.keys(leaked)) expect(dump).not.toContain(`${key}=`);
      expect(dump).toContain("PATH=");
    } finally {
      for (const [key, value] of Object.entries(saved)) {
        if (value === undefined) delete process.env[key];
        else process.env[key] = value;
      }
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

  it("rejects unknown config keys instead of silently tolerating drift", () => {
    expect(() =>
      loadConfigFromFile(["project: acme/7", "backend: opencode"].join("\n"), "/tmp/legion-config")
    ).toThrow('Unknown config key "backend"');
  });

  describe("runtime, daemon_url, and bind", () => {
    const yaml = (...lines: string[]) =>
      loadConfigFromFile(
        ["project: acme/7", "dispatch_project: ACME", "repos: [acme/widgets]", ...lines].join("\n"),
        "/tmp/legion-config"
      );
    const overrides = {
      githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
    };

    it("defaults to the tmux runtime, a loopback daemon_url on the configured port, and a loopback bind", () => {
      const { config } = resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_DAEMON_PORT: "14000" },
        cliOverrides: overrides,
      });
      expect(config.runtime).toBe("tmux");
      expect(config.daemonUrl).toBe("http://127.0.0.1:14000");
      expect(config.bind).toBe("127.0.0.1");
    });

    it("resolves all three from YAML for a kubernetes deployment", () => {
      const { config } = resolveDaemonConfig({
        configFile: yaml(
          "runtime: kubernetes",
          "daemon_url: http://legion-daemon.legion.svc:13370",
          "bind: 0.0.0.0"
        ),
        env: requiredEnv,
        cliOverrides: overrides,
      });
      expect(config.runtime).toBe("kubernetes");
      expect(config.daemonUrl).toBe("http://legion-daemon.legion.svc:13370");
      expect(config.bind).toBe("0.0.0.0");
    });

    it("rejects a runtime other than tmux or kubernetes, naming the source", () => {
      expect(() => yaml("runtime: docker")).toThrow("runtime must be 'tmux' or 'kubernetes'");
      expect(() =>
        resolveDaemonConfig({
          env: { ...requiredEnv, LEGION_RUNTIME: "docker" },
          cliOverrides: overrides,
        })
      ).toThrow("LEGION_RUNTIME must be 'tmux' or 'kubernetes'");
    });

    it("requires daemon_url under the kubernetes runtime", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: yaml("runtime: kubernetes", "bind: 0.0.0.0"),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow("daemon_url is required when runtime is kubernetes (or set LEGION_DAEMON_URL)");
    });

    it("rejects an invalid daemon_url from either source and normalizes a trailing slash", () => {
      expect(() => yaml("daemon_url: not a url")).toThrow("daemon_url must be a valid URL");
      expect(() =>
        resolveDaemonConfig({
          env: { ...requiredEnv, LEGION_DAEMON_URL: "nope" },
          cliOverrides: overrides,
        })
      ).toThrow("LEGION_DAEMON_URL must be a valid URL");
      const { config } = resolveDaemonConfig({
        configFile: yaml("daemon_url: http://h:1/"),
        env: requiredEnv,
        cliOverrides: overrides,
      });
      expect(config.daemonUrl).toBe("http://h:1");
    });

    it("rejects a non-loopback bind under the tmux runtime, from either source", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: yaml("bind: 0.0.0.0"),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow("bind must be 127.0.0.1 unless runtime is kubernetes");
      expect(() =>
        resolveDaemonConfig({
          env: { ...requiredEnv, LEGION_BIND: "0.0.0.0" },
          cliOverrides: overrides,
        })
      ).toThrow("bind must be 127.0.0.1 unless runtime is kubernetes");
    });

    it("rejects an empty bind", () => {
      expect(() => yaml('bind: ""')).toThrow("bind must not be empty");
    });

    it("lets a YAML daemon_url beat LEGION_DAEMON_URL, so a daemon started from inside a Legion pane never inherits the outer daemon's URL", () => {
      const { config } = resolveDaemonConfig({
        configFile: yaml("daemon_url: http://127.0.0.1:14100"),
        env: { ...requiredEnv, LEGION_DAEMON_URL: "http://127.0.0.1:13370" },
        cliOverrides: overrides,
      });
      expect(config.daemonUrl).toBe("http://127.0.0.1:14100");
      const { config: fromEnv } = resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_DAEMON_URL: "http://127.0.0.1:13370" },
        cliOverrides: overrides,
      });
      expect(fromEnv.daemonUrl).toBe("http://127.0.0.1:13370");
    });

    it("recognizes the three keys in the YAML loader shape", () => {
      expect(
        yaml("runtime: tmux", "daemon_url: http://127.0.0.1:14100", "bind: 127.0.0.1").fields
      ).toMatchObject({
        runtime: "tmux",
        daemonUrl: "http://127.0.0.1:14100",
        bind: "127.0.0.1",
      });
    });
  });
});
