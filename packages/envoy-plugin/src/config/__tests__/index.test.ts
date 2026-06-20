import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { logger } from "../../log";
import { loadEnvoyConfig } from "..";

describe("loadEnvoyConfig", () => {
  let homeDir: string;
  let repoDir: string;
  let warn: ReturnType<typeof spyOn>;

  beforeEach(async () => {
    homeDir = await mkdtemp(path.join(tmpdir(), "envoy-home-"));
    repoDir = await mkdtemp(path.join(tmpdir(), "envoy-repo-"));
    // Spy on the file logger — plugin diagnostics route there instead of
    // console to keep stderr clean for the in-process TUI host.
    warn = spyOn(logger, "warn").mockImplementation(() => {});
  });

  afterEach(() => {
    warn.mockRestore();
  });

  async function writeJson(filePath: string, value: unknown) {
    await mkdir(path.dirname(filePath), { recursive: true });
    await writeFile(filePath, JSON.stringify(value));
  }

  it("loads user-only config", async () => {
    await writeJson(path.join(homeDir, ".config", "opencode", "envoy.json"), {
      natsUrls: ["nats://127.0.0.1:4222"],
      dispatch: { enabled: true, defaultRepo: "sjawhar/legion" },
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({
      natsUrls: ["nats://127.0.0.1:4222"],
      dispatch: { enabled: true, defaultRepo: "sjawhar/legion" },
    });
  });

  it("loads repo-only config", async () => {
    await writeJson(path.join(repoDir, ".opencode", "envoy.json"), {
      dispatch: { serverUrl: "http://localhost:8766" },
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({
      dispatch: { serverUrl: "http://localhost:8766" },
    });
  });

  it("shallow-merges user config with repo config and lets repo dispatch keys win", async () => {
    await writeJson(path.join(homeDir, ".config", "opencode", "envoy.json"), {
      natsUrls: ["nats://user:4222"],
      dispatch: { enabled: false, defaultRepo: "sjawhar/legion" },
    });
    await writeJson(path.join(repoDir, ".opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "http://localhost:8766" },
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({
      natsUrls: ["nats://user:4222"],
      dispatch: {
        enabled: true,
        defaultRepo: "sjawhar/legion",
        serverUrl: "http://localhost:8766",
      },
    });
  });

  it("returns empty config and warns on invalid JSON", async () => {
    const configPath = path.join(homeDir, ".config", "opencode", "envoy.json");
    await mkdir(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "{");

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({});
    expect(warn).toHaveBeenCalled();
  });

  it("returns empty config and warns on schema-invalid JSON", async () => {
    await writeJson(path.join(repoDir, ".opencode", "envoy.json"), {
      natsUrls: "nats://127.0.0.1:4222",
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({});
    expect(warn).toHaveBeenCalled();
  });

  it("returns empty config when files are missing", async () => {
    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({});
    expect(warn).not.toHaveBeenCalled();
  });
});
