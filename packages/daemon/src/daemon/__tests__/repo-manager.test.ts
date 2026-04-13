import { describe, expect, it } from "bun:test";
import { resolveLegionPaths } from "../paths";
import {
  cleanupWorkspace,
  ensureRepoClone,
  ensureWorkspace,
  parseIssueRepo,
  type RepoManagerDeps,
  resolveWorkspacePath,
  startBackgroundFetch,
  verifyBranchPushed,
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
      symlink: async () => {},
    };

    const paths = resolveLegionPaths({}, "/home/test");
    await ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps);

    expect(commands[0]).toContain("git");
    expect(commands[0]).toContain("clone");
    expect(commands[0]).toContain("https://github.com/acme/widgets");
  });

  it("skips fetch when directory already exists", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const clonePath = await ensureRepoClone(
      paths,
      { host: "github.com", owner: "acme", repo: "widgets" },
      deps
    );

    expect(clonePath).toBe("/home/test/.local/share/legion/repos/github.com/acme/widgets");
    expect(commands).toEqual([]);
  });

  describe("characterization: ensureRepoClone", () => {
    it("returns clone path without fetching when directory exists", async () => {
      const commands: string[][] = [];
      const deps: RepoManagerDeps = {
        runJj: async (args) => {
          commands.push(args);
          return { exitCode: 0, stdout: "", stderr: "" };
        },
        exists: async () => true,
        rmDir: async () => {},
        symlink: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      const clonePath = await ensureRepoClone(
        paths,
        { host: "github.com", owner: "acme", repo: "widgets" },
        deps
      );

      expect(clonePath).toBe("/home/test/.local/share/legion/repos/github.com/acme/widgets");
      expect(commands).toEqual([]);
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
        symlink: async () => {},
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

  describe("clone failure propagation", () => {
    it("throws when jj git clone fails", async () => {
      const deps: RepoManagerDeps = {
        runJj: async () => ({
          exitCode: 1,
          stdout: "",
          stderr: "fatal: could not read from remote",
        }),
        exists: async () => false,
        rmDir: async () => {},
        symlink: async () => {},
      };
      const paths = resolveLegionPaths({}, "/home/test");

      await expect(
        ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps)
      ).rejects.toThrow("Failed to clone");
    });

    it("includes stderr in error message", async () => {
      const deps: RepoManagerDeps = {
        runJj: async () => ({
          exitCode: 128,
          stdout: "",
          stderr: "Permission denied (publickey)",
        }),
        exists: async () => false,
        rmDir: async () => {},
        symlink: async () => {},
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
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    const wsPath = await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(wsPath).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeDefined();
    expect(wsCmd).toContain("add");
    expect(wsCmd).toContain("--revision");
    expect(wsCmd).toContain("main");
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
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeUndefined();
  });

  it("creates .opencode symlink when .claude exists", async () => {
    const symlinkCalls: { target: string; linkPath: string }[] = [];
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      exists: async (p) => {
        if (p.includes("repos/")) return true;
        if (p.endsWith(".claude")) return true;
        if (p.endsWith(".opencode")) return false;
        return false;
      },
      rmDir: async () => {},
      symlink: async (target, linkPath) => {
        symlinkCalls.push({ target, linkPath });
      },
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(symlinkCalls).toHaveLength(1);
    expect(symlinkCalls[0].target).toBe(".claude");
    expect(symlinkCalls[0].linkPath).toEndWith(".opencode");
  });

  it("skips .opencode symlink when .claude does not exist", async () => {
    const symlinkCalls: { target: string; linkPath: string }[] = [];
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      exists: async (p) => {
        if (p.includes("repos/")) return true;
        return false;
      },
      rmDir: async () => {},
      symlink: async (target, linkPath) => {
        symlinkCalls.push({ target, linkPath });
      },
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(symlinkCalls).toHaveLength(0);
  });

  it("skips .opencode symlink when .opencode already exists", async () => {
    const symlinkCalls: { target: string; linkPath: string }[] = [];
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      exists: async (p) => {
        if (p.includes("repos/")) return true;
        if (p.endsWith(".claude")) return true;
        if (p.endsWith(".opencode")) return true;
        return false;
      },
      rmDir: async () => {},
      symlink: async (target, linkPath) => {
        symlinkCalls.push({ target, linkPath });
      },
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(symlinkCalls).toHaveLength(0);
  });

  it("does not fail when symlink creation throws", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      exists: async (p) => {
        if (p.includes("repos/")) return true;
        if (p.endsWith(".claude")) return true;
        if (p.endsWith(".opencode")) return false;
        return false;
      },
      rmDir: async () => {},
      symlink: async () => {
        throw new Error("EACCES: permission denied");
      },
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    // Should not throw despite symlink failure
    const wsPath = await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);
    expect(wsPath).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
  });
});

describe("verifyBranchPushed", () => {
  it("returns safe when no bookmark exists", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result).toEqual({ safe: true });
  });

  it("returns safe when jj command fails", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 1, stdout: "", stderr: "error" }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result).toEqual({ safe: true });
  });

  it("returns safe when bookmark is synced with origin (ahead:0)", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({
        exitCode: 0,
        stdout: [
          "acme-widgets-7 local",
          "acme-widgets-7 remote:git ahead:0",
          "acme-widgets-7 remote:origin ahead:0",
        ].join("\n"),
        stderr: "",
      }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result).toEqual({ safe: true });
  });

  it("returns unsafe when bookmark is ahead of origin", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({
        exitCode: 0,
        stdout: [
          "acme-widgets-7 local",
          "acme-widgets-7 remote:git ahead:0",
          "acme-widgets-7 remote:origin ahead:3",
        ].join("\n"),
        stderr: "",
      }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result.safe).toBe(false);
    expect(result.reason).toContain("3 commit(s) ahead of origin");
  });

  it("returns unsafe when bookmark has no origin remote tracking", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({
        exitCode: 0,
        stdout: ["acme-widgets-7 local", "acme-widgets-7 remote:git ahead:0"].join("\n"),
        stderr: "",
      }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result.safe).toBe(false);
    expect(result.reason).toContain("no remote tracking on origin");
  });

  it("returns safe when only remote entries exist (no local bookmark)", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({
        exitCode: 0,
        stdout: ["acme-widgets-7 remote:git ahead:0", "acme-widgets-7 remote:origin ahead:0"].join(
          "\n"
        ),
        stderr: "",
      }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    const result = await verifyBranchPushed("/clone", "acme-widgets-7", deps);
    expect(result).toEqual({ safe: true });
  });

  it("passes correct arguments to runJj", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };

    await verifyBranchPushed("/my/clone", "ACME-Widgets-7", deps);

    expect(commands).toHaveLength(1);
    expect(commands[0]).toContain("bookmark");
    expect(commands[0]).toContain("list");
    expect(commands[0]).toContain("acme-widgets-7"); // lowercased
    expect(commands[0]).toContain("--all");
    expect(commands[0]).toContain("-R");
    expect(commands[0]).toContain("/my/clone");
  });
});

