import { beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { EnvoyConfigError, loadEnvoyConfig } from "..";

describe("loadEnvoyConfig", () => {
  let homeDir: string;
  let repoDir: string;

  beforeEach(async () => {
    homeDir = await mkdtemp(path.join(tmpdir(), "envoy-home-"));
    repoDir = await mkdtemp(path.join(tmpdir(), "envoy-repo-"));
  });

  async function writeJson(filePath: string, value: unknown) {
    await mkdir(path.dirname(filePath), { recursive: true });
    await writeFile(filePath, JSON.stringify(value));
  }

  it("loads user-only config", async () => {
    await writeJson(path.join(homeDir, ".config", "opencode", "envoy.json"), {
      natsUrls: ["nats://127.0.0.1:4222"],
      dispatch: { enabled: true },
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({
      natsUrls: ["nats://127.0.0.1:4222"],
      dispatch: { enabled: true },
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
      dispatch: { enabled: false },
    });
    await writeJson(path.join(repoDir, ".opencode", "envoy.json"), {
      dispatch: { enabled: true, serverUrl: "http://localhost:8766" },
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({
      natsUrls: ["nats://user:4222"],
      dispatch: {
        enabled: true,
        serverUrl: "http://localhost:8766",
      },
    });
  });

  it("rejects removed dispatch keys (defaultRepo, appClientId) naming the file and both keys", async () => {
    const configPath = path.join(repoDir, ".opencode", "envoy.json");
    await writeJson(configPath, {
      dispatch: { enabled: true, defaultRepo: "sjawhar/legion", appClientId: "abc123" },
    });

    const error = await loadEnvoyConfig(repoDir, { homeDir }).then(
      () => null,
      (thrown: unknown) => thrown
    );
    expect(error).toBeInstanceOf(EnvoyConfigError);
    const message = (error as Error).message;
    expect(message).toContain(configPath);
    expect(message).toContain("defaultRepo");
    expect(message).toContain("appClientId");
  });

  it("rejects invalid JSON naming the file", async () => {
    const configPath = path.join(homeDir, ".config", "opencode", "envoy.json");
    await mkdir(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "{");

    await expect(loadEnvoyConfig(repoDir, { homeDir })).rejects.toThrow(configPath);
  });

  it("rejects schema-invalid JSON", async () => {
    await writeJson(path.join(repoDir, ".opencode", "envoy.json"), {
      natsUrls: "nats://127.0.0.1:4222",
    });

    await expect(loadEnvoyConfig(repoDir, { homeDir })).rejects.toThrow(EnvoyConfigError);
  });

  it("returns empty config when files are missing", async () => {
    await expect(loadEnvoyConfig(repoDir, { homeDir })).resolves.toEqual({});
  });
});
