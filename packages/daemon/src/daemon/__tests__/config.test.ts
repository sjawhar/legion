import { describe, expect, it, spyOn } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  DEFAULT_KUBERNETES_RESOURCES,
  DEFAULT_ROLE_PROFILES,
  type GitHubAppsConfig,
  loadConfig,
  loadConfigFromFile,
  type ResolveDaemonConfigOptions,
  resolveDaemonConfig,
} from "../config";

const requiredEnv = {
  LEGION_ID: "Acme/42",
  ENVOY_NATS_URL: "nats://one:4222, nats://two:4222",
  LEGION_REPOS: "acme/widgets",
  DISPATCH_PROJECT: "ACME",
};

/** Both Apps, as every valid deployment must configure them. */
const BOTH_APPS: GitHubAppsConfig = {
  implement: { appId: "1", privateKey: "test", installations: {} },
  review: { appId: "2", privateKey: "test", installations: {} },
};

/** The review App as a `legion.yaml` fragment, for file fixtures that exercise the implement
 * App's key-source rules and need the section to be complete. */
const REVIEW_APP_YAML = ["  review:", '    app_id: "2"', '    private_key: "test"'];

/** `resolveDaemonConfig` with both Apps supplied as the CLI override: the file fixtures below
 * exercise other settings and carry no `github_apps` section of their own. A call that passes its
 * own `cliOverrides` replaces this one wholesale (every such override already carries both Apps). */
function resolveWithApps(options: ResolveDaemonConfigOptions) {
  return resolveDaemonConfig({ cliOverrides: { githubApps: BOTH_APPS }, ...options });
}

const overrides = {
  githubApps: BOTH_APPS,
};

/** A kubernetes daemon must present an Envoy bearer (`envoy_token_file` or the `ENVOY_TOKEN`
 * alternative) and an operator token (`operator_token_file`, here as the programmatic override
 * `operatorToken`); the runtime tests below that are not about either token supply them this way. */
const kubernetesEnv = { ...requiredEnv, ENVOY_TOKEN: "envoy-test-token" };
const kubernetesOverrides = { githubApps: BOTH_APPS, operatorToken: "operator-test-token" };

