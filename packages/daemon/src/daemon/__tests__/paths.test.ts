import { describe, expect, it } from "bun:test";
import { resolveLegionPaths } from "../paths";

describe("resolveLegionPaths", () => {
  it("uses XDG defaults when env vars unset", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    expect(paths.dataDir).toBe("/home/testuser/.local/share/legion");
    expect(paths.stateDir).toBe("/home/testuser/.local/state/legion");
    expect(paths.reposDir).toBe("/home/testuser/.local/share/legion/repos");
    expect(paths.workspacesDir).toBe("/home/testuser/.local/share/legion/workspaces");
    expect(paths.legionsFile).toBe("/home/testuser/.local/state/legion/legions.json");
  });

  it("respects XDG_DATA_HOME", () => {
    const paths = resolveLegionPaths({ XDG_DATA_HOME: "/custom/data" }, "/home/testuser");
    expect(paths.dataDir).toBe("/custom/data/legion");
    expect(paths.reposDir).toBe("/custom/data/legion/repos");
  });

  it("respects XDG_STATE_HOME", () => {
    const paths = resolveLegionPaths({ XDG_STATE_HOME: "/custom/state" }, "/home/testuser");
    expect(paths.stateDir).toBe("/custom/state/legion");
    expect(paths.legionsFile).toBe("/custom/state/legion/legions.json");
  });

  it("falls back to default when XDG_DATA_HOME is empty string", () => {
    const paths = resolveLegionPaths({ XDG_DATA_HOME: "" }, "/home/testuser");
    expect(paths.dataDir).toBe("/home/testuser/.local/share/legion");
    expect(paths.reposDir).toBe("/home/testuser/.local/share/legion/repos");
  });

  it("falls back to default when XDG_STATE_HOME is relative path", () => {
    const paths = resolveLegionPaths({ XDG_STATE_HOME: "relative/path" }, "/home/testuser");
    expect(paths.stateDir).toBe("/home/testuser/.local/state/legion");
    expect(paths.legionsFile).toBe("/home/testuser/.local/state/legion/legions.json");
  });

  it("computes legion-specific paths", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    const legion = paths.forLegion("sjawhar/42");
    expect(legion.legionStateDir).toBe("/home/testuser/.local/state/legion/legions/sjawhar/42");
    expect(legion.workersFile).toBe(
      "/home/testuser/.local/state/legion/legions/sjawhar/42/workers.json"
    );
    expect(legion.logDir).toBe("/home/testuser/.local/state/legion/legions/sjawhar/42/logs");
    expect(legion.workspacesDir).toBe("/home/testuser/.local/share/legion/workspaces/sjawhar/42");
  });

  it("computes repo clone path", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    expect(paths.repoClonePath("github.com", "acme", "widgets")).toBe(
      "/home/testuser/.local/share/legion/repos/github.com/acme/widgets"
    );
  });
});

describe("path validation", () => {
  it("throws when homeDir is empty string", () => {
    expect(() => resolveLegionPaths({}, "")).toThrow("homeDir must be an absolute path, got: ");
  });

  it("throws when homeDir is relative path", () => {
    expect(() => resolveLegionPaths({}, "relative/path")).toThrow(
      "homeDir must be an absolute path, got: relative/path"
    );
  });

  it("succeeds when homeDir is valid absolute path", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    expect(paths.dataDir).toBe("/home/test/.local/share/legion");
  });

  it("throws when repoClonePath would escape repos directory", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    expect(() => paths.repoClonePath("github.com", "../../..", "etc")).toThrow(
      "Repo path would escape repos directory"
    );
  });

  it("succeeds when repoClonePath is valid", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    const result = paths.repoClonePath("github.com", "sjawhar", "42");
    expect(result).toBe("/home/test/.local/share/legion/repos/github.com/sjawhar/42");
  });

  it("throws when forLegion would escape legions directory", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    expect(() => paths.forLegion("../../../etc")).toThrow(
      "Project path would escape legions directory"
    );
  });

  it("succeeds when forLegion is valid", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    const legion = paths.forLegion("sjawhar/42");
    expect(legion.legionStateDir).toBe("/home/test/.local/state/legion/legions/sjawhar/42");
  });
});
