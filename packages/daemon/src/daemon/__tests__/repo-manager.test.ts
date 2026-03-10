import { describe, expect, it } from "bun:test";
import { resolveLegionPaths } from "../paths";
import {
  cleanupWorkspace,
  ensureRepoClone,
  ensureWorkspace,
  parseIssueRepo,
  type RepoManagerDeps,
  resolveWorkspacePath,
} from "../repo-manager";

describe("parseIssueRepo", () => {
  it("parses explicit owner/repo", () => {
    const result = parseIssueRepo("acme/widgets");
    expect(result).toEqual({ host: "github.com", owner: "acme", repo: "widgets" });
  });

  it("returns null for invalid repo string", () => {
    expect(parseIssueRepo("")).toBeNull();
    expect(parseIssueRepo("noslash")).toBeNull();
  });
});

describe("resolveWorkspacePath", () => {
  it("builds workspace path from paths + projectId + issueId", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    const result = resolveWorkspacePath(paths, "sjawhar/42", "acme-widgets-7");
    expect(result).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
  });

  it("throws when issueId would escape workspaces directory", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    expect(() => resolveWorkspacePath(paths, "sjawhar/42", "../../../etc")).toThrow(
      "Workspace path would escape workspaces directory"
    );
  });
});

describe("ensureRepoClone", () => {
  it("clones when directory does not exist", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => false,
      rmDir: async () => {},
    };

    const paths = resolveLegionPaths({}, "/home/test");
    await ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps);

    expect(commands[0]).toContain("git");
    expect(commands[0]).toContain("clone");
    expect(commands[0]).toContain("https://github.com/acme/widgets");
  });

  it("fetches when directory already exists", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    await ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps);

    expect(commands[0]).toContain("git");
    expect(commands[0]).toContain("fetch");
  });

  describe("characterization: ensureRepoClone", () => {
    it("returns clone path when directory exists and fetch succeeds", async () => {
      const commands: string[][] = [];
      const deps: RepoManagerDeps = {
        runJj: async (args) => {
          commands.push(args);
          return { exitCode: 0, stdout: "", stderr: "" };
        },
        exists: async () => true,
        rmDir: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      const clonePath = await ensureRepoClone(
        paths,
        { host: "github.com", owner: "acme", repo: "widgets" },
        deps
      );

      expect(clonePath).toBe("/home/test/.local/share/legion/repos/github.com/acme/widgets");
      expect(commands).toEqual([
        ["git", "fetch", "-R", "/home/test/.local/share/legion/repos/github.com/acme/widgets"],
      ]);
    });

    it("clones and returns clone path when directory does not exist", async () => {
      const commands: string[][] = [];
      const deps: RepoManagerDeps = {
        runJj: async (args) => {
          commands.push(args);
          return { exitCode: 0, stdout: "", stderr: "" };
        },
        exists: async () => false,
        rmDir: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      const clonePath = await ensureRepoClone(
        paths,
        { host: "github.com", owner: "acme", repo: "widgets" },
        deps
      );

      expect(clonePath).toBe("/home/test/.local/share/legion/repos/github.com/acme/widgets");
      expect(commands).toEqual([
        [
          "git",
          "clone",
          "https://github.com/acme/widgets",
          "/home/test/.local/share/legion/repos/github.com/acme/widgets",
        ],
      ]);
    });
  });

  describe("fetch failure propagation", () => {
    it("throws when jj git fetch fails", async () => {
      const deps: RepoManagerDeps = {
        runJj: async () => ({
          exitCode: 1,
          stdout: "",
          stderr: "fatal: could not read from remote",
        }),
        exists: async () => true,
        rmDir: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      await expect(
        ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps)
      ).rejects.toThrow("jj git fetch failed");
    });

    it("includes stderr in error message", async () => {
      const deps: RepoManagerDeps = {
        runJj: async () => ({ exitCode: 128, stdout: "", stderr: "Permission denied (publickey)" }),
        exists: async () => true,
        rmDir: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      await expect(
        ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps)
      ).rejects.toThrow("Permission denied (publickey)");
    });
  });
});

describe("ensureWorkspace", () => {
  it("creates jj workspace when it does not exist", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async (p) => p.includes("repos/"),
      rmDir: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    const wsPath = await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(wsPath).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeDefined();
    expect(wsCmd).toContain("add");
  });

  it("skips workspace creation when it already exists", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeUndefined();
  });
});

describe("cleanupWorkspace", () => {
  it("forgets jj workspace and removes directory", async () => {
    const commands: string[][] = [];
    const removedPaths: string[] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (p) => {
        removedPaths.push(p);
      },
    };

    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await cleanupWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    const forgetCmd = commands.find((c) => c.includes("forget"));
    expect(forgetCmd).toBeDefined();
    expect(forgetCmd).toContain("acme-widgets-7");
    expect(removedPaths).toContain(
      "/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7"
    );
  });
});
