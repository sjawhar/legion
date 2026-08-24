import { afterEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { formatIssueKey } from "@legion/contracts";
import { provisionIssueWorkspace } from "./workspace";

type RunCall = {
  readonly cmd: string[];
  readonly opts:
    | {
        readonly cwd?: string;
        readonly env?: Readonly<Record<string, string>>;
      }
    | undefined;
};
function provisioningEnv(call: RunCall): Readonly<Record<string, string>> {
  const env = call.opts?.env;
  if (!env) throw new Error("Provisioning command did not receive an environment");
  expect(env).toMatchObject({
    GIT_TERMINAL_PROMPT: "0",
    LEGION_PROVISIONING_TOKEN: "installation-token",
  });
  expect(env.GIT_ASKPASS).toMatch(/provisioning-credential-.+\/askpass$/);
  expect(env.GIT_CONFIG_COUNT).toBeUndefined();
  return env;
}

const temporaryDirectories: string[] = [];
const originalMaxRecursionDepth = process.env.LEGION_MAX_RECURSION_DEPTH;
const extensionPackage = path.resolve(import.meta.dir, "../../envoy-omp-extension");

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
  test("clones a missing repository before fetching and provisioning its issue workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const bookmark = "legion/issue-42";
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "11";

    const spec = await provisionIssueWorkspace(issue, {
      extensionPackage,
      stateDir,
      provisioningToken: async () => "installation-token",
      run: async (cmd, opts) => {
        calls.push({ cmd, opts });
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
          await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
        }
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          await mkdir(workspaceDir, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    });

    const [clone, fetch] = calls;
    if (!clone || !fetch) throw new Error("Provisioning did not clone and fetch the repository");
    const cloneEnv = provisioningEnv(clone);
    expect(provisioningEnv(fetch)).toEqual(cloneEnv);
    expect(existsSync(cloneEnv.GIT_ASKPASS)).toBeFalse();
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "clone", "https://github.com/acme/widgets", repoCloneDir],
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ["git", `--git-dir=${repoCloneDir}/.jj/repo/store/git`, "worktree", "prune"],
      [
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
      ["jj", "bookmark", "set", bookmark],
      ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
    ]);
    expect(
      calls.flatMap(({ cmd }) => cmd).some((argument) => argument.startsWith("user."))
    ).toBeFalse();
    expect(await readFile(path.join(workspaceDir, ".omp", "config.yml"), "utf8")).toBe(
      `task:\n  maxRecursionDepth: 11\nextensions:\n  - ${extensionPackage}\n`
    );
    expect(spec).toEqual({ repoCloneDir, workspaceDir, bookmark });
  });

  test("does not add a second workspace when an issue is reactivated", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    const deps = {
      extensionPackage,
      stateDir,
      provisioningToken: async () => "installation-token",
      run: async (cmd: string[], opts?: RunCall["opts"]) => {
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
    const fetch = calls[0];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ["jj", "bookmark", "set", "legion/issue-42"],
      ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
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
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        extensionPackage,
        stateDir,
        provisioningToken: async () => "installation-token",
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

    const fetch = calls[0];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      [
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
      ["jj", "workspace", "forget", "issue-42", "--cleanup", "--force", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      [
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
      ["jj", "bookmark", "set", "legion/issue-42"],
      ["git", "-C", workspaceDir, "config", "credential.helper", "!legion credential"],
    ]);
  });

  test("rejects a partial repository clone before provisioning an issue workspace", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    await mkdir(repoCloneDir, { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        extensionPackage,
        stateDir,
        provisioningToken: async () => "installation-token",
        run: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      })
    ).rejects.toThrow(`Incomplete Jujutsu clone at ${repoCloneDir}`);
  });
});
