import { describe, expect, it } from "bun:test";
import path from "node:path";
import { loadConfig, loadConfigFromFile, resolveDaemonConfig } from "../config";

const requiredEnv = {
  LEGION_ID: "Acme/42",
  ENVOY_NATS_URL: "nats://one:4222, nats://two:4222",
  LEGION_DISPATCH_URL: "http://127.0.0.1:13380",
};

describe("daemon config", () => {
  it("derives the typed lifecycle config from environment", () => {
    const config = loadConfig({
      ...requiredEnv,
      LEGION_DAEMON_PORT: "14000",
      LEGION_BOARD_PROJECT_IDS: "PVT_alpha,PVT_beta",
      LEGION_APP_LOGINS: "legion-implement[bot],legion-review[bot]",
      LEGION_MAX_FIX_ATTEMPTS: "5",
      LEGION_ADMISSION_CAP: "7",
      LEGION_MAX_RECURSION_DEPTH: "11",
      LEGION_LINGER_HOURS: "48",
      LEGION_WORKER_BUDGET: "9",
    });

    expect(config).toMatchObject({
      project: "acme42",
      legionId: "Acme/42",
      port: 14000,
      envoyUrl: "http://127.0.0.1:9020",
      natsUrls: ["nats://one:4222", "nats://two:4222"],
      dispatchUrl: "http://127.0.0.1:13380",
      boardProjectIds: ["PVT_alpha", "PVT_beta"],
      appLogins: ["legion-implement[bot]", "legion-review[bot]"],
      maxFixAttempts: 5,
      admissionCap: 7,
      maxRecursionDepth: 11,
      lingerHours: 48,
      workerBudget: 9,
      ciQuietMs: 30_000,
      resyncIntervalMs: 600_000,
      gates: { design: "root-issues", merge: "human" },
    });
    expect(config.stateDir).toEndWith(path.join(".legion", "acme42"));
  });

  it("loads lifecycle settings from the existing YAML loader shape", () => {
    const file = loadConfigFromFile(
      [
        "project: acme/7",
        "port: 14001",
        "envoy_url: http://listener:9020",
        "nats_urls:",
        "  - nats://one:4222",
        "dispatch_url: http://dispatch:13380",
        "board_project_ids:",
        "  - PVT_one",
        "app_logins:",
        "  - legion-implement[bot]",
        "max_fix_attempts: 4",
        "admission_cap: 3",
        "max_recursion_depth: 6",
        "linger_hours: 24",
        "worker_budget: 2",
        "ci_quiet_ms: 45000",
        "resync_interval_seconds: 120",
        "state_dir: ./state",
        "gates:",
        "  design: off",
        "  merge: off",
      ].join("\n"),
      "/tmp/legion-config"
    );
    const { config } = resolveDaemonConfig({ configFile: file, env: {} });

    expect(config).toMatchObject({
      project: "acme7",
      legionId: "acme/7",
      port: 14001,
      envoyUrl: "http://listener:9020",
      natsUrls: ["nats://one:4222"],
      dispatchUrl: "http://dispatch:13380",
      boardProjectIds: ["PVT_one"],
      appLogins: ["legion-implement[bot]"],
      maxFixAttempts: 4,
      admissionCap: 3,
      maxRecursionDepth: 6,
      lingerHours: 24,
      workerBudget: 2,
      ciQuietMs: 45_000,
      resyncIntervalMs: 120_000,
      stateDir: "/tmp/legion-config/state",
      gates: { design: "off", merge: "off" },
    });
    expect(file.warnings).toEqual([]);
  });

  it("rejects missing NATS or dispatch configuration instead of inventing a transport", () => {
    expect(() =>
      loadConfig({ LEGION_ID: "acme/7", LEGION_DISPATCH_URL: "http://dispatch" })
    ).toThrow("ENVOY_NATS_URL");
    expect(() => loadConfig({ LEGION_ID: "acme/7", ENVOY_NATS_URL: "nats://one:4222" })).toThrow(
      "LEGION_DISPATCH_URL"
    );
  });

  it("rejects invalid lifecycle numbers from either configuration source", () => {
    expect(() => loadConfig({ ...requiredEnv, LEGION_ADMISSION_CAP: "-1" })).toThrow(
      "LEGION_ADMISSION_CAP"
    );
    expect(() =>
      loadConfigFromFile("project: acme/7\nworker_budget: 1.5\n", "/tmp/legion-config")
    ).toThrow("worker_budget");
  });
});