describe("cleanupWorkspace", () => {
  it("forgets jj workspace and removes directory when branch is pushed", async () => {
    const commands: string[][] = [];
    const removedPaths: string[] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        // bookmark list returns synced bookmark
        if (args.includes("bookmark")) {
          return {
            exitCode: 0,
            stdout: ["acme-widgets-7 local", "acme-widgets-7 remote:origin ahead:0"].join("\n"),
            stderr: "",
          };
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (p) => {
        removedPaths.push(p);
      },
      symlink: async () => {},
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

  it("refuses to clean when branch has unpushed commits", async () => {
    const removedPaths: string[] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        if (args.includes("bookmark")) {
          return {
            exitCode: 0,
            stdout: ["acme-widgets-7 local", "acme-widgets-7 remote:origin ahead:5"].join("\n"),
            stderr: "",
          };
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (p) => {
        removedPaths.push(p);
      },
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };

    await expect(
      cleanupWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps)
    ).rejects.toThrow("Refusing to clean workspace");
    expect(removedPaths).toEqual([]);
  });

  it("refuses to clean when bookmark has no origin tracking", async () => {
    const removedPaths: string[] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        if (args.includes("bookmark")) {
          return {
            exitCode: 0,
            stdout: "acme-widgets-7 local\n",
            stderr: "",
          };
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (p) => {
        removedPaths.push(p);
      },
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };

    await expect(
      cleanupWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps)
    ).rejects.toThrow("Refusing to clean workspace");
    expect(removedPaths).toEqual([]);
  });

  it("allows cleanup when no bookmark exists (no work done)", async () => {
    const commands: string[][] = [];
    const removedPaths: string[] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        if (args.includes("bookmark")) {
          return { exitCode: 0, stdout: "", stderr: "" };
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (p) => {
        removedPaths.push(p);
      },
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await cleanupWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    const forgetCmd = commands.find((c) => c.includes("forget"));
    expect(forgetCmd).toBeDefined();
    expect(removedPaths).toHaveLength(1);
  });
});

describe("startBackgroundFetch", () => {
  it("fires jj git fetch with the correct clone path", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => {
        commands.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");

    const result = await startBackgroundFetch(
      paths,
      { host: "github.com", owner: "acme", repo: "widgets" },
      deps
    );

    expect(result.exitCode).toBe(0);
    expect(commands).toEqual([
      ["git", "fetch", "-R", "/home/test/.local/share/legion/repos/github.com/acme/widgets"],
    ]);
  });

  it("rejects on non-zero exit code so .catch() fires", async () => {
    const deps: RepoManagerDeps = {
      runJj: async () => ({
        exitCode: 128,
        stdout: "",
        stderr: "Permission denied (publickey)",
      }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };
    const paths = resolveLegionPaths({}, "/home/test");

    await expect(
      startBackgroundFetch(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps)
    ).rejects.toThrow("Permission denied");
  });
});
