import { describe, expect, it } from "bun:test";
import path from "node:path";
import { loadConfig, loadConfigFromFile, resolveDaemonConfig } from "../config";

const requiredEnv = {
  LEGION_ID: "Acme/42",
  ENVOY_NATS_URL: "nats://one:4222, nats://two:4222",
  LEGION_REPOS: "acme/widgets",
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
        LEGION_WORKER_BUDGET: "9",
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
      boardProjectIds: ["PVT_alpha", "PVT_beta"],
      repos: ["acme/widgets"],
      appLogins: ["legion-implement[bot]", "legion-review[bot]"],
      maxFixAttempts: 5,
      admissionCap: 7,
      maxRecursionDepth: 11,
      lingerHours: 48,
      workerBudget: 9,
      resyncIntervalMs: 600_000,
      gates: { design: "root-issues", merge: "human" },
      ompInvocation: "custom-omp-from-env",
    });
    expect(config.stateDir).toEndWith(path.join(".legion", "acme42"));
  });

  it("resolves the optional dispatch MCP passthrough from the environment", () => {
    const { config } = resolveDaemonConfig({
      env: {
        ...requiredEnv,
        LEGION_BOARD_PROJECT_IDS: "PVT_x",
        DISPATCH_MCP_URL: "http://127.0.0.1:18766/mcp",
      },
      cliOverrides: {
        githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
      },
    });
    expect(config.dispatchMcpUrl).toBe("http://127.0.0.1:18766/mcp");
  });

  it("rejects an invalid dispatch MCP passthrough URL", () => {
    expect(() =>
      resolveDaemonConfig({
        env: {
          ...requiredEnv,
          LEGION_BOARD_PROJECT_IDS: "PVT_x",
          DISPATCH_MCP_URL: "not a url",
        },
        cliOverrides: {
          githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
        },
      })
    ).toThrow(/DISPATCH_MCP_URL/);
  });

  it("loads lifecycle settings from the existing YAML loader shape", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "port: 14001",
        "envoy_url: http://listener:9020",
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
        "worker_budget: 2",
        "resync_interval_seconds: 120",
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
      workerBudget: 2,
      resyncIntervalMs: 120_000,
      stateDir: "/tmp/legion-config/state",
      gates: { design: "off", merge: "off" },
      ompInvocation: "mise x github:acme/oh-my-pi@18.0.3 -- omp",
    });
  });

  it("loads dispatch_mcp_url from the YAML config without rejecting it as unknown", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "dispatch_mcp_url: http://127.0.0.1:18766/mcp",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({ env: requiredEnv, configFile: file });
    expect(config.dispatchMcpUrl).toBe("http://127.0.0.1:18766/mcp");
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
      loadConfigFromFile("project: acme/7\nworker_budget: 1.5\n", "/tmp/legion-config")
    ).toThrow("worker_budget");
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
