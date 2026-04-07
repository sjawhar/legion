import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { loadRoutingConfig } from "../config";
import { ROUTING_CONFIG_PATH } from "../schema";

function createTempWorkspace(): string {
  return mkdtempSync(path.join(os.tmpdir(), "routing-config-test-"));
}

function writeConfig(workspace: string, content: string): void {
  const configDir = path.dirname(path.join(workspace, ROUTING_CONFIG_PATH));
  mkdirSync(configDir, { recursive: true });
  writeFileSync(path.join(workspace, ROUTING_CONFIG_PATH), content, "utf-8");
}

const tempDirs: string[] = [];

afterEach(() => {
  for (const dir of tempDirs) {
    rmSync(dir, { recursive: true, force: true });
  }
  tempDirs.length = 0;
});

describe("loadRoutingConfig", () => {
  it("returns null config with no warning when file is missing", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    const result = loadRoutingConfig(workspace);
    expect(result.config).toBeNull();
    expect(result.warning).toBeNull();
  });

  it("loads a valid YAML config", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    writeConfig(
      workspace,
      `
domains:
  - name: envoy
    paths:
      - "packages/envoy/**"
      - "packages/contracts/**"
    reviewers:
      - envoy-expert
  - name: daemon
    paths:
      - "packages/daemon/**"
    reviewers:
      - daemon-expert
`
    );

    const result = loadRoutingConfig(workspace);
    expect(result.config).not.toBeNull();
    expect(result.warning).toBeNull();
    expect(result.config?.domains).toHaveLength(2);
    expect(result.config?.domains[0].name).toBe("envoy");
    expect(result.config?.domains[0].paths).toEqual(["packages/envoy/**", "packages/contracts/**"]);
    expect(result.config?.domains[0].reviewers).toEqual(["envoy-expert"]);
  });

  it("returns warning for invalid YAML", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    writeConfig(workspace, "{{invalid yaml:::}}}");

    const result = loadRoutingConfig(workspace);
    expect(result.config).toBeNull();
    expect(result.warning).toContain("Invalid YAML");
  });

  it("returns warning for valid YAML but invalid schema", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    writeConfig(
      workspace,
      `
domains:
  - name: envoy
    paths: []
    reviewers:
      - alice
`
    );

    const result = loadRoutingConfig(workspace);
    expect(result.config).toBeNull();
    expect(result.warning).toContain("Invalid routing config");
  });

  it("returns warning when domains key is missing", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    writeConfig(workspace, "something_else: true");

    const result = loadRoutingConfig(workspace);
    expect(result.config).toBeNull();
    expect(result.warning).toContain("Invalid routing config");
  });

  it("returns warning for empty domains array", () => {
    const workspace = createTempWorkspace();
    tempDirs.push(workspace);

    writeConfig(workspace, "domains: []");

    const result = loadRoutingConfig(workspace);
    expect(result.config).toBeNull();
    expect(result.warning).toContain("Invalid routing config");
  });
});
