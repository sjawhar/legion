import { afterEach, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { formatIssueKey } from "@legion/contracts";
import { provisionIssueWorkspace, type RunResult } from "./workspace";

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

const STOCK_JJ = ["mise", "x", "github:jj-vcs/jj@0.44.0", "--", "jj"];
const credentialHelper =
  "!/opt/legion/bin/bun /opt/legion/packages/daemon/src/cli/index.ts credential";
const SYSTEM_GIT = "/usr/bin/git";

async function runCommand(command: string[], options?: RunCall["opts"]): Promise<RunResult> {
  const child = Bun.spawn(command, {
    cwd: options?.cwd,
    env: options?.env ? { ...process.env, ...options.env } : undefined,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { exitCode, stdout, stderr };
}
async function fillCredential(
  gitDir: string,
  env: Readonly<Record<string, string>>
): Promise<RunResult> {
  const child = Bun.spawn(
    [
      "sh",
      "-c",
      `printf 'protocol=https\\nhost=github.com\\n\\n' | ${SYSTEM_GIT} --git-dir="$1" credential fill`,
      "sh",
      gitDir,
    ],
    {
      env: { ...process.env, ...env },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { exitCode, stdout, stderr };
}
function credentialConfigCommands(gitDir: string, helper: string): string[][] {
  return [
    ["git", `--git-dir=${gitDir}`, "config", "--replace-all", "credential.helper", ""],
    ["git", `--git-dir=${gitDir}`, "config", "--add", "credential.helper", helper],
    [
      "git",
      `--git-dir=${gitDir}`,
      "config",
      "--replace-all",
      "credential.https://github.com.helper",
      "",
    ],
    [
      "git",
      `--git-dir=${gitDir}`,
      "config",
      "--add",
      "credential.https://github.com.helper",
      helper,
    ],
    ["git", `--git-dir=${gitDir}`, "config", "credential.interactive", "false"],
  ];
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
      credentialHelper,
      run: async (cmd, opts) => {
        calls.push({ cmd, opts });
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
          expect(existsSync(path.dirname(repoCloneDir))).toBeTrue();
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
      ["git", `--git-dir=${repoCloneDir}/.git`, "worktree", "prune"],
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
      ["jj", "bookmark", "set", bookmark, "--allow-backwards"],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
    ]);
    expect(
      calls.flatMap(({ cmd }) => cmd).some((argument) => argument.startsWith("user."))
    ).toBeFalse();
    expect(await readFile(path.join(workspaceDir, ".omp", "config.yml"), "utf8")).toBe(
      `task:\n  maxRecursionDepth: 11\nextensions:\n  - ${extensionPackage}\n`
    );
    expect(spec).toEqual({ repoCloneDir, workspaceDir, bookmark });
  });

  test("creates a missing workspace parent before adding an issue workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      run: async (cmd) => {
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          expect(existsSync(path.dirname(workspaceDir))).toBeTrue();
          await mkdir(workspaceDir, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    });
  });
  test("configures the backing Git repository for a stock JJ workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const gitDir = path.join(repoCloneDir, ".git");
    await mkdir(path.dirname(repoCloneDir), { recursive: true });
    expect(
      (await runCommand([...STOCK_JJ, "git", "init", "--colocate", repoCloneDir])).exitCode
    ).toBe(0);
    expect(
      (await runCommand([...STOCK_JJ, "bookmark", "set", "main", "-R", repoCloneDir])).exitCode
    ).toBe(0);
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      run: async (cmd) => {
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch") {
          return { exitCode: 0, stdout: "", stderr: "" };
        }
        return runCommand(cmd[0] === "jj" ? [...STOCK_JJ, ...cmd.slice(1)] : cmd);
      },
    });

    expect(existsSync(path.join(workspaceDir, ".git"))).toBeFalse();
    const credential = await runCommand([
      "git",
      `--git-dir=${gitDir}`,
      "config",
      "--get",
      "credential.helper",
    ]);
    expect(credential.exitCode).toBe(0);
    expect(credential.stdout.trim()).toBe(credentialHelper);
  });
  test("runs the supplied pinned credential helper instead of a PATH Legion and fails loudly", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const helperDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const gitDir = path.join(repoCloneDir, ".git");
    const runtime = path.join(helperDir, "bun");
    const cli = path.join(helperDir, "legion-cli.ts");
    const decoy = path.join(helperDir, "legion");
    const decoyMarker = path.join(helperDir, "decoy-ran");
    const pinnedHelper = `!${runtime} ${cli} credential`;
    await writeFile(
      runtime,
      `#!/bin/sh
if [ "$1" != "${cli}" ] || [ "$2" != "credential" ] || [ "$3" != "get" ]; then
  printf '%s\n' "unexpected pinned helper invocation: $*" >&2
  exit 86
fi
if [ "\${LEGION_HELPER_FAIL:-}" = "1" ]; then
  printf '%s\n' "pinned helper failed" >&2
  exit 87
fi
printf '%s\n' "username=x-access-token" "password=bot-token"
`
    );
    await writeFile(decoy, `#!/bin/sh\ntouch "$LEGION_DECOY_MARKER"\nexit 88\n`);
    await chmod(runtime, 0o700);
    await chmod(decoy, 0o700);
    await mkdir(repoCloneDir, { recursive: true });
    expect((await runCommand([SYSTEM_GIT, "init", "--bare", gitDir])).exitCode).toBe(0);
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper: pinnedHelper,
      run: (cmd, opts) =>
        cmd[0] === "git"
          ? runCommand([SYSTEM_GIT, ...cmd.slice(1)], opts)
          : Promise.resolve({ exitCode: 0, stdout: "", stderr: "" }),
    });

    const configured = await runCommand([
      SYSTEM_GIT,
      `--git-dir=${gitDir}`,
      "config",
      "--get",
      "credential.helper",
    ]);
    expect(configured.stdout.trim()).toBe(pinnedHelper);

    const helperEnv = {
      PATH: `${helperDir}:${process.env.PATH ?? ""}`,
      LEGION_DECOY_MARKER: decoyMarker,
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_NOSYSTEM: "1",
      HOME: helperDir,
      XDG_CONFIG_HOME: helperDir,
      GIT_TERMINAL_PROMPT: "0",
      GIT_CONFIG_COUNT: "0",
    };
    const credential = await fillCredential(gitDir, helperEnv);
    expect(credential.exitCode).toBe(0);
    expect(credential.stdout).toContain("username=x-access-token\npassword=bot-token\n");
    expect(existsSync(decoyMarker)).toBeFalse();

    const failedCredential = await fillCredential(gitDir, {
      ...helperEnv,
      LEGION_HELPER_FAIL: "1",
    });
    expect(failedCredential.exitCode).not.toBe(0);
    expect(failedCredential.stderr).toContain("pinned helper failed");
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
      credentialHelper,
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
    const fetch = calls[1];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "workspace", "update-stale"],
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ["jj", "bookmark", "set", "legion/issue-42", "--allow-backwards"],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
    ]);
  });

  test("repairs a stale reused workspace after its remote issue branch is deleted", async () => {
    for (const { name, command } of [
      { name: "local Sami JJ", command: ["jj"] },
      { name: "stock JJ 0.44", command: STOCK_JJ },
    ]) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const issue = formatIssueKey("acme", "widgets", 42);
      const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
      const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
      const remoteDir = path.join(stateDir, "remote");
      const siblingDir = path.join(stateDir, "sibling");
      const bookmark = "legion/issue-42";
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

      const runSuccessfully = async (args: string[], options?: RunCall["opts"]) => {
        const result = await runCommand([...command, ...args], options);
        expect(result.exitCode, `${name}: ${args.join(" ")}\n${result.stderr}`).toBe(0);
        return result;
      };
      const deps = {
        extensionPackage,
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        run: (cmd: string[], opts?: RunCall["opts"]) =>
          runCommand(cmd[0] === "jj" ? [...command, ...cmd.slice(1)] : cmd, opts),
      };

      await mkdir(path.dirname(repoCloneDir), { recursive: true });
      await runSuccessfully(["git", "init", "--colocate", remoteDir]);
      await runSuccessfully(["git", "init", "--colocate", repoCloneDir]);
      await runSuccessfully(["bookmark", "set", "main"], { cwd: repoCloneDir });
      await runSuccessfully(["git", "remote", "add", "origin", remoteDir], { cwd: repoCloneDir });

      await provisionIssueWorkspace(issue, deps);
      await runSuccessfully([
        "workspace",
        "add",
        siblingDir,
        "--name",
        "sibling",
        "--revision",
        "main",
        "-R",
        repoCloneDir,
      ]);
      await runSuccessfully(["new", "-m", "sibling advancement"], { cwd: siblingDir });
      await runSuccessfully(["bookmark", "set", "sibling-advance"], { cwd: siblingDir });
      await runSuccessfully(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      await runSuccessfully(["bookmark", "delete", bookmark], { cwd: repoCloneDir });
      await runSuccessfully(
        ["git", "push", "--remote", "origin", "--deleted", "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      expect(
        (await runCommand([...command, "bookmark", "list", bookmark], { cwd: remoteDir })).stdout
      ).not.toContain(bookmark);
      await runSuccessfully(
        ["bookmark", "set", "--revision", "sibling-advance", "--allow-backwards", bookmark],
        { cwd: repoCloneDir }
      );

      await expect(provisionIssueWorkspace(issue, deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      expect(
        (await runCommand([...command, "bookmark", "list", bookmark], { cwd: workspaceDir })).stdout
      ).toContain(bookmark);
    }
  });

  test("recovers a jj registration for a deleted issue workspace", async () => {
    const stateDir = await temporaryDirectory();
    const issue = formatIssueKey("acme", "widgets", 42);
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "issue-42");
    const gitDir = path.join(repoCloneDir, ".git");
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
        credentialHelper,
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
      ["jj", "bookmark", "set", "legion/issue-42", "--allow-backwards"],
      ...credentialConfigCommands(gitDir, credentialHelper),
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
        credentialHelper,
        run: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      })
    ).rejects.toThrow(`Incomplete Jujutsu clone at ${repoCloneDir}`);
  });
});