const KUBERNETES_BLOCK = [
  "runtime:",
  "  kubernetes:",
  "    namespace: legion",
  `    image: ghcr.io/sjawhar/legion-worker@sha256:${"a".repeat(64)}`,
].join("\n");

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
        githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
        githubApps: BOTH_APPS,
      },
    });
    expect(config.dispatchToken).toBeUndefined();
  });

  it("defaults worker and tree stop timeouts to 10 and 60 seconds", () => {
    const { config } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
        githubApps: BOTH_APPS,
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
        githubApps: BOTH_APPS,
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
        githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
        },
      })
    ).toThrow(/DISPATCH_URL must be the dispatch service base URL, not the \/mcp endpoint/);
  });

  it("resolves dispatch_project from the environment", () => {
    const { config } = resolveDaemonConfig({
      env: { ...requiredEnv, DISPATCH_PROJECT: "LEGION" },
      cliOverrides: {
        githubApps: BOTH_APPS,
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
    const { config } = resolveWithApps({
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
        githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
    const { config } = resolveWithApps({ configFile: file });

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

    const fromYaml = resolveWithApps({ configFile: file });
    expect(fromYaml.config.workerBootTimeoutSeconds).toBe(90);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveWithApps({
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
        githubApps: BOTH_APPS,
      },
    });
    expect(fromEnvOnly.workerBootTimeoutSeconds).toBe(45);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: BOTH_APPS,
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

    const fromYaml = resolveWithApps({ configFile: file });
    expect(fromYaml.config.workerRpcTimeoutSeconds).toBe(20);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveWithApps({
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
        githubApps: BOTH_APPS,
      },
    });
    expect(fromEnvOnly.workerRpcTimeoutSeconds).toBe(8);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: BOTH_APPS,
      },
    });
    expect(withoutEither.workerRpcTimeoutSeconds).toBe(5);
  });

  it("resolves workerIdleRetireSeconds: YAML beats env, env beats the 600 default, and 0 is valid from either source", () => {
    const yaml = (seconds: number) =>
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "repos:",
        "  - acme/widgets",
        `worker_idle_retire_seconds: ${seconds}`,
        "gates:",
        "  design: off",
      ].join("\n");
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };
    const file = loadConfigFromFile(yaml(900), "/tmp/legion-config");

    expect(resolveWithApps({ configFile: file }).config.workerIdleRetireSeconds).toBe(900);
    // Config-file value wins over env (`resolveValue`: cli > config > env > default).
    expect(
      resolveWithApps({ configFile: file, env: { LEGION_WORKER_IDLE_RETIRE_SECONDS: "30" } }).config
        .workerIdleRetireSeconds
    ).toBe(900);
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "30" },
        cliOverrides,
      }).config.workerIdleRetireSeconds
    ).toBe(30);
    expect(
      resolveDaemonConfig({ env: requiredEnv, cliOverrides }).config.workerIdleRetireSeconds
    ).toBe(600);

    // `0` disables the timer: unlike every other seconds key it must survive both parsers.
    expect(
      resolveWithApps({ configFile: loadConfigFromFile(yaml(0), "/tmp/legion-config") }).config
        .workerIdleRetireSeconds
    ).toBe(0);
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "0" },
        cliOverrides,
      }).config.workerIdleRetireSeconds
    ).toBe(0);
  });

  it("rejects a negative or non-integer worker_idle_retire_seconds from either source, naming the key", () => {
    expect(() =>
      loadConfigFromFile("project: acme/7\nworker_idle_retire_seconds: -1\n", "/tmp/legion-config")
    ).toThrow("worker_idle_retire_seconds must be a non-negative integer");
    expect(() =>
      loadConfigFromFile("project: acme/7\nworker_idle_retire_seconds: 1.5\n", "/tmp/legion-config")
    ).toThrow("worker_idle_retire_seconds must be a non-negative integer");
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "-1" } })
    ).toThrow("LEGION_WORKER_IDLE_RETIRE_SECONDS must be a non-negative integer");
    expect(() =>
      resolveDaemonConfig({ env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "ten" } })
    ).toThrow("LEGION_WORKER_IDLE_RETIRE_SECONDS must be a non-negative integer");
  });

  it("bounds worker_idle_retire_seconds at 2147483 from either source: the boundary is accepted, one more is a startup error naming the key, the bound, and 0 as the disable value", () => {
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };
    // 2147483 * 1000 = 2_147_483_000 ms fits a signed 32-bit timer delay; 2147484 * 1000 does not,
    // and the runtime would clamp it to 1 ms -- retiring every finished worker the instant it
    // went idle. The bound must sit exactly at the last whole second that fits.
    expect(
      resolveWithApps({
        configFile: loadConfigFromFile(
          [
            "project: acme/7",
            "envoy_url: http://listener:9020",
            "dispatch_project: ACME",
            "nats_urls:",
            "  - nats://one:4222",
            "repos:",
            "  - acme/widgets",
            "worker_idle_retire_seconds: 2147483",
            "gates:",
            "  design: off",
          ].join("\n"),
          "/tmp/legion-config"
        ),
      }).config.workerIdleRetireSeconds
    ).toBe(2_147_483);
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "2147483" },
        cliOverrides,
      }).config.workerIdleRetireSeconds
    ).toBe(2_147_483);
    expect(() =>
      loadConfigFromFile(
        "project: acme/7\nworker_idle_retire_seconds: 2147484\n",
        "/tmp/legion-config"
      )
    ).toThrow(
      "worker_idle_retire_seconds must be at most 2147483; use 0 to disable idle retirement"
    );
    expect(() =>
      loadConfigFromFile(
        "project: acme/7\nworker_idle_retire_seconds: 31536000\n",
        "/tmp/legion-config"
      )
    ).toThrow("worker_idle_retire_seconds must be at most 2147483");
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "2147484" },
        cliOverrides,
      })
    ).toThrow(
      "LEGION_WORKER_IDLE_RETIRE_SECONDS must be at most 2147483; use 0 to disable idle retirement"
    );
    expect(() =>
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: { ...cliOverrides, workerIdleRetireSeconds: 2_147_484 },
      })
    ).toThrow("workerIdleRetireSeconds must be at most 2147483");
  });

  it("treats only the literal 0 as the disable value: a blank, -0, padded, or non-canonical worker_idle_retire_seconds is a startup error naming the key", () => {
    // `Number("  ")` and `Number("-0")` are both zero and `Number("05")`/`Number("+5")` are five, so
    // a parser built on `Number()` alone would let a mistyped variable silently disable the timer
    // (or silently succeed). An unset or empty variable stays "unset" exactly as
    // `parseEnvPositiveInteger` treats it; everything else must be the canonical decimal spelling.
    for (const value of [" ", "-0", "05", "+5", " 5", "5 ", "0x10", "1e3"]) {
      expect(() =>
        resolveDaemonConfig({ env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: value } })
      ).toThrow("LEGION_WORKER_IDLE_RETIRE_SECONDS must be a non-negative integer");
    }
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_WORKER_IDLE_RETIRE_SECONDS: "" },
        cliOverrides: {
          githubApps: BOTH_APPS,
        },
      }).config.workerIdleRetireSeconds
    ).toBe(600);
    // The YAML loader hands `-0` through as a negative zero; it must not read as "disabled" either.
    expect(() =>
      loadConfigFromFile("project: acme/7\nworker_idle_retire_seconds: -0\n", "/tmp/legion-config")
    ).toThrow("worker_idle_retire_seconds must be a non-negative integer");
  });

  it("refuses every timer setting above its bound from the file, naming the key and the bound", () => {
    // One row per key: each defends its own call site (a `max` forgotten on one key), so a shared
    // helper passing for one key proves nothing about the others.
    for (const [key, value, bound] of [
      ["worker_stop_timeout_seconds", 2147484, 2147483],
      ["tree_stop_timeout_seconds", 2147484, 2147483],
      ["worker_boot_timeout_seconds", 2147484, 2147483],
      ["worker_rpc_timeout_seconds", 2147484, 2147483],
      ["resync_interval_seconds", 2147484, 2147483],
      ["slow_command_timeout_seconds", 2147484, 2147483],
      ["linger_hours", 597, 596],
    ] as const) {
      expect(() =>
        loadConfigFromFile(`project: acme/7\n${key}: ${value}\n`, "/tmp/legion-config")
      ).toThrow(`${key} must be at most ${bound}`);
    }
  });

  it("refuses every timer setting above its bound from its LEGION_* variable, naming the variable and the bound", () => {
    for (const [name, value, bound] of [
      ["LEGION_WORKER_STOP_TIMEOUT_SECONDS", "2147484", 2147483],
      ["LEGION_TREE_STOP_TIMEOUT_SECONDS", "2147484", 2147483],
      ["LEGION_WORKER_BOOT_TIMEOUT_SECONDS", "2147484", 2147483],
      ["LEGION_WORKER_RPC_TIMEOUT_SECONDS", "2147484", 2147483],
      ["LEGION_RESYNC_INTERVAL_SECONDS", "2147484", 2147483],
      ["LEGION_SLOW_COMMAND_TIMEOUT_SECONDS", "2147484", 2147483],
      ["LEGION_LINGER_HOURS", "597", 596],
    ] as const) {
      expect(() => resolveDaemonConfig({ env: { ...requiredEnv, [name]: value } })).toThrow(
        `${name} must be at most ${bound}`
      );
    }
  });

  it("accepts resync_interval_seconds exactly at 2147483 from either source and refuses 2147484: the post-resolve check judges milliseconds", () => {
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };
    // The file value is multiplied into milliseconds by the file loader and the env value only in
    // resolveDaemonConfig's return, while a cliOverride is already milliseconds; the post-resolve
    // check therefore judges the field in milliseconds (2147483 s = 2_147_483_000 ms fits).
    expect(
      resolveWithApps({
        configFile: loadConfigFromFile(
          [
            "project: acme/7",
            "envoy_url: http://listener:9020",
            "dispatch_project: ACME",
            "nats_urls:",
            "  - nats://one:4222",
            "repos:",
            "  - acme/widgets",
            "resync_interval_seconds: 2147483",
            "gates:",
            "  design: off",
          ].join("\n"),
          "/tmp/legion-config"
        ),
      }).config.resyncIntervalMs
    ).toBe(2_147_483_000);
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_RESYNC_INTERVAL_SECONDS: "2147483" },
        cliOverrides,
      }).config.resyncIntervalMs
    ).toBe(2_147_483_000);
    expect(() =>
      loadConfigFromFile(
        "project: acme/7\nresync_interval_seconds: 2147484\n",
        "/tmp/legion-config"
      )
    ).toThrow("resync_interval_seconds must be at most 2147483");
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_RESYNC_INTERVAL_SECONDS: "2147484" },
        cliOverrides,
      })
    ).toThrow("LEGION_RESYNC_INTERVAL_SECONDS must be at most 2147483");
    expect(() =>
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: { ...cliOverrides, resyncIntervalMs: 2_147_483_001 },
      })
    ).toThrow("resyncIntervalMs must be at most 2147483000");
    expect(
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: { ...cliOverrides, resyncIntervalMs: 2_147_483_000 },
      }).config.resyncIntervalMs
    ).toBe(2_147_483_000);
  });

  it("refuses a cliOverride above the bound post-resolve, naming the field", () => {
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };
    expect(() =>
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: { ...cliOverrides, workerStopTimeoutSeconds: 2_147_484 },
      })
    ).toThrow("workerStopTimeoutSeconds must be at most 2147483");
    expect(() =>
      resolveDaemonConfig({ env: requiredEnv, cliOverrides: { ...cliOverrides, lingerHours: 597 } })
    ).toThrow("lingerHours must be at most 596");
  });

  it("bounds the root registration deadline, the product of worker_boot_timeout_seconds and worker_boot_registration_deadline_intervals, at 2147483 s", () => {
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };
    // Each factor is within its own bound; only the product overflows the one timer that
    // multiplies them (the root/controller registration deadline). The file supplies the pair,
    // `requiredEnv` the rest.
    expect(() =>
      resolveWithApps({
        configFile: loadConfigFromFile(
          "worker_boot_timeout_seconds: 1000000\nworker_boot_registration_deadline_intervals: 3\n",
          "/tmp/legion-config"
        ),
        env: requiredEnv,
        cliOverrides,
      })
    ).toThrow(
      "worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals must be at most 2147483"
    );
    expect(
      resolveWithApps({
        configFile: loadConfigFromFile(
          "worker_boot_timeout_seconds: 2147483\nworker_boot_registration_deadline_intervals: 1\n",
          "/tmp/legion-config"
        ),
        env: requiredEnv,
        cliOverrides,
      }).config.workerBootTimeoutSeconds
    ).toBe(2_147_483);
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_WORKER_BOOT_TIMEOUT_SECONDS: "715828",
          LEGION_WORKER_BOOT_REGISTRATION_DEADLINE_INTERVALS: "3",
        },
        cliOverrides,
      })
    ).toThrow(
      "worker_boot_timeout_seconds * worker_boot_registration_deadline_intervals must be at most 2147483"
    );
  });

  it("resolves slowCommandTimeoutSeconds: YAML beats env, env beats the 300 default", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "envoy_url: http://listener:9020",
        "dispatch_project: ACME",
        "nats_urls:",
        "  - nats://one:4222",
        "repos:",
        "  - acme/widgets",
        "slow_command_timeout_seconds: 900",
        "gates:",
        "  design: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    const cliOverrides = {
      githubApps: BOTH_APPS,
    };

    const fromYaml = resolveWithApps({ configFile: file });
    expect(fromYaml.config.slowCommandTimeoutSeconds).toBe(900);

    const fileBeatsEnv = resolveWithApps({
      configFile: file,
      env: { LEGION_SLOW_COMMAND_TIMEOUT_SECONDS: "120" },
    });
    expect(fileBeatsEnv.config.slowCommandTimeoutSeconds).toBe(900);

    const { config: fromEnvOnly } = resolveDaemonConfig({
      env: { ...requiredEnv, LEGION_SLOW_COMMAND_TIMEOUT_SECONDS: "120" },
      cliOverrides,
    });
    expect(fromEnvOnly.slowCommandTimeoutSeconds).toBe(120);

    const { config: withoutEither } = resolveDaemonConfig({ env: requiredEnv, cliOverrides });
    expect(withoutEither.slowCommandTimeoutSeconds).toBe(300);
  });

  it("refuses a slow_command_timeout_seconds that is not a positive integer, naming the key", () => {
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_SLOW_COMMAND_TIMEOUT_SECONDS: "0" },
        cliOverrides: {
          githubApps: BOTH_APPS,
        },
      })
    ).toThrow("LEGION_SLOW_COMMAND_TIMEOUT_SECONDS");
    expect(() =>
      loadConfigFromFile(
        "project: acme/7\nslow_command_timeout_seconds: -5\n",
        "/tmp/legion-config"
      )
    ).toThrow("slow_command_timeout_seconds");
  });

  it("resolves workerStreamPort: YAML beats env, env beats the port + 1 default", () => {
    const cliOverrides = {
      githubApps: BOTH_APPS,
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
    expect(resolveWithApps({ configFile: file }).config.workerStreamPort).toBe(19400);
    expect(
      resolveWithApps({ configFile: file, env: { LEGION_WORKER_STREAM_PORT: "19500" } }).config
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
      githubApps: BOTH_APPS,
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

    const fromYaml = resolveWithApps({ configFile: file });
    expect(fromYaml.config.workerBootRegistrationDeadlineIntervals).toBe(5);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveWithApps({
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
        githubApps: BOTH_APPS,
      },
    });
    expect(fromEnvOnly.workerBootRegistrationDeadlineIntervals).toBe(2);

    const { config: withoutEither } = resolveDaemonConfig({
      env: requiredEnv,
      cliOverrides: {
        githubApps: BOTH_APPS,
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

    const fromYaml = resolveWithApps({ configFile: file });
    expect(fromYaml.config.ompLaunchPrefix).toEqual(["secrets", "ANTHROPIC_API_KEY", "--"]);

    // Config-file value wins over env, matching every other lifecycle setting's precedence
    // (`resolveValue`: cli > config > env > default).
    const fileBeatsEnv = resolveWithApps({
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
        githubApps: BOTH_APPS,
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
        githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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
          githubApps: BOTH_APPS,
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

    it("defaults to the tmux runtime, a loopback daemon_url on the configured port, and a loopback bind", () => {
      const { config } = resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_DAEMON_PORT: "14000" },
        cliOverrides: overrides,
      });
      expect(config.runtime).toEqual({ name: "tmux" });
      expect(config.daemonUrl).toBe("http://127.0.0.1:14000");
      expect(config.bind).toBe("127.0.0.1");
    });

    it("resolves all three from YAML for a kubernetes deployment", () => {
      const { config } = resolveWithApps({
        configFile: yaml(
          KUBERNETES_BLOCK,
          "daemon_url: http://legion-daemon.legion.svc:13370",
          "bind: 0.0.0.0"
        ),
        env: kubernetesEnv,
        cliOverrides: kubernetesOverrides,
      });
      expect(config.runtime.name).toBe("kubernetes");
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

    it("refuses session_store under the tmux runtime naming the field: the key exists only inside runtime.kubernetes", () => {
      expect(() => yaml("runtime: tmux", "session_store: postgres")).toThrow(
        'Unknown config key "session_store"'
      );
      expect(() => yaml("runtime:", "  tmux:", "    session_store: postgres")).toThrow(
        "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes"
      );
    });

    it("requires daemon_url under the kubernetes runtime", () => {
      expect(() =>
        resolveWithApps({
          configFile: yaml(KUBERNETES_BLOCK, "bind: 0.0.0.0"),
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
      const { config } = resolveWithApps({
        configFile: yaml(KUBERNETES_BLOCK, "bind: 0.0.0.0", "daemon_url: http://h:1/"),
        env: kubernetesEnv,
        cliOverrides: kubernetesOverrides,
      });
      expect(config.daemonUrl).toBe("http://h:1");
    });

    it("rejects a non-loopback bind under the tmux runtime, from either source", () => {
      expect(() =>
        resolveWithApps({
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

    it("under tmux, rejects any daemon_url but the loopback default, naming the source of an inherited value", () => {
      const refusal = (got: string, port = 13370) =>
        `daemon_url must be http://127.0.0.1:${port} when runtime is tmux (got ${got}; an inherited LEGION_DAEMON_URL from an outer Legion pane?)`;
      // An env-sourced value on another port: exactly what a daemon started inside a Legion pane
      // inherits from the outer daemon.
      expect(() =>
        resolveDaemonConfig({
          env: {
            ...requiredEnv,
            LEGION_DAEMON_URL: "http://127.0.0.1:13370",
            LEGION_DAEMON_PORT: "14100",
          },
          cliOverrides: overrides,
        })
      ).toThrow(refusal("http://127.0.0.1:13370", 14100));
      // A YAML value on another port, or another host, is just as wrong under tmux.
      expect(() =>
        resolveWithApps({
          configFile: yaml("daemon_url: http://127.0.0.1:14100"),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow(refusal("http://127.0.0.1:14100"));
      expect(() =>
        resolveWithApps({
          configFile: yaml("port: 14100", "daemon_url: http://legion-daemon:14100"),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow(refusal("http://legion-daemon:14100", 14100));
      // The default, and an explicit value equal to it from either source, are accepted — the
      // smoke rig writes `daemon_url: http://127.0.0.1:${daemon_port}` into its legion.yaml.
      expect(
        resolveDaemonConfig({ env: requiredEnv, cliOverrides: overrides }).config.daemonUrl
      ).toBe("http://127.0.0.1:13370");
      expect(
        resolveWithApps({
          configFile: yaml("port: 19370", "daemon_url: http://127.0.0.1:19370/"),
          env: requiredEnv,
          cliOverrides: overrides,
        }).config.daemonUrl
      ).toBe("http://127.0.0.1:19370");
      expect(
        resolveDaemonConfig({
          env: { ...requiredEnv, LEGION_DAEMON_URL: "http://127.0.0.1:13370" },
          cliOverrides: overrides,
        }).config.daemonUrl
      ).toBe("http://127.0.0.1:13370");
    });

    it("lets a YAML daemon_url beat LEGION_DAEMON_URL (kubernetes, where the value is free), so a daemon started from inside a Legion pane never inherits the outer daemon's URL", () => {
      const { config } = resolveWithApps({
        configFile: yaml(
          KUBERNETES_BLOCK,
          "bind: 0.0.0.0",
          "daemon_url: http://legion-daemon.legion.svc:13370"
        ),
        env: { ...kubernetesEnv, LEGION_DAEMON_URL: "http://127.0.0.1:13370" },
        cliOverrides: kubernetesOverrides,
      });
      expect(config.daemonUrl).toBe("http://legion-daemon.legion.svc:13370");
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

  describe("envoy_token_file / ENVOY_TOKEN_FILE / ENVOY_TOKEN", () => {
    const configDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-config-envoy-"));
    const tokenFile = path.join(configDir, "envoy-token");
    fs.writeFileSync(tokenFile, "  file-token\n", { mode: 0o600 });
    const otherFile = path.join(configDir, "other-token");
    fs.writeFileSync(otherFile, "env-file-token\n", { mode: 0o600 });
    fs.writeFileSync(path.join(configDir, "blank"), " \n", { mode: 0o600 });
    const kubernetesYaml = (...lines: string[]) =>
      loadConfigFromFile(
        [
          "project: acme/7",
          "dispatch_project: ACME",
          "repos: [acme/widgets]",
          KUBERNETES_BLOCK,
          "daemon_url: http://legion-daemon.legion.svc:13370",
          "bind: 0.0.0.0",
          ...lines,
        ].join("\n"),
        configDir
      );

    it("is optional under tmux: nothing set leaves envoyToken undefined", () => {
      expect(
        resolveDaemonConfig({ env: requiredEnv, cliOverrides: overrides }).config.envoyToken
      ).toBeUndefined();
    });

    it("refuses a kubernetes daemon with no token, naming the key and its environment form", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow(
        "envoy_token_file is required when runtime is kubernetes (or set ENVOY_TOKEN_FILE)"
      );
    });

    it("reads the trimmed file named by envoy_token_file, resolving a relative path against the config directory", () => {
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml("envoy_token_file: ./envoy-token"),
          env: requiredEnv,
          cliOverrides: kubernetesOverrides,
        }).config.envoyToken
      ).toBe("file-token");
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml(`envoy_token_file: ${tokenFile}`),
          env: requiredEnv,
          cliOverrides: kubernetesOverrides,
        }).config.envoyToken
      ).toBe("file-token");
    });

    it("lets the file key beat ENVOY_TOKEN_FILE, and ENVOY_TOKEN_FILE beat ENVOY_TOKEN", () => {
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml("envoy_token_file: ./envoy-token"),
          env: { ...requiredEnv, ENVOY_TOKEN_FILE: otherFile, ENVOY_TOKEN: "plain" },
          cliOverrides: kubernetesOverrides,
        }).config.envoyToken
      ).toBe("file-token");
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: { ...requiredEnv, ENVOY_TOKEN_FILE: otherFile, ENVOY_TOKEN: "plain" },
          cliOverrides: kubernetesOverrides,
        }).config.envoyToken
      ).toBe("env-file-token");
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: { ...requiredEnv, ENVOY_TOKEN: " plain \n" },
          cliOverrides: kubernetesOverrides,
        }).config.envoyToken
      ).toBe("plain");
    });

    it("refuses a set-but-missing, unreadable, or blank file naming the key and the resolved path, from either source, never falling back to ENVOY_TOKEN", () => {
      const missing = path.join(configDir, "nope");
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml("envoy_token_file: ./nope"),
          env: { ...requiredEnv, ENVOY_TOKEN: "plain" },
          cliOverrides: overrides,
        })
      ).toThrow(`envoy_token_file names ${missing}, which could not be read: ENOENT`);
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: { ...requiredEnv, ENVOY_TOKEN_FILE: missing, ENVOY_TOKEN: "plain" },
          cliOverrides: overrides,
        })
      ).toThrow(`ENVOY_TOKEN_FILE names ${missing}, which could not be read: ENOENT`);
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml("envoy_token_file: ./blank"),
          env: { ...requiredEnv, ENVOY_TOKEN: "plain" },
          cliOverrides: overrides,
        })
      ).toThrow(`envoy_token_file names ${path.join(configDir, "blank")}, which is empty`);
      // A tmux daemon that names a file is held to the same rule: the key is set, so it must work.
      expect(() =>
        resolveDaemonConfig({
          env: { ...requiredEnv, ENVOY_TOKEN_FILE: missing },
          cliOverrides: overrides,
        })
      ).toThrow(`ENVOY_TOKEN_FILE names ${missing}, which could not be read: ENOENT`);
    });

    it("rejects an empty envoy_token_file key", () => {
      expect(() => kubernetesYaml("envoy_token_file: ''")).toThrow(
        "envoy_token_file must not be empty"
      );
    });

    it("under --check-config (resolveSecrets: false) validates the pointer but never reads the file: an in-cluster legion.yaml checks out on a machine without the mount", () => {
      const missing = path.join(configDir, "nope");
      const { config } = resolveDaemonConfig({
        configFile: kubernetesYaml("envoy_token_file: ./nope"),
        env: requiredEnv,
        cliOverrides: kubernetesOverrides,
        resolveSecrets: false,
      });
      expect(config.envoyToken).toBe("(not executed)");
      expect(fs.existsSync(missing)).toBe(false);
      // The pointer is still what the kubernetes rule requires: no pointer, no placeholder.
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: requiredEnv,
          cliOverrides: overrides,
          resolveSecrets: false,
        })
      ).toThrow(
        "envoy_token_file is required when runtime is kubernetes (or set ENVOY_TOKEN_FILE)"
      );
    });
  });

  describe("operator_token_file", () => {
    const configDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-config-operator-"));
    fs.writeFileSync(path.join(configDir, "operator-token"), "tok\n", { mode: 0o600 });
    fs.writeFileSync(path.join(configDir, "blank"), " \n", { mode: 0o600 });
    const kubernetesYaml = (...lines: string[]) =>
      loadConfigFromFile(
        [
          "project: acme/7",
          "dispatch_project: ACME",
          "repos: [acme/widgets]",
          KUBERNETES_BLOCK,
          "daemon_url: http://legion-daemon.legion.svc:13370",
          "bind: 0.0.0.0",
          ...lines,
        ].join("\n"),
        configDir
      );
    const tmuxYaml = (...lines: string[]) =>
      loadConfigFromFile(
        ["project: acme/7", "dispatch_project: ACME", "repos: [acme/widgets]", ...lines].join("\n"),
        configDir
      );

    it("is required under runtime: kubernetes", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml(),
          env: kubernetesEnv,
          cliOverrides: overrides,
        })
      ).toThrow(
        "operator_token_file is required when runtime is kubernetes: the daemon cannot launch the controller there; legion controller start presents this token"
      );
    });

    it("is refused under tmux, whose daemon launches its own controller", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: tmuxYaml("operator_token_file: ./operator-token"),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow(
        "operator_token_file is only used when runtime is kubernetes: the tmux daemon launches its own controller; remove operator_token_file"
      );
    });

    it("resolves a relative path against the config directory and reads the trimmed contents", () => {
      expect(
        resolveDaemonConfig({
          configFile: kubernetesYaml("operator_token_file: operator-token"),
          env: kubernetesEnv,
          cliOverrides: overrides,
        }).config.operatorToken
      ).toBe("tok");
    });

    it("refuses a blank file naming the key and the path", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: kubernetesYaml("operator_token_file: ./blank"),
          env: kubernetesEnv,
          cliOverrides: overrides,
        })
      ).toThrow(`operator_token_file names ${path.join(configDir, "blank")}, which is empty`);
    });

    it("under --check-config never reads the file", () => {
      const missing = path.join(configDir, "nope");
      const { config } = resolveDaemonConfig({
        configFile: kubernetesYaml("operator_token_file: ./nope"),
        env: kubernetesEnv,
        cliOverrides: overrides,
        resolveSecrets: false,
      });
      expect(config.operatorToken).toBe("(not executed)");
      expect(fs.existsSync(missing)).toBe(false);
    });
  });

  describe("runtime.kubernetes", () => {
    const digest = `ghcr.io/sjawhar/legion-worker@sha256:${"a".repeat(64)}`;
    const block = (...lines: string[]) =>
      [
        "runtime:",
        "  kubernetes:",
        "    namespace: legion",
        `    image: ${digest}`,
        ...lines.map((l) => `    ${l}`),
      ].join("\n");
    const yaml = (...lines: string[]) =>
      loadConfigFromFile(
        ["project: acme/7", "dispatch_project: ACME", "repos: [acme/widgets]", ...lines].join("\n"),
        "/tmp/legion-config"
      );
    const resolve = (...lines: string[]) =>
      resolveDaemonConfig({
        configFile: yaml(
          "daemon_url: http://legion-daemon.legion.svc:13370",
          "bind: 0.0.0.0",
          ...lines
        ),
        env: kubernetesEnv,
        cliOverrides: kubernetesOverrides,
      }).config;

    it("selects kubernetes from the mapping form and applies the §3 defaults", () => {
      const config = resolve(block());
      expect(config.runtime.name).toBe("kubernetes");
      expect(config.runtime).toEqual({
        name: "kubernetes",
        namespace: "legion",
        image: {
          reference: digest,
          name: "ghcr.io/sjawhar/legion-worker",
          digest: `sha256:${"a".repeat(64)}`,
        },
        treeVolume: "20Gi",
        sessionStore: { kind: "pvc" },
        resources: DEFAULT_KUBERNETES_RESOURCES,
        roleProfiles: DEFAULT_ROLE_PROFILES,
      });
    });

    it("reads every optional field, resolving a relative kubeconfig against the config directory, and lets a profile override one quantity", () => {
      const config = resolve(
        block(
          "storage_class: gp3",
          "tree_volume: 50Gi",
          "kubeconfig: ./kind.kubeconfig",
          "resources:",
          "  large:",
          "    limits:",
          "      memory: 24Gi",
          "role_profiles:",
          "  planner: medium"
        )
      );
      if (config.runtime.name !== "kubernetes") throw new Error("expected the kubernetes runtime");
      expect(config.runtime.storageClass).toBe("gp3");
      expect(config.runtime.treeVolume).toBe("50Gi");
      expect(config.runtime.kubeconfig).toBe("/tmp/legion-config/kind.kubeconfig");
      expect(config.runtime.resources.large).toEqual({
        ...DEFAULT_KUBERNETES_RESOURCES.large,
        limits: { ...DEFAULT_KUBERNETES_RESOURCES.large.limits, memory: "24Gi" },
      });
      expect(config.runtime.roleProfiles).toEqual({
        ...DEFAULT_ROLE_PROFILES,
        planner: "medium",
      });
    });

    it("selects the postgres session store with its providers-Secret key, and pvc when named explicitly", () => {
      expect(
        resolve(block("session_store: postgres", "session_dsn_secret: SESSION_DSN")).runtime
      ).toMatchObject({ sessionStore: { kind: "postgres", dsnSecretKey: "SESSION_DSN" } });
      expect(resolve(block("session_store: pvc")).runtime).toMatchObject({
        sessionStore: { kind: "pvc" },
      });
    });

    it.each([
      [
        "runtime: kubernetes",
        "runtime.kubernetes is required when runtime is kubernetes: set runtime.kubernetes.namespace and runtime.kubernetes.image in legion.yaml",
      ],
      [
        ["runtime:", "  kubernetes:", `    image: ${digest}`].join("\n"),
        "runtime.kubernetes.namespace is required",
      ],
      [
        ["runtime:", "  kubernetes:", "    namespace: legion"].join("\n"),
        "runtime.kubernetes.image is required",
      ],
      [
        [
          "runtime:",
          "  kubernetes:",
          "    namespace: legion",
          "    image: ghcr.io/sjawhar/legion-worker:latest",
        ].join("\n"),
        "runtime.kubernetes.image must be pinned by digest (@sha256:…)",
      ],
      [
        block("tree_volume: twenty"),
        "runtime.kubernetes.tree_volume must be a Kubernetes quantity (e.g. 20Gi)",
      ],
      [block("resources:", "  huge: {}"), 'Unknown config key "runtime.kubernetes.resources.huge"'],
      [
        block("role_profiles:", "  tester: enormous"),
        "runtime.kubernetes.role_profiles.tester must be one of small, medium, large",
      ],
      [
        block("role_profiles:", "  janitor: small"),
        'Unknown config key "runtime.kubernetes.role_profiles.janitor"',
      ],
      [
        "runtime: {}",
        "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes",
      ],
      [
        ["runtime:", "  tmux: {}"].join("\n"),
        "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes",
      ],
      [
        ["runtime:", "  kubernetes: {}", "  tmux: {}"].join("\n"),
        "runtime accepts tmux, kubernetes, or a mapping with the single key kubernetes",
      ],
      [["runtime:", "  kubernetes: kubernetes"].join("\n"), "runtime.kubernetes must be a mapping"],
      [
        block("session_store: sqlite"),
        "runtime.kubernetes.session_store must be 'pvc' or 'postgres'",
      ],
      [block("session_store: 1"), "runtime.kubernetes.session_store must be a string"],
      [
        block("session_store: postgres"),
        "runtime.kubernetes.session_dsn_secret is required when runtime.kubernetes.session_store is postgres",
      ],
      [
        block("session_store: postgres", "session_dsn_secret: ''"),
        "runtime.kubernetes.session_dsn_secret must not be empty",
      ],
      [
        block("session_store: postgres", "session_dsn_secret: sub/dir"),
        "runtime.kubernetes.session_dsn_secret must be a Secret data key ([-._a-zA-Z0-9]+)",
      ],
      [
        block("session_store: postgres", "session_dsn_secret: OMP_SESSION_SQL_DSN_FILE"),
        "runtime.kubernetes.session_dsn_secret must not be OMP_SESSION_STORAGE or OMP_SESSION_SQL_DSN_FILE: the worker shim exports every providers key into Oh My Pi's environment, and that name would shadow the daemon's value",
      ],
      [
        block("session_store: postgres", "session_dsn_secret: OMP_SESSION_STORAGE"),
        "runtime.kubernetes.session_dsn_secret must not be OMP_SESSION_STORAGE or OMP_SESSION_SQL_DSN_FILE: the worker shim exports every providers key into Oh My Pi's environment, and that name would shadow the daemon's value",
      ],
      [
        block("session_dsn_secret: SESSION_DSN"),
        "runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is pvc; remove it",
      ],
      [
        block("session_store: pvc", "session_dsn_secret: SESSION_DSN"),
        "runtime.kubernetes.session_dsn_secret is not used when runtime.kubernetes.session_store is pvc; remove it",
      ],
    ])("refuses %s naming the field", (lines, message) => {
      expect(() => resolve(lines)).toThrow(message);
    });

    it("refuses omp_launch_prefix under kubernetes with the migration message, from either source", () => {
      const message =
        "omp_launch_prefix is not used when runtime is kubernetes: provider keys come from the mounted Secret legion-acme7-providers; remove omp_launch_prefix (or LEGION_OMP_LAUNCH_PREFIX)";
      expect(() => resolve(block(), "omp_launch_prefix: [secrets, KEY, --]")).toThrow(message);
      expect(() =>
        resolveDaemonConfig({
          configFile: yaml("daemon_url: http://h:1", "bind: 0.0.0.0", block()),
          env: { ...kubernetesEnv, LEGION_OMP_LAUNCH_PREFIX: "secrets KEY --" },
          cliOverrides: {
            githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
            operatorToken: "operator-test-token",
          },
        })
      ).toThrow(message);
    });

    it("keeps daemon_url required under the mapping form too", () => {
      expect(() =>
        resolveDaemonConfig({
          configFile: yaml("bind: 0.0.0.0", block()),
          env: requiredEnv,
          cliOverrides: overrides,
        })
      ).toThrow("daemon_url is required when runtime is kubernetes (or set LEGION_DAEMON_URL)");
    });

    it("the file's runtime outranks LEGION_RUNTIME (existing precedence) and the disagreement is logged once", () => {
      const warn = spyOn(console, "warn").mockImplementation(() => {});
      try {
        const config = resolveDaemonConfig({
          configFile: yaml("daemon_url: http://h:1", "bind: 0.0.0.0", block()),
          env: { ...kubernetesEnv, LEGION_RUNTIME: "tmux" },
          cliOverrides: kubernetesOverrides,
        }).config;
        expect(config.runtime.name).toBe("kubernetes");
        expect(warn.mock.calls.map((c) => c[0])).toEqual([
          "[legion] LEGION_RUNTIME=tmux ignored: legion.yaml's runtime.kubernetes block selects kubernetes (the file outranks the environment)",
        ]);
      } finally {
        warn.mockRestore();
      }
    });
  });

  it("resolves instructions: a relative file key against the config dir, an absolute one as given, LEGION_INSTRUCTIONS as given when the file omits it, undefined when neither is set", () => {
    const relative = loadConfigFromFile(
      ["project: acme/7", "instructions: ./deployment.md"].join("\n"),
      "/tmp/legion-config"
    );
    expect(
      resolveWithApps({ configFile: relative, env: requiredEnv }).config.instructionsPath
    ).toBe("/tmp/legion-config/deployment.md");

    const absolute = loadConfigFromFile(
      ["project: acme/7", "instructions: /srv/legion/deployment.md"].join("\n"),
      "/tmp/legion-config"
    );
    expect(
      resolveWithApps({ configFile: absolute, env: requiredEnv }).config.instructionsPath
    ).toBe("/srv/legion/deployment.md");

    // Config-file value wins over env, matching every other setting's precedence
    // (`resolveValue`: cli > config > env > default).
    expect(
      resolveWithApps({
        configFile: relative,
        env: { ...requiredEnv, LEGION_INSTRUCTIONS: "/elsewhere/relative-looking.md" },
      }).config.instructionsPath
    ).toBe("/tmp/legion-config/deployment.md");

    // Env is never config-relative: there is no config file to be relative to.
    expect(
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_INSTRUCTIONS: "relative/deployment.md" },
        cliOverrides: {
          githubApps: BOTH_APPS,
        },
      }).config.instructionsPath
    ).toBe("relative/deployment.md");

    expect(
      resolveDaemonConfig({
        env: requiredEnv,
        cliOverrides: {
          githubApps: BOTH_APPS,
        },
      }).config.instructionsPath
    ).toBeUndefined();
  });

  it("rejects an empty instructions path from either source, naming the key", () => {
    expect(() =>
      loadConfigFromFile(["project: acme/7", "instructions: ''"].join("\n"), "/tmp/legion-config")
    ).toThrow("instructions must not be empty");
    expect(() =>
      loadConfigFromFile(["project: acme/7", "instructions: '  '"].join("\n"), "/tmp/legion-config")
    ).toThrow("instructions must not be empty");
    expect(() =>
      resolveDaemonConfig({
        env: { ...requiredEnv, LEGION_INSTRUCTIONS: "" },
        cliOverrides: {
          githubApps: BOTH_APPS,
        },
      })
    ).toThrow("LEGION_INSTRUCTIONS must not be empty");
  });

  describe("github_apps.<role>.private_key_secret", () => {
    const baseYaml = [
      "project: acme/7",
      "dispatch_project: ACME",
      "repos:",
      "  - acme/widgets",
      "nats_urls:",
      "  - nats://one:4222",
      "gates:",
      "  design: off",
    ];

    function secretYaml(name = "GH_AGENT_APP_PRIVATE_KEY_B64"): string {
      return [
        ...baseYaml,
        "github_apps:",
        "  implement:",
        '    app_id: "1"',
        `    private_key_secret: ${name}`,
        ...REVIEW_APP_YAML,
      ].join("\n");
    }

    /** A fake `secrets` first on PATH. `status` is what `get <NAME> --no-request` prints on
     * stdout; `value` is what `get <NAME> --value` prints; `statusExit`/`valueExit` and `stderr`
     * shape the failure cases. Every invocation appends one line to `<dir>/calls`:
     * `<argv>\t<readlink /proc/self/fd/0>\t<SECRETSD_SESSION_TOKEN_FILE or "unset">`. Like the
     * private_key_command leak test above, this mutates `process.env` (the resolver reads it for
     * its spawnSync call) and restores it in `finally`. */
    function withFakeSecrets(
      fake: {
        status?: string;
        statusExit?: number;
        value?: string;
        valueExit?: number;
        stderr?: string;
      },
      run: (calls: () => string[][]) => void
    ): void {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-fake-secrets-"));
      const callsFile = path.join(dir, "calls");
      // Outputs live in files the script `cat`s, so no fixture text is ever shell-quoted.
      fs.writeFileSync(path.join(dir, "status"), `${fake.status ?? ""}\n`);
      fs.writeFileSync(path.join(dir, "value"), `${fake.value ?? ""}\n`);
      fs.writeFileSync(
        path.join(dir, "stderr"),
        fake.stderr === undefined ? "" : `${fake.stderr}\n`
      );
      const script = [
        "#!/bin/sh",
        `printf '%s\\t%s\\t%s\\n' "$*" "$(readlink /proc/self/fd/0)" "\${SECRETSD_SESSION_TOKEN_FILE:-unset}" >> '${callsFile}'`,
        `cat '${path.join(dir, "stderr")}' >&2`,
        'case "$*" in',
        `  *--no-request) cat '${path.join(dir, "status")}'; exit ${fake.statusExit ?? 0} ;;`,
        `  *--value) cat '${path.join(dir, "value")}'; exit ${fake.valueExit ?? 0} ;;`,
        "esac",
        "exit 2",
      ].join("\n");
      fs.writeFileSync(path.join(dir, "secrets"), `${script}\n`, { mode: 0o755 });
      const savedPath = process.env.PATH;
      process.env.PATH = `${dir}${path.delimiter}${savedPath}`;
      try {
        run(() =>
          fs.existsSync(callsFile)
            ? fs
                .readFileSync(callsFile, "utf8")
                .trim()
                .split("\n")
                .map((line) => line.split("\t"))
            : []
        );
      } finally {
        process.env.PATH = savedPath;
        fs.rmSync(dir, { recursive: true, force: true });
      }
    }

    const FAKE_PEM =
      "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END RSA PRIVATE KEY-----\n";
    const FAKE_PEM_B64 = Buffer.from(FAKE_PEM).toString("base64");

    it("accepts the key as known and validates the name only under --check-config (resolveSecrets: false)", () => {
      withFakeSecrets(
        { status: '{"key":"GH_AGENT_APP_PRIVATE_KEY_B64","tier":"agent"}' },
        (calls) => {
          const file = loadConfigFromFile(secretYaml(), "/tmp/legion-config", {
            resolveSecrets: false,
          });
          const { config } = resolveDaemonConfig({ configFile: file });
          expect(config.githubApps.implement?.privateKey).toBe("(not executed)");
          expect(calls()).toEqual([]);
        }
      );
    });

    it("rejects a name with whitespace: a pasted command, not a secretsd key name", () => {
      expect(() =>
        loadConfigFromFile(
          secretYaml('"secrets get GH_AGENT_APP_PRIVATE_KEY_B64"'),
          "/tmp/legion-config",
          {
            resolveSecrets: false,
          }
        )
      ).toThrow(
        "github_apps.implement.private_key_secret must be a single secretsd key name (no whitespace)"
      );
    });

    it("rejects two private-key sources, naming all three", () => {
      expect(() =>
        loadConfigFromFile(
          [
            ...baseYaml,
            "github_apps:",
            "  implement:",
            '    app_id: "1"',
            '    private_key_command: "printf key"',
            "    private_key_secret: GH_AGENT_APP_PRIVATE_KEY_B64",
            ...REVIEW_APP_YAML,
          ].join("\n"),
          "/tmp/legion-config",
          { resolveSecrets: false }
        )
      ).toThrow(
        "github_apps.implement requires exactly one of private_key, private_key_command, or private_key_secret"
      );
    });

    it("requires both Apps: a section missing one role is refused naming the key, before any key command runs", () => {
      const marker = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "legion-config-")), "ran");
      expect(() =>
        loadConfigFromFile(
          [
            ...baseYaml,
            "github_apps:",
            "  implement:",
            '    app_id: "1"',
            `    private_key_command: "touch ${marker}; printf key"`,
          ].join("\n"),
          "/tmp/legion-config"
        )
      ).toThrow("github_apps.review is required");
      expect(fs.existsSync(marker)).toBeFalse();
      expect(() =>
        loadConfigFromFile(
          [...baseYaml, "github_apps:", ...REVIEW_APP_YAML].join("\n"),
          "/tmp/legion-config"
        )
      ).toThrow("github_apps.implement is required");
    });

    it("requires the github_apps section itself, from the file or a CLI override carrying both roles", () => {
      const file = loadConfigFromFile(baseYaml.join("\n"), "/tmp/legion-config");
      expect(() => resolveDaemonConfig({ configFile: file })).toThrow("github_apps is required");
      expect(() =>
        resolveWithApps({
          configFile: file,
          cliOverrides: { githubApps: { implement: BOTH_APPS.implement } },
        })
      ).toThrow("github_apps.review is required");
      expect(resolveWithApps({ configFile: file }).config.githubApps).toEqual(BOTH_APPS);
    });

    it("refuses an agent-tier key naming it, without ever fetching the value", () => {
      withFakeSecrets(
        { status: '{"key":"GH_AGENT_APP_PRIVATE_KEY_B64","tier":"agent"}' },
        (calls) => {
          expect(() => loadConfigFromFile(secretYaml(), "/tmp/legion-config")).toThrow(
            "App private key GH_AGENT_APP_PRIVATE_KEY_B64 is readable by agent-tier callers; move it to a daemon-only store"
          );
          expect(calls().map((row) => row[0])).toEqual([
            "get GH_AGENT_APP_PRIVATE_KEY_B64 --no-request",
          ]);
        }
      );
    });

    it("resolves a human-tier key to the decoded PEM: status before value, on the daemon's own stdin, without a session token", () => {
      const savedToken = process.env.SECRETSD_SESSION_TOKEN_FILE;
      process.env.SECRETSD_SESSION_TOKEN_FILE = "/leaked/agent-session-token";
      try {
        withFakeSecrets(
          {
            status: '{"key":"GH_AGENT_APP_PRIVATE_KEY_B64","tier":"human","grant":false}',
            value: FAKE_PEM_B64,
          },
          (calls) => {
            const file = loadConfigFromFile(secretYaml(), "/tmp/legion-config");
            const { config } = resolveDaemonConfig({ configFile: file });
            expect(config.githubApps.implement?.privateKey).toBe(FAKE_PEM.trim());
            const rows = calls();
            expect(rows.map((row) => row[0])).toEqual([
              "get GH_AGENT_APP_PRIVATE_KEY_B64 --no-request",
              "get GH_AGENT_APP_PRIVATE_KEY_B64 --value",
            ]);
            // secretsd finds a tokenless caller's terminal through isatty(stdin) plus
            // /proc/self/fd/0, so both children must run on the daemon's own fd 0, not a pipe.
            const ownStdin = fs.readlinkSync("/proc/self/fd/0");
            for (const row of rows) expect(row[1]).toBe(ownStdin);
            // The grant is the launcher pane's (tty scope), never an agent session's.
            for (const row of rows) expect(row[2]).toBe("unset");
          }
        );
      } finally {
        if (savedToken === undefined) delete process.env.SECRETSD_SESSION_TOKEN_FILE;
        else process.env.SECRETSD_SESSION_TOKEN_FILE = savedToken;
      }
    });

    it("refuses an unknown key with secretsd's own message, naming the setting and key", () => {
      withFakeSecrets({ statusExit: 1, stderr: "secrets: secret 'GH_NOPE' not found" }, () => {
        expect(() => loadConfigFromFile(secretYaml("GH_NOPE"), "/tmp/legion-config")).toThrow(
          "github_apps.implement.private_key_secret: secrets get GH_NOPE --no-request failed (exit 1): secrets: secret 'GH_NOPE' not found"
        );
      });
    });

    it("refuses an unparsable status naming the setting and key", () => {
      withFakeSecrets({ status: "not json" }, () => {
        expect(() => loadConfigFromFile(secretYaml(), "/tmp/legion-config")).toThrow(
          'github_apps.implement.private_key_secret: secrets get GH_AGENT_APP_PRIVATE_KEY_B64 --no-request printed an unparsable status (expected {"key","tier"})'
        );
      });
    });

    it("refuses a human-tier value that does not decode to a PEM, without printing the material", () => {
      const junk = Buffer.from("definitely-not-a-key").toString("base64");
      withFakeSecrets(
        {
          status: '{"key":"GH_AGENT_APP_PRIVATE_KEY_B64","tier":"human","grant":true}',
          value: junk,
        },
        () => {
          let message = "";
          try {
            loadConfigFromFile(secretYaml(), "/tmp/legion-config");
          } catch (error) {
            message = (error as Error).message;
          }
          expect(message).toBe(
            "github_apps.implement.private_key_secret: GH_AGENT_APP_PRIVATE_KEY_B64 did not decode to a PEM private key (expected base64 of a -----BEGIN block)"
          );
          expect(message).not.toContain(junk);
          expect(message).not.toContain("definitely-not-a-key");
        }
      );
    });

    it("refuses when the secrets command is not on PATH, naming the setting and key", () => {
      const empty = fs.mkdtempSync(path.join(os.tmpdir(), "legion-no-secrets-"));
      const savedPath = process.env.PATH;
      process.env.PATH = empty;
      try {
        expect(() => loadConfigFromFile(secretYaml(), "/tmp/legion-config")).toThrow(
          "github_apps.implement.private_key_secret: the secrets command is not on PATH, so GH_AGENT_APP_PRIVATE_KEY_B64 cannot be read"
        );
      } finally {
        process.env.PATH = savedPath;
        fs.rmSync(empty, { recursive: true, force: true });
      }
    });
  });
});
