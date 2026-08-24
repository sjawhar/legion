import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { formatIssueKey } from "@legion/contracts";
import { provisionIssueWorkspace } from "./workspace";

type RunCall = {
  readonly cmd: string[];
  readonly opts: { readonly cwd?: string } | undefined;
};

const temporaryDirectories: string[] = [];
const originalMaxRecursionDepth = process.env.LEGION_MAX_RECURSION_DEPTH;

afterEach(async () => {
  if (originalMaxRecursionDepth === undefined) delete process.env.LEGION_MAX_RECURSION_DEPTH;
  else process.env.LEGION_MAX_RECURSION_DEPTH = originalMaxRecursionDepth;

  await Promise.all(
    temporaryDirectories
      .splice(0)
      .map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

async function temporaryDirectory(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-workspace-"));
  temporaryDirectories.push(directory);
  return directory;
}

describe("provisionIssueWorkspace", () => {
  test("creates a fetched issue workspace with its branch, credential helper, and OMP configuration", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const bookmark = "legion/issue-42";
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "11";
    await mkdir(repoCloneDir, { recursive: true });

    const spec = await provisionIssueWorkspace(issue, {
      daemonUrl: "http://127.0.0.1:13370",
      stateDir,
      run: async (cmd, opts) => {
        calls.push({ cmd, opts });
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          await mkdir(workspaceDir, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    });

    expect(spec).toEqual({ repoCloneDir, workspaceDir, bookmark });
    expect(calls).toEqual([
      { cmd: ["jj", "git", "fetch", "-R", repoCloneDir], opts: undefined },
      {
        cmd: ["git", `--git-dir=${repoCloneDir}/.jj/repo/store/git`, "worktree", "prune"],
        opts: undefined,
      },
      {
        cmd: [
          "jj",
          "workspace",
          "add",
          workspaceDir,
          "--name",
          "issue-42",
          "--revision",
          "main",
          "-R",
          repoCloneDir,
        ],
        opts: undefined,
      },
      { cmd: ["jj", "bookmark", "set", bookmark], opts: { cwd: workspaceDir } },
      {
        cmd: ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
        opts: undefined,
      },
    ]);
    expect(
      calls.flatMap(({ cmd }) => cmd).some((argument) => argument.startsWith("user."))
    ).toBeFalse();
    expect(await readFile(path.join(workspaceDir, ".omp", "config.yml"), "utf8")).toBe(
      `task:\n  maxRecursionDepth: 11\nextensions:\n  - ${path.resolve(import.meta.dir, "../..")}\n`
    );
  });

  test("does not add a second workspace when an issue is reactivated", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(repoCloneDir, { recursive: true });

    const deps = {
      daemonUrl: "http://127.0.0.1:13370",
      stateDir,
      run: async (cmd: string[], opts?: { cwd?: string }) => {
        calls.push({ cmd, opts });
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          await mkdir(workspaceDir, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    };

    await provisionIssueWorkspace(issue, deps);
    calls.length = 0;

    await expect(provisionIssueWorkspace(issue, deps)).resolves.toEqual({
      repoCloneDir,
      workspaceDir,
      bookmark: "legion/issue-42",
    });
    expect(calls).toEqual([
      { cmd: ["jj", "bookmark", "set", "legion/issue-42"], opts: { cwd: workspaceDir } },
      {
        cmd: ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
        opts: undefined,
      },
    ]);
  });
  test("recovers a jj registration for a deleted issue workspace", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const gitDir = path.join(repoCloneDir, ".jj", "repo", "store", "git");
    const calls: RunCall[] = [];
    let workspaceAddAttempts = 0;
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        daemonUrl: "http://127.0.0.1:13370",
        stateDir,
        run: async (cmd, opts) => {
          calls.push({ cmd, opts });
          if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
            workspaceAddAttempts += 1;
            if (workspaceAddAttempts === 1) {
              return {
                exitCode: 1,
                stdout: "",
                stderr: "Workspace named 'issue-42' already exists",
              };
            }
            await mkdir(workspaceDir, { recursive: true });
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      })
    ).resolves.toEqual({
      repoCloneDir,
      workspaceDir,
      bookmark: "legion/issue-42",
    });

    expect(calls).toEqual([
      { cmd: ["jj", "git", "fetch", "-R", repoCloneDir], opts: undefined },
      { cmd: ["git", `--git-dir=${gitDir}`, "worktree", "prune"], opts: undefined },
      {
        cmd: [
          "jj",
          "workspace",
          "add",
          workspaceDir,
          "--name",
          "issue-42",
          "--revision",
          "main",
          "-R",
          repoCloneDir,
        ],
        opts: undefined,
      },
      {
        cmd: ["jj", "workspace", "forget", "issue-42", "--cleanup", "--force", "-R", repoCloneDir],
        opts: undefined,
      },
      { cmd: ["git", `--git-dir=${gitDir}`, "worktree", "prune"], opts: undefined },
      {
        cmd: [
          "jj",
          "workspace",
          "add",
          workspaceDir,
          "--name",
          "issue-42",
          "--revision",
          "legion/issue-42",
          "-R",
          repoCloneDir,
        ],
        opts: undefined,
      },
      { cmd: ["jj", "bookmark", "set", "legion/issue-42"], opts: { cwd: workspaceDir } },
      {
        cmd: ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
        opts: undefined,
      },
    ]);
  });
});
