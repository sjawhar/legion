import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { existsSync, watch } from "node:fs";
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  ownCommitsRevset,
  provisionIssueWorkspace,
  type RunResult,
  removeIssueWorkspace,
  type WorkspaceSpec,
} from "./workspace";

type RunCall = {
  readonly cmd: string[];
  readonly opts:
    | {
        readonly cwd?: string;
        readonly env?: Readonly<Record<string, string>>;
        readonly timeoutMs?: number;
      }
    | undefined;
};
const commandTimeoutMs = 300_000;
function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
/** Resolves once `target` exists, woken by `watchedDir`'s own filesystem events — never a timer.
 * The re-check after arming covers a change that landed between the first check and the watch. */
function whenPathExists(watchedDir: string, target: string): Promise<void> {
  if (existsSync(target)) return Promise.resolve();
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  const watcher = watch(watchedDir, () => {
    if (!existsSync(target)) return;
    watcher.close();
    resolve();
  });
  watcher.on("error", reject);
  if (existsSync(target)) {
    watcher.close();
    resolve();
  }
  return promise;
}
/** The environment provisioning hands its clone and fetch on the tmux runtime, exactly: the token
 * the one-shot helper answers with, no askpass and no terminal prompt, and the pairs that reset the
 * clone's credential-helper chain (LEGION-178) and then name that helper for https://github.com
 * alone. The exact key set is the contract: the token travels only through
 * `LEGION_PROVISIONING_TOKEN`, never as inline config. */
function provisioningEnv(call: RunCall): Readonly<Record<string, string>> {
  const env = call.opts?.env;
  if (!env) throw new Error("Provisioning command did not receive an environment");
  expect(env).toEqual({
    GIT_ASKPASS: "",
    GIT_TERMINAL_PROMPT: "0",
    LEGION_PROVISIONING_TOKEN: "installation-token",
    GIT_CONFIG_COUNT: "3",
    GIT_CONFIG_KEY_0: "credential.helper",
    GIT_CONFIG_VALUE_0: "",
    GIT_CONFIG_KEY_1: "credential.https://github.com.helper",
    GIT_CONFIG_VALUE_1: expect.stringMatching(/^!'.+\/provisioning-credential-[^/]+\/helper'$/),
    GIT_CONFIG_KEY_2: "core.hooksPath",
    GIT_CONFIG_VALUE_2: "/dev/null",
  });
  return env;
}
/** The one-shot helper's path, out of the `!'<path>'` the environment names it by. */
function provisioningHelper(env: Readonly<Record<string, string>>): string {
  return (env.GIT_CONFIG_VALUE_1 ?? "").slice(2, -1);
}

const temporaryDirectories: string[] = [];

afterEach(async () => {
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
  env: Readonly<Record<string, string>>,
  url = "https://github.com"
): Promise<RunResult> {
  const child = Bun.spawn(
    [
      "sh",
      "-c",
      `printf 'url=%s\\n\\n' "$2" | ${SYSTEM_GIT} --git-dir="$1" credential fill`,
      "sh",
      gitDir,
      url,
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
/** A TLS stand-in for a host a tree agent controls. It answers every request with a 401 and a Basic
 * challenge, so a git that holds a credential for it sends one, and it records the path and any
 * Authorization header of every request. */
async function plantedHost() {
  const dir = await temporaryDirectory();
  const key = path.join(dir, "key.pem");
  const cert = path.join(dir, "cert.pem");
  const made = await runCommand([
    "openssl",
    "req",
    "-x509",
    "-newkey",
    "ec",
    "-pkeyopt",
    "ec_paramgen_curve:P-256",
    "-nodes",
    "-keyout",
    key,
    "-out",
    cert,
    "-days",
    "1",
    "-subj",
    "/CN=127.0.0.1",
  ]);
  expect(made.exitCode, made.stderr).toBe(0);
  const requests: string[] = [];
  const authorizations: string[] = [];
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    tls: { key: Bun.file(key), cert: Bun.file(cert) },
    fetch(request) {
      requests.push(new URL(request.url).pathname);
      const authorization = request.headers.get("authorization");
      if (authorization) authorizations.push(authorization);
      return new Response("", {
        status: 401,
        headers: { "WWW-Authenticate": 'Basic realm="planted"' },
      });
    },
  });
  return {
    url: `https://127.0.0.1:${server.port}`,
    requests,
    authorizations,
    stop: () => server.stop(true),
  };
}
/** Runs `body` with the global git configuration at `file` and no system configuration, as a host
 * whose user configuration is exactly that file; the process environment is restored after. */
async function withGlobalGitConfig<T>(file: string, body: () => Promise<T>): Promise<T> {
  const saved = {
    GIT_CONFIG_GLOBAL: process.env.GIT_CONFIG_GLOBAL,
    GIT_CONFIG_NOSYSTEM: process.env.GIT_CONFIG_NOSYSTEM,
  };
  process.env.GIT_CONFIG_GLOBAL = file;
  process.env.GIT_CONFIG_NOSYSTEM = "1";
  try {
    return await body();
  } finally {
    for (const [name, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
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
/** The command `createWorkspace` runs before anything else: every row of the bookmark, one per
 * line, `<where>|<present>|<conflict>|<tracked>|<adds>|<removes>` (the Go twin's template). */
function bookmarkRowsCommand(bookmark: string, repoCloneDir: string): string[] {
  return [
    "jj",
    "bookmark",
    "list",
    "--all-remotes",
    `exact:${bookmark}`,
    "-T",
    'if(remote, remote, "local") ++ "|" ++ if(present, "1", "0") ++ "|" ++ if(conflict, "1", "0") ++ "|" ++ if(tracked, "1", "0") ++ "|" ++ added_targets.map(|c| c.commit_id()).join(",") ++ "|" ++ removed_targets.map(|c| c.commit_id()).join(",") ++ "\\n"',
    "--ignore-working-copy",
    "-R",
    repoCloneDir,
  ];
}
/** The read provisioning runs after the clone step and before every fetch: the clone's per-repo
 * `git.abandon-unreachable-commits` setting (LEGION-84). */
function readKeepUnreachableCommitsCommand(repoCloneDir: string): string[] {
  return ["jj", "config", "get", "git.abandon-unreachable-commits", "-R", repoCloneDir];
}
/** The write that follows the read only when it did not print `false`: with the setting `false`,
 * the fetch that deletes a merged branch's bookmark leaves its commits and every working copy
 * alone (LEGION-84). */
function writeKeepUnreachableCommitsCommand(repoCloneDir: string): string[] {
  return [
    "jj",
    "config",
    "set",
    "--repo",
    "git.abandon-unreachable-commits",
    "false",
    "-R",
    repoCloneDir,
  ];
}
/** The `jj workspace add` provisioning runs: the workspace's directory and name, the revision it is
 * created at (a commit id when the bookmark resolved, `main` when nothing did), against the clone. */
function workspaceAddCommand(
  workspaceDir: string,
  workspaceName: string,
  revision: string,
  repoCloneDir: string
): string[] {
  return [
    "jj",
    "workspace",
    "add",
    workspaceDir,
    "--name",
    workspaceName,
    "--revision",
    revision,
    "-R",
    repoCloneDir,
  ];
}

/** The registration check `removeIssueWorkspace` runs first (LEGION-104), verbatim: one workspace
 * name per line, against the shared clone with `--ignore-working-copy`, since the issue's own
 * directory may already be gone and no other workspace's working copy is to be touched. */
function workspaceListCommand(repoCloneDir: string): string[] {
  return [
    "jj",
    "workspace",
    "list",
    "-T",
    'name ++ "\n"',
    "--ignore-working-copy",
    "-R",
    repoCloneDir,
  ];
}
/** `git worktree list --porcelain` rows are `worktree <path>`: the directories git still knows. */
async function gitWorktrees(repoCloneDir: string): Promise<string[]> {
  const listed = await runCommand([
    SYSTEM_GIT,
    `--git-dir=${path.join(repoCloneDir, ".git")}`,
    "worktree",
    "list",
    "--porcelain",
  ]);
  expect(listed.exitCode, listed.stderr).toBe(0);
  return listed.stdout
    .split("\n")
    .filter((line) => line.startsWith("worktree "))
    .map((line) => line.slice("worktree ".length));
}

const JJ_BINARIES = [
  { name: "local Sami JJ", command: ["jj"] },
  { name: "stock JJ 0.44", command: STOCK_JJ },
] as const;

/** A colocated scratch remote, a colocated clone of it at the daemon's repo path with `main` set
 * and `origin` pointing at the remote, and a `provisionIssueWorkspace` dependency set that runs
 * `jj` as `command` and records every command line it is asked to run. Every jj invocation — the
 * rig's own and provisioning's — carries a commit identity through `JJ_USER`/`JJ_EMAIL`: the CI
 * runner has no jj user config, and `jj git push` refuses a commit with no author. */
async function realJjRig(command: readonly string[], stateDir: string) {
  const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
  const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
  const remoteDir = path.join(stateDir, "remote");
  const calls: string[][] = [];
  const withIdentity = (options?: RunCall["opts"]): RunCall["opts"] => ({
    ...options,
    env: { ...options?.env, JJ_USER: "Legion test", JJ_EMAIL: "legion-test@example.invalid" },
  });
  const jj = async (args: string[], options?: RunCall["opts"]) => {
    const result = await runCommand([...command, ...args], withIdentity(options));
    expect(result.exitCode, `${command.join(" ")} ${args.join(" ")}\n${result.stderr}`).toBe(0);
    return result;
  };
  const commitOf = async (revision: string, cwd: string = repoCloneDir) =>
    (await jj(["log", "-r", revision, "--no-graph", "-T", "commit_id"], { cwd })).stdout.trim();
  const deps = {
    repo: "acme/widgets" as const,
    stateDir,
    provisioningToken: async () => "installation-token",
    credentialHelper,
    commandTimeoutMs,
    isolateGitConfig: false,
    run: (cmd: string[], opts?: RunCall["opts"]) => {
      calls.push(cmd);
      return cmd[0] === "jj"
        ? runCommand([...command, ...cmd.slice(1)], withIdentity(opts))
        : runCommand(cmd, opts);
    },
  };
  await mkdir(path.dirname(repoCloneDir), { recursive: true });
  await jj(["git", "init", "--colocate", remoteDir]);
  await jj(["git", "init", "--colocate", repoCloneDir]);
  await jj(["bookmark", "set", "main"], { cwd: repoCloneDir });
  await jj(["git", "remote", "add", "origin", remoteDir], { cwd: repoCloneDir });
  /** Deletes branch on the remote, as GitHub does when a pull request merges or a branch is
   * deleted by hand. */
  const deleteRemoteBranch = async (branch: string) => {
    const deleted = await runCommand([
      SYSTEM_GIT,
      `--git-dir=${remoteDir}/.git`,
      "branch",
      "-D",
      branch,
    ]);
    expect(deleted.exitCode, `${command.join(" ")}: ${deleted.stderr}`).toBe(0);
  };
  /** What a refused provisioning must leave: no `jj workspace add` among its commands, no
   * directory, and no registration for the next resume to adopt. */
  const expectNothingRegistered = async (label: string) => {
    expect(
      calls.some((cmd) => cmd[1] === "workspace" && cmd[2] === "add"),
      label
    ).toBeFalse();
    expect(existsSync(workspaceDir), label).toBeFalse();
    expect((await jj(["workspace", "list", "-R", repoCloneDir])).stdout, label).not.toContain(
      "widgets-42:"
    );
  };
  return {
    repoCloneDir,
    workspaceDir,
    remoteDir,
    calls,
    jj,
    commitOf,
    deps,
    deleteRemoteBranch,
    expectNothingRegistered,
  };
}

/** The repository-scoped identity probes every provisioning runs (`removeRepoScopedIdentity`);
 * an `unset` follows a probe only when it printed a value. `--include-overridden` makes the probe
 * see a value the daemon's own `JJ_USER`/`JJ_EMAIL` would otherwise hide. */
function identityProbe(repoCloneDir: string, key: string): string[] {
  return ["jj", "config", "list", "--repo", "--include-overridden", "-R", repoCloneDir, key];
}
function identityProbeCommands(repoCloneDir: string): string[][] {
  return [identityProbe(repoCloneDir, "user.name"), identityProbe(repoCloneDir, "user.email")];
}

describe("provisionIssueWorkspace", () => {
  test("clones a missing repository into a temporary sibling, renames it into place, then fetches and provisions its issue workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const bookmark = "legion/WIDGETS-42";
    const calls: RunCall[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let spec: WorkspaceSpec;
    let logged: unknown[][];
    try {
      spec = await provisionIssueWorkspace(issue, {
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd, opts) => {
          calls.push({ cmd, opts });
          if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
            const target = cmd[4];
            if (!target) throw new Error("clone is missing its destination");
            // The clone lands in a sibling of the final path, never at the final path itself.
            expect(target).not.toBe(repoCloneDir);
            expect(existsSync(repoCloneDir)).toBeFalse();
            await mkdir(path.join(target, ".jj"), { recursive: true });
          }
          if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
            await mkdir(workspaceDir, { recursive: true });
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      });
      logged = errorSpy.mock.calls;
    } finally {
      errorSpy.mockRestore();
    }

    const [clone, read, write, fetch] = calls;
    if (!clone || !read || !write || !fetch) {
      throw new Error("Provisioning did not clone and fetch the repository");
    }
    const cloneEnv = provisioningEnv(clone);
    expect(provisioningEnv(fetch)).toEqual(cloneEnv);
    // The credential reaches the clone and the fetch, never the settings read or write.
    expect(read.opts?.env).toBeUndefined();
    expect(write.opts?.env).toBeUndefined();
    expect(existsSync(provisioningHelper(cloneEnv))).toBeFalse();
    expect(calls.map((call) => call.cmd)).toEqual([
      [
        "jj",
        "git",
        "clone",
        "https://github.com/acme/widgets",
        expect.stringMatching(new RegExp(`^${escapeRegExp(repoCloneDir)}\\.clone-`)),
      ],
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      bookmarkRowsCommand(bookmark, repoCloneDir),
      ["git", `--git-dir=${repoCloneDir}/.git`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", "main", repoCloneDir),
      ["jj", "bookmark", "set", bookmark, "-r", "@"],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    // The bookmark is created in the new workspace, on its own working copy — and a brand-new
    // issue has no bookmark to miss, so nothing is logged.
    const bookmarkSet = calls.find((call) => call.cmd[1] === "bookmark" && call.cmd[2] === "set");
    expect(bookmarkSet?.opts?.cwd).toBe(workspaceDir);
    expect(logged).toEqual([]);
    // Every provisioning command runs under the slow budget, not the runner's generic default.
    expect(calls.map((call) => call.opts?.timeoutMs)).toEqual(calls.map(() => commandTimeoutMs));
    // Provisioning reads the clone's jj config (the identity probes) and never sets an identity
    // key in it; the one key it does set is `git.abandon-unreachable-commits` (LEGION-84).
    expect(
      calls.some(
        ({ cmd }) =>
          cmd[0] === "jj" && cmd[1] === "config" && cmd[2] === "set" && cmd.includes("user.name")
      )
    ).toBeFalse();
    expect(
      calls.some(
        ({ cmd }) =>
          cmd[0] === "jj" && cmd[1] === "config" && cmd[2] === "set" && cmd.includes("user.email")
      )
    ).toBeFalse();
    expect(existsSync(path.join(repoCloneDir, ".jj"))).toBeTrue();
    expect(
      (await readdir(path.dirname(repoCloneDir))).filter((entry) => entry.startsWith("widgets."))
    ).toEqual([]);
    expect(existsSync(path.join(workspaceDir, ".omp", "config.yml"))).toBeFalse();
    expect(spec).toEqual({ repoCloneDir, workspaceDir, bookmark });
  });

  test("two issues cloning the same repository for the first time at once both provision; the clone that lands second yields to the one already in place", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const jjDir = path.join(repoCloneDir, ".jj");
    const workspacesDir = path.join(stateDir, "workspaces", "acme", "widgets");
    const workspaceDir42 = path.join(workspacesDir, "widgets-42");
    const workspaceDir43 = path.join(workspacesDir, "widgets-43");
    const cloneTargets: string[] = [];
    // Neither clone returns before both callers are inside `jj git clone`: both have passed the
    // "already cloned?" check and hold a temporary destination, so two clones always run.
    const bothCloning = Promise.withResolvers<void>();

    const deps = {
      repo: "acme/widgets" as const,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
      run: async (cmd: string[]) => {
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
          const target = cmd[4];
          if (!target) throw new Error("clone is missing its destination");
          cloneTargets.push(target);
          if (cloneTargets.length === 1) {
            await bothCloning.promise;
          } else {
            bothCloning.resolve();
            // The second clone finishes only once the first has been renamed into place, so its
            // own rename collides with a complete clone — the branch under test.
            await whenPathExists(path.dirname(repoCloneDir), jjDir);
          }
          await mkdir(path.join(target, ".jj"), { recursive: true });
          await writeFile(path.join(target, ".jj", "clone-origin"), target, "utf8");
        }
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          const directory = cmd[3];
          if (!directory) throw new Error("workspace add is missing its directory");
          await mkdir(directory, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    };

    const [first, second] = await Promise.all([
      provisionIssueWorkspace("WIDGETS-42", deps),
      provisionIssueWorkspace("WIDGETS-43", deps),
    ]);

    expect(first).toEqual({
      repoCloneDir,
      workspaceDir: workspaceDir42,
      bookmark: "legion/WIDGETS-42",
    });
    expect(second).toEqual({
      repoCloneDir,
      workspaceDir: workspaceDir43,
      bookmark: "legion/WIDGETS-43",
    });
    expect(existsSync(jjDir)).toBeTrue();
    // The clone that landed is the first one; the loser did not rename its own over it.
    expect(await readFile(path.join(jjDir, "clone-origin"), "utf8")).toBe(cloneTargets[0]);
    // The surplus clone is gone: no `widgets.clone-*` sibling beside the one that landed.
    expect(await readdir(path.dirname(repoCloneDir))).toEqual(["widgets"]);
    const temporaryClone = new RegExp(`^${escapeRegExp(repoCloneDir)}\\.clone-`);
    expect(cloneTargets).toEqual([
      expect.stringMatching(temporaryClone),
      expect.stringMatching(temporaryClone),
    ]);
    expect(cloneTargets[1]).not.toBe(cloneTargets[0]);
    expect(existsSync(workspaceDir42)).toBeTrue();
    expect(existsSync(workspaceDir43)).toBeTrue();
  });

  test("creates a workspace on top of a resolving bookmark and writes no bookmark", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const bookmark = "legion/WIDGETS-42";
    const commit = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d";
    const calls: RunCall[] = [];
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: unknown[][];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          isolateGitConfig: false,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "bookmark" && cmd[2] === "list") {
              return { exitCode: 0, stdout: `local|1|0|0|${commit}|\n`, stderr: "" };
            }
            if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
              await mkdir(workspaceDir, { recursive: true });
            }
            return { exitCode: 0, stdout: "", stderr: "" };
          },
        })
      ).resolves.toEqual({ repoCloneDir, workspaceDir, bookmark });
      logged = errorSpy.mock.calls;
    } finally {
      errorSpy.mockRestore();
    }

    expect(calls.map((call) => call.cmd)).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      bookmarkRowsCommand(bookmark, repoCloneDir),
      ["git", `--git-dir=${repoCloneDir}/.git`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", commit, repoCloneDir),
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    expect(calls.some((call) => call.cmd[1] === "bookmark" && call.cmd[2] !== "list")).toBeFalse();
    expect(logged).toEqual([]);
  });

  test("creates a missing workspace parent before adding an issue workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await provisionIssueWorkspace(issue, {
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
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
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const gitDir = path.join(repoCloneDir, ".git");
    await mkdir(path.dirname(repoCloneDir), { recursive: true });
    expect(
      (await runCommand([...STOCK_JJ, "git", "init", "--colocate", repoCloneDir])).exitCode
    ).toBe(0);
    expect(
      (await runCommand([...STOCK_JJ, "bookmark", "set", "main", "-R", repoCloneDir])).exitCode
    ).toBe(0);
    // A repository-scoped identity on the shared clone, which provisioning must remove.
    for (const [key, value] of [
      ["user.name", "stale-bot[bot]"],
      ["user.email", "1+stale-bot[bot]@users.noreply.github.com"],
    ] as const) {
      expect(
        (
          await runCommand([
            ...STOCK_JJ,
            "config",
            "set",
            "--repo",
            "-R",
            repoCloneDir,
            key,
            JSON.stringify(value),
          ])
        ).exitCode
      ).toBe(0);
    }

    await provisionIssueWorkspace(issue, {
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
      run: async (cmd, opts) => {
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch") {
          return { exitCode: 0, stdout: "", stderr: "" };
        }
        return runCommand(cmd[0] === "jj" ? [...STOCK_JJ, ...cmd.slice(1)] : cmd, opts);
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
    const repoIdentity = await runCommand([
      ...STOCK_JJ,
      "config",
      "list",
      "--repo",
      "--include-overridden",
      "-R",
      repoCloneDir,
      "user",
    ]);
    expect(repoIdentity.exitCode).toBe(0);
    expect(repoIdentity.stdout.trim()).toBe("");
  }, 60_000);
  test("runs the supplied pinned credential helper instead of a PATH Legion and fails loudly", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const helperDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
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

    await provisionIssueWorkspace(issue, {
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper: pinnedHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
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

  test("a fetch on a clone that carries the pane helper gets the one-shot credential for github.com alone, without running the pane helper", async () => {
    // The clone's persisted config is the pane's: `credential.helper` and the github.com-specific
    // entry name the pane helper, and `credential.interactive=false` keeps a pane's git from ever
    // prompting. Provisioning's own clone and fetch run with no grant, so that helper fails there
    // (LEGION-178). The environment provisioning hands the fetch resets the helper chain and names
    // the one-shot helper for https://github.com, for that command alone, leaving the persisted
    // config as it is. git asks that helper for every repository on github.com, whatever
    // credential.useHttpPath says (an operator's global configuration can set it), and never for
    // another scheme, host or port.
    const stateDir = path.join(await temporaryDirectory(), "state");
    const helperDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const gitDir = path.join(repoCloneDir, ".git");
    const paneHelper = path.join(helperDir, "failhelper");
    const marker = path.join(helperDir, "pane-helper-ran");
    const stubLine = "stub pane helper ran (no grant here)";
    await writeFile(
      paneHelper,
      `#!/bin/sh\ntouch "${marker}"\nprintf '%s\\n' "${stubLine}" >&2\nexit 1\n`,
      { mode: 0o700 }
    );
    await chmod(paneHelper, 0o700);
    await mkdir(repoCloneDir, { recursive: true });
    expect((await runCommand([SYSTEM_GIT, "init", "--bare", gitDir])).exitCode).toBe(0);
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    // Every fill below runs under this isolation: no system or inherited command-scope config (a
    // developer shell may carry GIT_CONFIG_COUNT pairs that reset the github.com helper), a
    // global config of the test's own, and the fixture directory as home, so the box's own
    // credential settings take no part.
    const globals = await temporaryDirectory();
    const isolation = (useHttpPath: boolean) => ({
      GIT_CONFIG_COUNT: "0",
      GIT_CONFIG_GLOBAL: path.join(globals, `use-http-path-${useHttpPath}`),
      GIT_CONFIG_NOSYSTEM: "1",
      HOME: helperDir,
      XDG_CONFIG_HOME: helperDir,
    });
    for (const useHttpPath of [false, true]) {
      await writeFile(
        isolation(useHttpPath).GIT_CONFIG_GLOBAL,
        `[credential]\n\tuseHttpPath = ${useHttpPath}\n`
      );
    }
    const urls = [
      { url: "https://github.com", answered: true },
      { url: "https://github.com/acme/widgets", answered: true },
      { url: "https://github.com/acme/other", answered: true },
      { url: "http://github.com/acme/widgets", answered: false },
      { url: "https://evil.example/acme/widgets", answered: false },
      { url: "https://github.com.evil.example/acme/widgets", answered: false },
      { url: "https://github.com:8443/acme/widgets", answered: false },
    ];
    type Fill = { readonly url: string; readonly useHttpPath: boolean; readonly result: RunResult };
    let observed:
      | { readonly fills: Fill[]; readonly markerAfterFill: boolean; readonly control: RunResult }
      | undefined;
    let observeFetch = false;
    const calls: RunCall[] = [];
    const deps = {
      repo: "acme/widgets" as const,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper: `!${paneHelper}`,
      commandTimeoutMs,
      isolateGitConfig: false,
      run: async (cmd: string[], opts?: RunCall["opts"]) => {
        calls.push({ cmd, opts });
        if (cmd[0] === "git") return runCommand([SYSTEM_GIT, ...cmd.slice(1)], opts);
        if (observeFetch && cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch") {
          // jj is stubbed here; stand in for the git its fetch spawns with exactly the fetch's
          // environment, while the one-shot helper still exists (provisioning removes it once
          // the fetch returns).
          const env = opts?.env;
          if (!env) throw new Error("the fetch did not receive an environment");
          const fills: Fill[] = [];
          for (const useHttpPath of [false, true]) {
            for (const { url } of urls) {
              // The fetch env's own GIT_CONFIG_COUNT=3 wins over the isolation's 0.
              const result = await fillCredential(
                gitDir,
                { ...isolation(useHttpPath), ...env },
                url
              );
              fills.push({ url, useHttpPath, result });
            }
          }
          const markerAfterFill = existsSync(marker);
          // The control is LEGION-178: the environment without the pairs, so nothing about the
          // clone's helper chain.
          const control = await fillCredential(gitDir, {
            GIT_ASKPASS: "",
            GIT_TERMINAL_PROMPT: "0",
            LEGION_PROVISIONING_TOKEN: env.LEGION_PROVISIONING_TOKEN as string,
            ...isolation(false),
          });
          observed = { fills, markerAfterFill, control };
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    };

    // The first provisioning writes production's config into the clone. The reads below run under
    // the same isolation as the fills: `--get-all` concatenates every scope, so a developer box's
    // global `credential.helper` would otherwise appear above the clone's own entries.
    await provisionIssueWorkspace(issue, deps);
    const persisted = async (...args: string[]) =>
      (
        await runCommand([SYSTEM_GIT, `--git-dir=${gitDir}`, "config", ...args], {
          env: isolation(false),
        })
      ).stdout;
    expect(await persisted("--get-all", "credential.helper")).toBe(`\n!${paneHelper}\n`);
    expect(await persisted("--get-all", "credential.https://github.com.helper")).toBe(
      `\n!${paneHelper}\n`
    );
    expect(await persisted("--get", "credential.interactive")).toBe("false\n");

    // The second provisioning fetches on that clone.
    observeFetch = true;
    calls.length = 0;
    await provisionIssueWorkspace(issue, deps);
    if (!observed) throw new Error("the fetch was not observed");
    for (const { url, useHttpPath, result } of observed.fills) {
      const label = `useHttpPath ${useHttpPath}, ${url}`;
      expect(result.stderr, label).not.toContain(stubLine);
      if (urls.find((u) => u.url === url)?.answered) {
        expect(result.exitCode, `${label}\n${result.stderr}`).toBe(0);
        expect(result.stdout, label).toContain(
          "username=x-access-token\npassword=installation-token\n"
        );
      } else {
        expect(result.exitCode, label).not.toBe(0);
        expect(result.stdout, label).not.toContain("password=");
      }
    }
    expect(observed.markerAfterFill).toBeFalse();
    // Without the pairs the pane helper runs, and fails.
    expect(existsSync(marker)).toBeTrue();
    expect(observed.control.stderr).toContain(stubLine);
    expect(observed.control.exitCode).not.toBe(0);
    // The persisted config is exactly what the first provisioning wrote: the pane's.
    expect(await persisted("--get-all", "credential.helper")).toBe(`\n!${paneHelper}\n`);
    expect(await persisted("--get", "credential.interactive")).toBe("false\n");
    // And the fetch that produced the fills received exactly the provisioning environment.
    const fetch = calls.find(
      ({ cmd }) => cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch"
    );
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
  });

  // A URL rewrite a tree agent wrote sends provisioning's credentialed fetch to a host of its own.
  // The one-shot credential answers https://github.com alone, so that host receives no credential,
  // whether the rewrite sits in the shared clone's own configuration, which every agent of the tree
  // can write, or, on the tmux runtime, in the global configuration of the daemon's user, which
  // every pane shares.
  for (const { where, pod, inGlobal } of [
    { where: "the shared clone's configuration on the tmux runtime", pod: false, inGlobal: false },
    { where: "the shared clone's configuration in a pod", pod: true, inGlobal: false },
    {
      where: "the daemon user's global configuration on the tmux runtime",
      pod: false,
      inGlobal: true,
    },
  ]) {
    test(`a remote a rewrite in ${where} sends elsewhere gets that host no credential`, async () => {
      for (const { name, command } of JJ_BINARIES) {
        const host = await plantedHost();
        try {
          const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
          const global = path.join(await temporaryDirectory(), "gitconfig");
          await writeFile(global, "");
          const target = inGlobal
            ? [`--file=${global}`]
            : [`--file=${path.join(rig.repoCloneDir, ".git", "config")}`];
          for (const [key, value] of [
            [`url.${host.url}/acme/widgets.insteadOf`, rig.remoteDir],
            ["http.sslVerify", "false"],
          ] as const) {
            const set = await runCommand([SYSTEM_GIT, "config", ...target, key, value]);
            expect(set.exitCode, set.stderr).toBe(0);
          }
          await withGlobalGitConfig(global, async () => {
            await expect(
              provisionIssueWorkspace("WIDGETS-42", { ...rig.deps, isolateGitConfig: pod }),
              name
            ).rejects.toThrow("jj git fetch");
          });
          // The rewrite took the fetch to the planted host, which then got no credential.
          expect(host.requests.length, name).toBeGreaterThan(0);
          expect(host.authorizations, name).toEqual([]);
        } finally {
          host.stop();
        }
      }
    }, 60_000);
  }

  test("a pod's credentialed fetch reads no global or system git configuration", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const host = await plantedHost();
      try {
        const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
        const global = path.join(await temporaryDirectory(), "gitconfig");
        await writeFile(
          global,
          `[url "${host.url}/acme/widgets"]\n\tinsteadOf = ${rig.remoteDir}\n[http]\n\tsslVerify = false\n`
        );
        const spec = await withGlobalGitConfig(global, () =>
          provisionIssueWorkspace("WIDGETS-42", { ...rig.deps, isolateGitConfig: true })
        );
        expect(spec.workspaceDir, name).toBe(rig.workspaceDir);
        expect(host.requests, name).toEqual([]);
      } finally {
        host.stop();
      }
    }
  }, 60_000);

  test("does not add a second workspace or run a bookmark command when an issue is reactivated", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const calls: RunCall[] = [];
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    const deps = {
      repo: "acme/widgets" as const,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
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
      bookmark: "legion/WIDGETS-42",
    });
    const fetch = calls[3];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "workspace", "update-stale"],
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
  });

  test("removes a repository-scoped jj identity from the shared clone, logging each key", async () => {
    // `--repo` on a workspace is the one config file every workspace of the clone shares, so a
    // `user.name`/`user.email` there is the author and committer for every tree's commits. Identity
    // rides each pane's environment; a value found here is removed, where the clone's config
    // writes already live, and nothing writes it.
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    const repoConfig = new Map<string, string>([
      ["user.name", 'user.name = "legion-reviewer[bot]"'],
      ["user.email", 'user.email = "3202653+legion-reviewer[bot]@users.noreply.github.com"'],
    ]);
    const calls: string[][] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: string[];
    try {
      await provisionIssueWorkspace(issue, {
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) => {
          calls.push(cmd);
          if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
            await mkdir(workspaceDir, { recursive: true });
          }
          if (cmd[0] === "jj" && cmd[1] === "config") {
            const key = cmd.at(-1) ?? "";
            if (cmd[2] === "list") {
              return { exitCode: 0, stdout: repoConfig.get(key) ?? "", stderr: "" };
            }
            if (cmd[2] === "unset" && !repoConfig.delete(key)) {
              return { exitCode: 1, stdout: "", stderr: `Error: "${key}" doesn't exist` };
            }
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      });
      logged = errorSpy.mock.calls.map((call) => String(call[0]));
    } finally {
      errorSpy.mockRestore();
    }

    // Every `jj config` command, pinned: the LEGION-84 read and write (the fake runner answers the
    // read with empty stdout, so the write always follows), then the identity probes and unsets —
    // any other identity write in provisioning fails this list.
    expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      identityProbe(repoCloneDir, "user.name"),
      ["jj", "config", "unset", "--repo", "-R", repoCloneDir, "user.name"],
      identityProbe(repoCloneDir, "user.email"),
      ["jj", "config", "unset", "--repo", "-R", repoCloneDir, "user.email"],
    ]);
    expect(repoConfig.size).toBe(0);
    expect(logged.filter((line) => line.includes("repository-scoped jj"))).toEqual([
      expect.stringContaining(`removing repository-scoped jj user.name from ${repoCloneDir}`),
      expect.stringContaining(`removing repository-scoped jj user.email from ${repoCloneDir}`),
    ]);
  });

  test("treats a stale identity another provisioning removed first as already gone", async () => {
    // Two issues provisioning the same clone at once both see the key; the second `unset` finds it
    // gone (exit 1). A re-probe that finds nothing is success, not a launch failure.
    const stateDir = path.join(await temporaryDirectory(), "state");
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    let nameProbes = 0;
    const calls: string[][] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await provisionIssueWorkspace("WIDGETS-42", {
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) => {
          calls.push(cmd);
          if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
            await mkdir(workspaceDir, { recursive: true });
          }
          if (cmd[0] === "jj" && cmd[1] === "config" && cmd.at(-1) === "user.name") {
            if (cmd[2] === "list") {
              nameProbes += 1;
              // Present on the first probe, gone by the re-probe after the failed unset.
              return { exitCode: 0, stdout: nameProbes === 1 ? 'user.name = "x"' : "", stderr: "" };
            }
            return { exitCode: 1, stdout: "", stderr: 'Error: "user.name" doesn\'t exist' };
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      });
    } finally {
      errorSpy.mockRestore();
    }
    // Every `jj config` command, pinned: the LEGION-84 read and write (the fake runner answers the
    // read with empty stdout, so the write always follows), then the identity probes and unset —
    // any other identity write in provisioning fails this list.
    expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      identityProbe(repoCloneDir, "user.name"),
      ["jj", "config", "unset", "--repo", "-R", repoCloneDir, "user.name"],
      identityProbe(repoCloneDir, "user.name"),
      identityProbe(repoCloneDir, "user.email"),
    ]);
  });

  test("fails the provision loudly when a stale identity cannot be removed", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        provisionIssueWorkspace("WIDGETS-42", {
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          isolateGitConfig: false,
          run: async (cmd) => {
            if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
              await mkdir(workspaceDir, { recursive: true });
            }
            if (cmd[0] === "jj" && cmd[1] === "config" && cmd.at(-1) === "user.name") {
              if (cmd[2] === "list") return { exitCode: 0, stdout: 'user.name = "x"', stderr: "" };
              return { exitCode: 1, stdout: "", stderr: "Error: Permission denied (os error 13)" };
            }
            return { exitCode: 0, stdout: "", stderr: "" };
          },
        })
      ).rejects.toThrow(
        `Command failed (exit 1): jj config unset --repo -R ${repoCloneDir} user.name\nError: Permission denied (os error 13)`
      );
    } finally {
      errorSpy.mockRestore();
    }
  });

  test("leaves an existing bookmark that points at another workspace's commit untouched after its remote branch is deleted", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, remoteDir, calls, jj, commitOf, deps } = await realJjRig(
        command,
        stateDir
      );
      const siblingDir = path.join(stateDir, "sibling");
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      // Snapshot the initial working copy so later concurrent workspace operations exercise its
      // recorded jj state.
      await jj(["status"], { cwd: workspaceDir });
      await jj(workspaceAddCommand(siblingDir, "sibling", "main", repoCloneDir).slice(1));
      await jj(["new", "-m", "sibling advancement"], { cwd: siblingDir });
      await jj(["bookmark", "set", "sibling-advance"], { cwd: siblingDir });
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      await jj(["bookmark", "delete", bookmark], { cwd: repoCloneDir });
      await jj(["git", "push", "--remote", "origin", "--deleted", "--allow-empty-description"], {
        cwd: repoCloneDir,
      });
      expect(
        (await runCommand([...command, "bookmark", "list", bookmark], { cwd: remoteDir })).stdout,
        name
      ).not.toContain(bookmark);
      await jj(
        ["bookmark", "set", "--revision", "sibling-advance", "--allow-backwards", bookmark],
        {
          cwd: repoCloneDir,
        }
      );
      const siblingCommit = await commitOf("sibling-advance");

      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      expect(
        calls.filter((cmd) => cmd.includes("bookmark")),
        name
      ).toEqual([]);
      // Before this change the second provision moved the bookmark onto this workspace's `@`.
      expect(await commitOf(bookmark), name).toBe(siblingCommit);
    }
    // Two real jj binaries (one resolved through mise) and ~30 subprocesses: well past bun's 5 s
    // default on a loaded host.
  }, 60_000);

  test("leaves the bookmark missing but the workspace intact after its merged pull request's branch is deleted", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps, deleteRemoteBranch } =
        await realJjRig(command, stateDir);
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      const firstProvisioning = calls.slice(0, 3);
      // The worker's work is a file the pushed commit carries. Snapshotting it makes that commit
      // the bookmark's final target — jj deletes a local bookmark on fetch only while it still
      // points where the deleted remote branch did.
      await writeFile(path.join(workspaceDir, "feature.txt"), "shipped\n", "utf8");
      await jj(["status"], { cwd: workspaceDir });
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      const pushedCommit = await commitOf(bookmark);
      // GitHub deletes the branch when the pull request merges.
      await deleteRemoteBranch(bookmark);

      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      // What the resumed worker finds: a working copy that is not stale, its pushed commit still
      // beneath it, its file still on disk, and the bookmark gone with its remote branch. Before
      // this change the fetch printed `Abandoned 1 commits that are no longer reachable`, rebased
      // the working copy onto the fork point, and `jj status` here failed with `The working copy
      // is stale`.
      const status = await jj(["status"], { cwd: workspaceDir });
      expect(status.stderr, name).not.toContain("stale");
      expect(
        (
          await jj(["log", "-r", "ancestors(@)", "--no-graph", "-T", 'commit_id ++ "\n"'], {
            cwd: workspaceDir,
          })
        ).stdout.split("\n"),
        name
      ).toContain(pushedCommit);
      expect(existsSync(path.join(workspaceDir, "feature.txt")), name).toBeTrue();
      expect((await jj(["bookmark", "list", bookmark], { cwd: repoCloneDir })).stdout, name).toBe(
        ""
      );
      // The clone's first provisioning read the setting (jj's default, `true`), wrote it once, and
      // only then fetched; this one finds `false` and only reads it before the fetch. No bookmark
      // command runs in either.
      expect(firstProvisioning, name).toEqual([
        readKeepUnreachableCommitsCommand(repoCloneDir),
        writeKeepUnreachableCommitsCommand(repoCloneDir),
        ["jj", "git", "fetch", "-R", repoCloneDir],
      ]);
      expect(calls, name).toEqual([
        ["jj", "workspace", "update-stale"],
        readKeepUnreachableCommitsCommand(repoCloneDir),
        ["jj", "git", "fetch", "-R", repoCloneDir],
        ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
        ...identityProbeCommands(repoCloneDir),
      ]);
    }
  }, 60_000);

  test("leaves an existing bookmark where it was when the working copy has moved on", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = await realJjRig(
        command,
        stateDir
      );
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      // The worker's shape: the pushed head carries the bookmark, `@` is new work on top of it.
      await jj(["new", "-m", "later work"], { cwd: workspaceDir });
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      const bookmarkCommit = await commitOf(bookmark);
      const workingCopy = await commitOf("@", workspaceDir);
      expect(workingCopy, name).not.toBe(bookmarkCommit);

      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      expect(
        calls.filter((cmd) => cmd.includes("bookmark")),
        name
      ).toEqual([]);
      expect(await commitOf(bookmark), name).toBe(bookmarkCommit);
      expect(await commitOf("@", workspaceDir), name).toBe(workingCopy);
    }
  }, 60_000);

  test("re-creates a forgotten workspace on top of its surviving bookmark without moving it", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = await realJjRig(
        command,
        stateDir
      );
      const bookmark = "legion/WIDGETS-42";
      const pointOperations = async () =>
        (
          await jj(["op", "log", "--no-graph", "-T", 'description ++ "\n"', "-R", repoCloneDir])
        ).stdout
          .split("\n")
          .filter((line) => line.startsWith(`point bookmark ${bookmark}`)).length;

      await provisionIssueWorkspace("WIDGETS-42", deps);
      await jj(["status"], { cwd: workspaceDir });
      const bookmarkCommit = await commitOf(bookmark);
      expect(await pointOperations(), name).toBe(1);
      // An operator's hand-run forget plus a wiped directory: jj no longer knows the workspace,
      // the bookmark is still there.
      await jj(["workspace", "forget", "widgets-42", "-R", repoCloneDir]);
      await rm(workspaceDir, { recursive: true, force: true });

      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      expect(
        calls.some((cmd) => cmd[1] === "bookmark" && cmd[2] !== "list"),
        name
      ).toBeFalse();
      // Before this change the fresh path started at `main` and the `bookmark set -r @` that
      // followed refused to move the surviving bookmark sideways.
      expect(await commitOf("@-", workspaceDir), name).toBe(bookmarkCommit);
      expect(await commitOf(bookmark), name).toBe(bookmarkCommit);
      expect(await pointOperations(), name).toBe(1);
    }
  }, 60_000);

  type Rig = Awaited<ReturnType<typeof realJjRig>>;
  const issueBookmark = "legion/WIDGETS-42";

  /** A commit on main in the rig's clone carrying `<label>.txt`, left out of the working copy. */
  async function commitOnMain(rig: Rig, label: string): Promise<string> {
    await rig.jj(["new", "main", "-m", label], { cwd: rig.repoCloneDir });
    await writeFile(path.join(rig.repoCloneDir, `${label}.txt`), `${label}\n`, "utf8");
    const commit = await rig.commitOf("@");
    await rig.jj(["new", "main"], { cwd: rig.repoCloneDir });
    return commit;
  }

  /** Puts `legion/WIDGETS-42` on origin at a commit carrying `fixture.txt`, pushed from the rig's
   * clone as another clone would have pushed it, and returns that commit. The local bookmark is
   * left for the caller to remove the way its case needs. */
  async function pushIssueBranch(rig: Rig): Promise<string> {
    const pushed = await commitOnMain(rig, "fixture");
    await rig.jj(["bookmark", "create", issueBookmark, "-r", pushed], { cwd: rig.repoCloneDir });
    // The rig's main is an empty commit with no description, which jj pushes only when told to.
    await rig.jj(
      [
        "git",
        "push",
        "--remote",
        "origin",
        "--bookmark",
        issueBookmark,
        "--allow-empty-description",
      ],
      { cwd: rig.repoCloneDir }
    );
    return pushed;
  }

  /** Moves `legion/WIDGETS-42` on origin to commit, as another clone's push would. */
  async function moveRemoteBranch(rig: Rig, commit: string): Promise<void> {
    const moved = await runCommand([
      SYSTEM_GIT,
      `--git-dir=${rig.repoCloneDir}/.git`,
      "push",
      "--force",
      rig.remoteDir,
      `${commit}:refs/heads/${issueBookmark}`,
    ]);
    expect(moved.exitCode, moved.stderr).toBe(0);
  }

  /** Provisions WIDGETS-42 through deps and answers the refusal's message, or fails the test. */
  async function refusal(deps: Rig["deps"]): Promise<string> {
    return provisionIssueWorkspace("WIDGETS-42", deps).then(
      () => "provisioning resolved",
      (error: Error) => error.message
    );
  }

  test("the credentialed fetch runs no hook the shared clone carries, in .git/hooks or under a configured core.hooksPath", async () => {
    for (const { name, command } of JJ_BINARIES) {
      for (const placement of ["hooks directory", "core.hooksPath"] as const) {
        const label = `${name}, ${placement}`;
        const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
        const { repoCloneDir, remoteDir, jj, deps } = rig;
        // A hook git runs on the fetch's ref update, recording whether the provisioning token was
        // in its environment: what a tree could plant in the shared clone it writes.
        const sink = path.join(await temporaryDirectory(), "hook.log");
        const hooks =
          placement === "hooks directory"
            ? path.join(repoCloneDir, ".git", "hooks")
            : path.join(await temporaryDirectory(), "planted-hooks");
        await mkdir(hooks, { recursive: true });
        const hook = path.join(hooks, "reference-transaction");
        await writeFile(
          hook,
          `#!/bin/sh\nprintf 'hook token=%s\\n' "\${LEGION_PROVISIONING_TOKEN:+set}" >> ${JSON.stringify(sink)}\ncat >/dev/null\n`,
          { mode: 0o700 }
        );
        if (placement === "core.hooksPath") {
          const set = await runCommand([
            SYSTEM_GIT,
            `--git-dir=${path.join(repoCloneDir, ".git")}`,
            "config",
            "core.hooksPath",
            hooks,
          ]);
          expect(set.exitCode, `${label}: ${set.stderr}`).toBe(0);
        }
        // origin's main moves, so the fetch updates a ref and git runs the hook if it may.
        await jj(["commit", "-m", "upstream"], { cwd: remoteDir });
        await jj(["bookmark", "set", "main", "-r", "@-"], { cwd: remoteDir });

        await provisionIssueWorkspace("WIDGETS-42", deps);
        const recorded = existsSync(sink) ? await readFile(sink, "utf8") : "";
        expect(recorded, label).not.toContain("token=set");
      }
    }
  }, 120_000);

  test("adopts an issue branch only origin has: tracks it and adds the workspace at its commit", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = rig;
      const pushed = await pushIssueBranch(rig);
      // The clone knows the branch only as an untracked origin row: the shape a fresh clone has for
      // a branch another clone pushed (a repository's fixture, or a tree's branch before its volume
      // was lost).
      await jj(["bookmark", "forget", issueBookmark], { cwd: repoCloneDir });
      expect(
        (await jj(["bookmark", "list", "--all-remotes", issueBookmark], { cwd: repoCloneDir }))
          .stdout,
        name
      ).toStartWith(`${issueBookmark}@origin:`);

      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark: issueBookmark,
      });
      // The workspace starts on the branch, its pushed file is there, and the local bookmark is the
      // origin row, tracked, so the issue's next push moves it rather than diverging from it.
      expect(await commitOf("@-", workspaceDir), name).toBe(pushed);
      expect(existsSync(path.join(workspaceDir, "fixture.txt")), name).toBeTrue();
      expect(await commitOf(issueBookmark), name).toBe(pushed);
      expect(
        (await jj(["bookmark", "list", "--tracked", issueBookmark], { cwd: repoCloneDir })).stdout,
        name
      ).toContain(`${issueBookmark}: `);
      expect(
        calls.filter((cmd) => cmd[1] === "bookmark" && cmd[2] !== "list"),
        name
      ).toEqual([
        [
          "jj",
          "bookmark",
          "track",
          `${issueBookmark}@origin`,
          "--ignore-working-copy",
          "-R",
          repoCloneDir,
        ],
      ]);
    }
  }, 60_000);

  test("refuses by name a tracked origin row with no local bookmark, before any workspace add", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = rig;
      const pushed = await pushIssueBranch(rig);
      // A local deletion never pushed: a `jj bookmark delete`, or a `jj abandon` of the commit the
      // bookmark pointed at, leaves this shape, and `jj bookmark track` is a no-op on it.
      await jj(["bookmark", "delete", issueBookmark], { cwd: repoCloneDir });

      for (const attempt of [1, 2]) {
        calls.length = 0;
        expect(await refusal(deps), `${name}, attempt ${attempt}`).toBe(
          `Bookmark ${issueBookmark} was deleted in the shared clone ${repoCloneDir} and the deletion never pushed, while ${issueBookmark}@origin is tracked at ${pushed}; workspace ${workspaceDir} was not created. ` +
            `Restore it: \`jj bookmark set ${issueBookmark} -r ${issueBookmark}@origin -R ${repoCloneDir}\`. ` +
            `Cancel the deletion, and the next provisioning adopts origin's branch: \`jj bookmark forget ${issueBookmark} -R ${repoCloneDir}\`. ` +
            `Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or \`gh api -X DELETE repos/acme/widgets/git/refs/heads/${issueBookmark}\`), and the next provisioning starts at main.`
        );
        await rig.expectNothingRegistered(`${name}, attempt ${attempt}`);
      }

      // The third way out, followed: the branch is deleted on GitHub, and the next provisioning's
      // fetch drops the origin row, so the workspace starts at main with none of the branch.
      await rig.deleteRemoteBranch(issueBookmark);
      await expect(provisionIssueWorkspace("WIDGETS-42", deps), name).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark: issueBookmark,
      });
      expect(await commitOf("@-", workspaceDir), name).toBe(await commitOf("main"));
      expect(existsSync(path.join(workspaceDir, "fixture.txt")), name).toBeFalse();
      expect(await commitOf(issueBookmark), name).toBe(await commitOf("@", workspaceDir));
    }
  }, 60_000);

  // A conflicted local bookmark is refused by name, and each of the refusal's two ways out is followed
  // as it is written, every command in order: keep an added commit, which the workspace then starts
  // at; or start from main, deleting the local bookmark, after deleting the branch on GitHub when
  // origin has it. A conflict with a deleted side resolves to its other side alone under
  // `bookmarks(exact:legion/<KEY>)`, as if the bookmark were whole; with two added commits the
  // GitHub deletion alone would leave the local side in conflict with a deletion.
  for (const shape of [
    {
      name: "a local deletion never pushed, then origin's branch moved",
      build: async (rig: Rig) => {
        const base = await pushIssueBranch(rig);
        await rig.jj(["bookmark", "delete", issueBookmark], { cwd: rig.repoCloneDir });
        const moved = await commitOnMain(rig, "moved");
        await moveRemoteBranch(rig, moved);
        return { base, added: [moved], originHasIt: true };
      },
    },
    {
      name: "a local move never pushed, then the branch deleted on GitHub",
      build: async (rig: Rig) => {
        const base = await pushIssueBranch(rig);
        const moved = await commitOnMain(rig, "moved");
        await rig.jj(["bookmark", "set", issueBookmark, "-r", moved, "--allow-backwards"], {
          cwd: rig.repoCloneDir,
        });
        await rig.deleteRemoteBranch(issueBookmark);
        return { base, added: [moved], originHasIt: false };
      },
    },
    {
      name: "a local move never pushed while origin's branch moved",
      build: async (rig: Rig) => {
        const base = await pushIssueBranch(rig);
        const local = await commitOnMain(rig, "local");
        await rig.jj(["bookmark", "set", issueBookmark, "-r", local, "--allow-backwards"], {
          cwd: rig.repoCloneDir,
        });
        const remote = await commitOnMain(rig, "remote");
        await moveRemoteBranch(rig, remote);
        return { base, added: [local, remote], originHasIt: true };
      },
    },
  ]) {
    test(`refuses a conflicted bookmark, ${shape.name}, and follows both ways out`, async () => {
      for (const { name, command } of JJ_BINARIES) {
        for (const wayOut of ["keep", "main"] as const) {
          const label = `${name}, ${wayOut}`;
          const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
          const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = rig;
          const { base, added, originHasIt } = await shape.build(rig);
          const [kept] = added;
          const deleteLocal = `\`jj bookmark delete ${issueBookmark} -R ${repoCloneDir}\``;
          const fromMain = originHasIt
            ? `delete the branch on GitHub (the pull request's Delete branch button, or \`gh api -X DELETE repos/acme/widgets/git/refs/heads/${issueBookmark}\`) and run ${deleteLocal}`
            : deleteLocal;
          const keep =
            added.length > 1
              ? `Keep one of its added commits: \`jj bookmark set ${issueBookmark} -r <commit> -R ${repoCloneDir}\`. `
              : `Keep its added commit: \`jj bookmark set ${issueBookmark} -r ${kept} -R ${repoCloneDir}\`. `;
          const deletion = added.length > 1 ? "" : ", one side a deletion";
          // The added commits in either order: jj's, from the merge of the two operations.
          const refusals = [added, [...added].reverse()].map(
            (adds) =>
              `Bookmark ${issueBookmark} is conflicted (adds ${adds.join(", ")}; removes ${base})${deletion}; workspace ${workspaceDir} was not created. ` +
              keep +
              `Start from main instead: ${fromMain}, and the next provisioning starts at main.`
          );

          for (const attempt of [1, 2]) {
            calls.length = 0;
            expect(refusals, `${label}, attempt ${attempt}`).toContain(await refusal(deps));
            await rig.expectNothingRegistered(`${label}, attempt ${attempt}`);
          }

          if (wayOut === "keep") {
            await jj(["bookmark", "set", issueBookmark, "-r", kept, "-R", repoCloneDir]);
          } else {
            if (originHasIt) await rig.deleteRemoteBranch(issueBookmark);
            await jj(["bookmark", "delete", issueBookmark, "-R", repoCloneDir]);
          }
          await expect(provisionIssueWorkspace("WIDGETS-42", deps), label).resolves.toEqual({
            repoCloneDir,
            workspaceDir,
            bookmark: issueBookmark,
          });
          expect(await commitOf("@-", workspaceDir), label).toBe(
            wayOut === "keep" ? kept : await commitOf("main")
          );
        }
      }
    }, 120_000);
  }

  // `jj bookmark track` before the first push, or `remotes.origin.auto-track-bookmarks`, leaves
  // origin's row tracked with no commit beside the local bookmark, where the workspace starts.
  for (const tracking of ["jj bookmark track", "remotes.origin.auto-track-bookmarks"] as const) {
    test(`starts at a local bookmark tracked before its first push (${tracking})`, async () => {
      for (const { name, command } of JJ_BINARIES) {
        const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
        const { repoCloneDir, workspaceDir, jj, commitOf, deps } = rig;
        if (tracking === "remotes.origin.auto-track-bookmarks") {
          await jj([
            "config",
            "set",
            "--repo",
            "remotes.origin.auto-track-bookmarks",
            "glob:legion/*",
            "-R",
            repoCloneDir,
          ]);
        }
        const local = await commitOnMain(rig, "local");
        await jj(["bookmark", "create", issueBookmark, "-r", local], { cwd: repoCloneDir });
        if (tracking === "jj bookmark track") {
          await jj(["bookmark", "track", `${issueBookmark}@origin`], { cwd: repoCloneDir });
        }
        expect(
          (await jj(["bookmark", "list", "--all-remotes", issueBookmark], { cwd: repoCloneDir }))
            .stdout,
          name
        ).toContain("@origin (not created yet)");

        await expect(provisionIssueWorkspace("WIDGETS-42", deps), name).resolves.toEqual({
          repoCloneDir,
          workspaceDir,
          bookmark: issueBookmark,
        });
        expect(await commitOf("@-", workspaceDir), name).toBe(local);
      }
    }, 60_000);
  }

  test("refuses by name an origin row a racing fetch left conflicted, registering nothing, and adopts it once settled", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
      const { repoCloneDir, workspaceDir, jj, commitOf, deps } = rig;
      const moveTo = async (label: string) => {
        const commit = await commitOnMain(rig, label);
        await moveRemoteBranch(rig, commit);
        return commit;
      };
      // Origin has the branch at A, which the clone knows as an untracked row; origin then moves
      // it to B, and provisioning's fetch will see B.
      const a = await moveTo("a");
      await jj(["git", "fetch", "-R", repoCloneDir]);
      const beforeFetch = (
        await jj([
          "op",
          "log",
          "--no-graph",
          "-T",
          'id ++ "\n"',
          "--limit",
          "1",
          "-R",
          repoCloneDir,
        ])
      ).stdout.trim();
      const b = await moveTo("b");
      // While provisioning reads the bookmark, another jj process in the shared clone fetches from
      // the operation before provisioning's fetch, and sees origin at C.
      let c = "";
      const racing = {
        ...deps,
        run: async (cmd: string[], opts?: RunCall["opts"]) => {
          if (c === "" && cmd[1] === "git" && cmd[2] === "fetch") {
            const result = await deps.run(cmd, opts);
            c = await moveTo("c");
            await jj(["--at-op", beforeFetch, "git", "fetch", "-R", repoCloneDir]);
            return result;
          }
          return deps.run(cmd, opts);
        },
      };

      const refused = await refusal(racing);
      // The conflict's two targets, in the order jj merged the two fetches' operations.
      expect(
        [`${b}, ${c}`, `${c}, ${b}`].map(
          (adds) =>
            `Remote bookmark ${issueBookmark}@origin is conflicted (adds ${adds}; removes ${a}), which concurrent fetches leave; workspace ${workspaceDir} was not created. Provision again: the next provisioning's fetch sets the row to origin's branch as it is then.`
        ),
        name
      ).toContain(refused);
      await rig.expectNothingRegistered(name);

      // The refusal's way out, followed: provision again. The fetch settles the row at origin's C.
      await expect(provisionIssueWorkspace("WIDGETS-42", deps), name).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark: issueBookmark,
      });
      expect(await commitOf("@-", workspaceDir), name).toBe(c);
    }
  }, 60_000);

  test("refuses an origin row a fetch moved while provisioning tracked it, and adopts it next time", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
      const { repoCloneDir, workspaceDir, jj, commitOf, deps } = rig;
      const listed = await pushIssueBranch(rig);
      await jj(["bookmark", "forget", issueBookmark], { cwd: repoCloneDir });
      // Between provisioning's read and its `jj bookmark track`, another jj process in the shared
      // clone fetches origin's branch, which has moved on.
      let newer = "";
      const racing = {
        ...deps,
        run: async (cmd: string[], opts?: RunCall["opts"]) => {
          if (newer === "" && cmd[1] === "bookmark" && cmd[2] === "track") {
            newer = await commitOnMain(rig, "newer");
            await moveRemoteBranch(rig, newer);
            await jj(["git", "fetch", "-R", repoCloneDir]);
          }
          return deps.run(cmd, opts);
        },
      };

      expect(await refusal(racing), name).toBe(
        `Bookmark ${issueBookmark}@origin moved from ${listed} to ${newer} while it was being tracked; workspace ${workspaceDir} was not created. Provision again: the next provisioning starts at origin's branch as it is then.`
      );
      await rig.expectNothingRegistered(name);

      // The refusal's way out, followed: provision again, which starts at the moved commit.
      await expect(provisionIssueWorkspace("WIDGETS-42", deps), name).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark: issueBookmark,
      });
      expect(await commitOf("@-", workspaceDir), name).toBe(newer);
    }
  }, 60_000);

  test("starts a merged issue's workspace at main when GitHub deleted its branch, and brings none of the branch back", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const rig = await realJjRig(command, path.join(await temporaryDirectory(), "state"));
      const { repoCloneDir, workspaceDir, jj, commitOf, deps } = rig;
      await pushIssueBranch(rig);
      await jj(["new", "main"], { cwd: repoCloneDir });
      // The pull request merged and GitHub deleted the branch: provisioning's fetch drops the local
      // bookmark that still matched it, and no origin row is left (LEGION-28, LEGION-84).
      await rig.deleteRemoteBranch(issueBookmark);

      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark: issueBookmark,
      });
      expect(await commitOf("@-", workspaceDir), name).toBe(await commitOf("main"));
      expect(existsSync(path.join(workspaceDir, "fixture.txt")), name).toBeFalse();
      expect(await commitOf(issueBookmark), name).toBe(await commitOf("@", workspaceDir));
    }
  }, 60_000);

  test("refuses to create a workspace on a conflicted bookmark and leaves no directory behind, on consecutive attempts", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps, expectNothingRegistered } =
        await realJjRig(command, stateDir);
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      await jj(["new", "-m", "later work"], { cwd: workspaceDir });
      const base = await commitOf(bookmark);
      // Two moves of the bookmark from the same operation — one on the main operation line, one
      // `--at-op` the operation before it — which the next command reconciles into a conflict.
      const baseOperation = (
        await jj(
          [
            "op",
            "log",
            "--no-graph",
            "-T",
            'id.short() ++ "\n"',
            "--limit",
            "1",
            "-R",
            repoCloneDir,
          ],
          { cwd: repoCloneDir }
        )
      ).stdout.trim();
      await jj(["bookmark", "set", bookmark, "-r", "@"], { cwd: workspaceDir });
      await jj(
        ["--at-op", baseOperation, "bookmark", "set", bookmark, "-r", "main", "--allow-backwards"],
        { cwd: repoCloneDir }
      );
      await jj(["workspace", "forget", "widgets-42", "-R", repoCloneDir]);
      await rm(workspaceDir, { recursive: true, force: true });
      const listed = (await jj(["bookmark", "list", bookmark], { cwd: repoCloneDir })).stdout;
      expect(listed, name).toContain("(conflicted)");
      const conflictedTargets = (
        await jj([
          "log",
          "-r",
          `bookmarks(exact:${bookmark})`,
          "--no-graph",
          "-T",
          'commit_id ++ "\n"',
          "-R",
          repoCloneDir,
        ])
      ).stdout
        .split("\n")
        .filter((line) => line !== "");
      expect(conflictedTargets, name).toHaveLength(2);

      for (const attempt of [1, 2]) {
        calls.length = 0;
        const refusal = await provisionIssueWorkspace("WIDGETS-42", deps).then(
          () => "provisioning resolved",
          (error: Error) => error.message
        );
        // Both moves are the conflict's adds, in the order jj merged the two operations; the
        // bookmark's commit before them is what it removes.
        expect(refusal, `${name}, attempt ${attempt}`).toStartWith(
          `Bookmark ${bookmark} is conflicted (adds `
        );
        for (const target of conflictedTargets) {
          expect(refusal, `${name}, attempt ${attempt}`).toContain(target);
        }
        expect(refusal, `${name}, attempt ${attempt}`).toEndWith(
          `; removes ${base}); workspace ${workspaceDir} was not created. ` +
            `Keep one of its added commits: \`jj bookmark set ${bookmark} -r <commit> -R ${repoCloneDir}\`. ` +
            `Start from main instead: \`jj bookmark delete ${bookmark} -R ${repoCloneDir}\`, and the next provisioning starts at main.`
        );
        // The resolution is the last command (the settings read — already `false` from the first
        // provisioning, so no write — and the fetch precede it): nothing was added, so nothing
        // exists or is registered for the next resume to adopt.
        expect(
          calls.map((cmd) => cmd[1]),
          `${name}, attempt ${attempt}`
        ).toEqual(["config", "git", "bookmark"]);
        await expectNothingRegistered(`${name}, attempt ${attempt}`);
      }
    }
  }, 60_000);

  test("re-adds a forgotten issue workspace at its bookmark's commit when the bookmark resolves, writing no bookmark", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const gitDir = path.join(repoCloneDir, ".git");
    const commit = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d";
    const calls: RunCall[] = [];
    let workspaceAddAttempts = 0;
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: unknown[][];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          isolateGitConfig: false,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "bookmark" && cmd[2] === "list") {
              return { exitCode: 0, stdout: `local|1|0|0|${commit}|\n`, stderr: "" };
            }
            if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
              workspaceAddAttempts += 1;
              if (workspaceAddAttempts === 1) {
                return {
                  exitCode: 1,
                  stdout: "",
                  stderr: "Workspace named 'widgets-42' already exists",
                };
              }
              await mkdir(workspaceDir, { recursive: true });
            }
            return { exitCode: 0, stdout: "", stderr: "" };
          },
        })
      ).resolves.toEqual({ repoCloneDir, workspaceDir, bookmark: "legion/WIDGETS-42" });
      logged = errorSpy.mock.calls;
    } finally {
      errorSpy.mockRestore();
    }

    const fetch = calls[2];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      bookmarkRowsCommand("legion/WIDGETS-42", repoCloneDir),
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", commit, repoCloneDir),
      ["jj", "workspace", "forget", "widgets-42", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", commit, repoCloneDir),
      ...credentialConfigCommands(gitDir, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    expect(logged).toEqual([]);
  });

  test("re-adds a forgotten issue workspace at main and creates the bookmark when the bookmark is gone, logging it once", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const gitDir = path.join(repoCloneDir, ".git");
    const calls: RunCall[] = [];
    let workspaceAddAttempts = 0;
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: string[];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          isolateGitConfig: false,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
              workspaceAddAttempts += 1;
              if (workspaceAddAttempts === 1) {
                return {
                  exitCode: 1,
                  stdout: "",
                  stderr: "Workspace named 'widgets-42' already exists",
                };
              }
              await mkdir(workspaceDir, { recursive: true });
            }
            // The resolution too: jj 0.44 and 0.45 print nothing for a bookmark that is gone.
            return { exitCode: 0, stdout: "", stderr: "" };
          },
        })
      ).resolves.toEqual({ repoCloneDir, workspaceDir, bookmark: "legion/WIDGETS-42" });
      logged = errorSpy.mock.calls.map((call) => String(call[0]));
    } finally {
      errorSpy.mockRestore();
    }

    expect(calls.map((call) => call.cmd)).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      bookmarkRowsCommand("legion/WIDGETS-42", repoCloneDir),
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", "main", repoCloneDir),
      ["jj", "workspace", "forget", "widgets-42", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      workspaceAddCommand(workspaceDir, "widgets-42", "main", repoCloneDir),
      ["jj", "bookmark", "set", "legion/WIDGETS-42", "-r", "@"],
      ...credentialConfigCommands(gitDir, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    const bookmarkSet = calls.find((call) => call.cmd[1] === "bookmark" && call.cmd[2] === "set");
    expect(bookmarkSet?.opts?.cwd).toBe(workspaceDir);
    expect(logged).toHaveLength(1);
    expect(logged[0]).toContain("legion/WIDGETS-42 is gone");
  });

  test("refuses to create a workspace on a conflicted bookmark, before any workspace add", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const commits = [
      "19508b7be08e4c3a2b1d0f9e8d7c6b5a49382716",
      "4e4bde478a1d5f6e7a8b9c0d1e2f3a4b5c6d7e8f",
    ];
    const base = "7c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d";
    const calls: RunCall[] = [];
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd, opts) => {
          calls.push({ cmd, opts });
          if (cmd[0] === "jj" && cmd[1] === "bookmark" && cmd[2] === "list") {
            return {
              exitCode: 0,
              stdout: `local|1|1|0|${commits[0]},${commits[1]}|${base}\n`,
              stderr: "",
            };
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      })
    ).rejects.toThrow(
      `Bookmark legion/WIDGETS-42 is conflicted (adds ${commits[0]}, ${commits[1]}; removes ${base}); workspace ${workspaceDir} was not created. ` +
        `Keep one of its added commits: \`jj bookmark set legion/WIDGETS-42 -r <commit> -R ${repoCloneDir}\`. ` +
        `Start from main instead: \`jj bookmark delete legion/WIDGETS-42 -R ${repoCloneDir}\`, and the next provisioning starts at main.`
    );
    // The resolution is the last command: no prune, no add, nothing registered for the next
    // resume to adopt.
    expect(calls.map((call) => call.cmd)).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
      ["jj", "git", "fetch", "-R", repoCloneDir],
      bookmarkRowsCommand("legion/WIDGETS-42", repoCloneDir),
    ]);
    expect(existsSync(workspaceDir)).toBeFalse();
  });

  test("fails provisioning when the bookmark read fails or prints a row it cannot print, before any workspace add", async () => {
    const commit = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d";
    const other = "19508b7be08e4c3a2b1d0f9e8d7c6b5a49382716";
    const shape = (row: string) => (command: string, workspaceDir: string) =>
      `Bookmark legion/WIDGETS-42's row ${JSON.stringify(row)} is not the shape ${command} prints; workspace ${workspaceDir} was not created.`;
    for (const { name, result, refusal } of [
      {
        name: "read fails",
        result: { exitCode: 1, stdout: "", stderr: "Error: Failed to read the operation log" },
        refusal: (command: string, workspaceDir: string) =>
          `Bookmark legion/WIDGETS-42 could not be resolved; workspace ${workspaceDir} was not created.\nCommand failed (exit 1): ${command}\nError: Failed to read the operation log`,
      },
      // jj prints a template's evaluation error in place and exits 0.
      ...[
        "origin|1|0|0|<Error: No Commit available>|",
        `origin|1|0|2|${commit}|`,
        `local|1|0|0|${commit},${other}|`,
      ].map((row) => ({
        name: row,
        result: { exitCode: 0, stdout: `${row}\n`, stderr: "" },
        refusal: shape(row),
      })),
      {
        name: "a second row for the same place",
        result: {
          exitCode: 0,
          stdout: `local|1|0|0|${commit}|\nlocal|1|0|0|${other}|\n`,
          stderr: "",
        },
        refusal: shape(`local|1|0|0|${other}|`),
      },
    ]) {
      const stateDir = await temporaryDirectory();
      const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
      const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
      const calls: RunCall[] = [];
      await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

      await expect(
        provisionIssueWorkspace("WIDGETS-42", {
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          isolateGitConfig: false,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "bookmark" && cmd[2] === "list") return result;
            return { exitCode: 0, stdout: "", stderr: "" };
          },
        }),
        name
      ).rejects.toThrow(
        refusal(bookmarkRowsCommand("legion/WIDGETS-42", repoCloneDir).join(" "), workspaceDir)
      );
      // Nothing is guessed from a failed read: no prune, no add, no bookmark write.
      expect(
        calls.map((call) => call.cmd),
        name
      ).toEqual([
        readKeepUnreachableCommitsCommand(repoCloneDir),
        writeKeepUnreachableCommitsCommand(repoCloneDir),
        ["jj", "git", "fetch", "-R", repoCloneDir],
        bookmarkRowsCommand("legion/WIDGETS-42", repoCloneDir),
      ]);
      expect(existsSync(workspaceDir), name).toBeFalse();
    }
  });

  test("reports a clone killed at its budget as a timeout, leaves nothing at the final path, and clones fresh on the next attempt", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const cloneTargets: string[] = [];
    let clones = 0;
    const deps = {
      stateDir,
      repo: "acme/widgets" as const,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
      isolateGitConfig: false,
      run: async (cmd: string[]) => {
        if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
          const target = cmd[4];
          if (!target) throw new Error("clone is missing its destination");
          cloneTargets.push(target);
          clones += 1;
          if (clones === 1) {
            // The runner killed the clone mid-way: a half-written `.jj` is left in the target.
            await mkdir(path.join(target, ".jj"), { recursive: true });
            await writeFile(path.join(target, ".jj", "partial"), "half", "utf8");
            return {
              exitCode: 143,
              stdout: "",
              stderr: "",
              timedOut: { limitMs: 300_000, elapsedMs: 300_400 },
            };
          }
          await mkdir(path.join(target, ".jj"), { recursive: true });
        }
        if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
          await mkdir(workspaceDir, { recursive: true });
        }
        return { exitCode: 0, stdout: "", stderr: "" };
      },
    };

    await expect(provisionIssueWorkspace(issue, deps)).rejects.toThrow(
      `Command timed out after 300 s (ran 300.4 s): jj git clone https://github.com/acme/widgets `
    );
    expect(existsSync(repoCloneDir)).toBeFalse();
    expect(
      (await readdir(path.dirname(repoCloneDir))).filter((entry) => entry.startsWith("widgets"))
    ).toEqual([]);

    await expect(provisionIssueWorkspace(issue, deps)).resolves.toMatchObject({ repoCloneDir });
    expect(clones).toBe(2);
    expect(cloneTargets[1]).not.toBe(cloneTargets[0]);
    expect(existsSync(path.join(repoCloneDir, ".jj"))).toBeTrue();
    expect(existsSync(path.join(repoCloneDir, ".jj", "partial"))).toBeFalse();
  });

  test("reports a clone the runner killed on the caller's abort as aborted, not as a plain exit-143 failure, and leaves nothing at the final path", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");

    await expect(
      provisionIssueWorkspace(issue, {
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) =>
          cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone"
            ? { exitCode: 143, stdout: "", stderr: "", aborted: true }
            : { exitCode: 0, stdout: "", stderr: "" },
      })
    ).rejects.toThrow(/^Command aborted: jj git clone https:\/\/github\.com\/acme\/widgets /);
    expect(existsSync(repoCloneDir)).toBeFalse();
    expect(
      (await readdir(path.dirname(repoCloneDir))).filter((entry) => entry.startsWith("widgets"))
    ).toEqual([]);
  });

  // The cleanup failure is made real with an unreadable (mode 000) directory inside the clone,
  // which root ignores — as root the test would pass for the wrong reason, so it is skipped.
  test.skipIf(process.getuid?.() === 0)(
    "a failed clone whose temporary directory cannot be removed still reports the clone failure, logging the cleanup failure",
    async () => {
      const stateDir = await temporaryDirectory();
      const issue = "WIDGETS-42";
      const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
      let lockedDir: string | undefined;
      const errorSpy = spyOn(console, "error").mockImplementation(() => {});
      let logged: string[];
      try {
        await expect(
          provisionIssueWorkspace(issue, {
            stateDir,
            repo: "acme/widgets",
            provisioningToken: async () => "installation-token",
            credentialHelper,
            commandTimeoutMs,
            isolateGitConfig: false,
            run: async (cmd) => {
              if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
                const target = cmd[4];
                if (!target) throw new Error("clone is missing its destination");
                // The killed clone left an unreadable subtree behind: `rm` of the temp dir fails.
                lockedDir = path.join(target, "locked");
                await mkdir(path.join(lockedDir, "inner"), { recursive: true });
                await writeFile(path.join(lockedDir, "inner", "f"), "x", "utf8");
                await chmod(lockedDir, 0o000);
                return {
                  exitCode: 143,
                  stdout: "",
                  stderr: "",
                  timedOut: { limitMs: 300_000, elapsedMs: 300_400 },
                };
              }
              return { exitCode: 0, stdout: "", stderr: "" };
            },
          })
        ).rejects.toThrow("Command timed out after 300 s (ran 300.4 s): jj git clone");
        logged = errorSpy.mock.calls.map((call) => String(call[0]));
      } finally {
        errorSpy.mockRestore();
        if (lockedDir) await chmod(lockedDir, 0o755);
      }
      expect(existsSync(repoCloneDir)).toBeFalse();
      expect(logged).toContainEqual(expect.stringContaining("failed to remove temporary clone"));
    }
  );

  test("removes an incomplete clone that has no .jj and clones again, logging it", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(repoCloneDir, { recursive: true });
    await writeFile(path.join(repoCloneDir, "leftover"), "from an older daemon", "utf8");
    const calls: string[][] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: string[];
    try {
      await provisionIssueWorkspace(issue, {
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) => {
          calls.push(cmd);
          if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
            const target = cmd[4];
            if (!target) throw new Error("clone is missing its destination");
            await mkdir(path.join(target, ".jj"), { recursive: true });
          }
          if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
            await mkdir(workspaceDir, { recursive: true });
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      });
      logged = errorSpy.mock.calls.map((call) => String(call[0]));
    } finally {
      errorSpy.mockRestore();
    }

    expect(
      calls.some((cmd) => cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone")
    ).toBeTrue();
    expect(logged).toContainEqual(
      expect.stringContaining(`removing incomplete clone at ${repoCloneDir} (no .jj)`)
    );
    expect(existsSync(path.join(repoCloneDir, ".jj"))).toBeTrue();
    expect(existsSync(path.join(repoCloneDir, "leftover"))).toBeFalse();
  });

  test("still reports a command that failed on its own as a plain failure with its stderr", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) =>
          cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch"
            ? { exitCode: 1, stdout: "", stderr: "fatal: x" }
            : { exitCode: 0, stdout: "", stderr: "" },
      })
    ).rejects.toThrow(`Command failed (exit 1): jj git fetch -R ${repoCloneDir}\nfatal: x`);
  });

  test("stops before the fetch when the settings write fails", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const calls: string[][] = [];
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        isolateGitConfig: false,
        run: async (cmd) => {
          calls.push(cmd);
          // The read answers jj's default; the write that follows cannot land.
          if (cmd[0] === "jj" && cmd[1] === "config" && cmd[2] === "get") {
            return { exitCode: 0, stdout: "true\n", stderr: "" };
          }
          return cmd[0] === "jj" && cmd[1] === "config" && cmd[2] === "set"
            ? { exitCode: 1, stdout: "", stderr: "Config error: cannot write" }
            : { exitCode: 0, stdout: "", stderr: "" };
        },
      })
    ).rejects.toThrow(
      `Command failed (exit 1): ${writeKeepUnreachableCommitsCommand(repoCloneDir).join(" ")}\nConfig error: cannot write`
    );
    // The fetch is the destructive step: nothing ran after the failed write.
    expect(calls).toEqual([
      readKeepUnreachableCommitsCommand(repoCloneDir),
      writeKeepUnreachableCommitsCommand(repoCloneDir),
    ]);
  });

  test("removes a finished issue's workspace: its unreachable commits leave the other workspace's log, jj and git forget it, the directory is gone, a second removal is a no-op, and the next provisioning starts fresh at main", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps, deleteRemoteBranch } =
        await realJjRig(command, stateDir);
      const workspaceB = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-43");
      const bookmark = "legion/WIDGETS-42";
      // Provisioning itself writes `git.abandon-unreachable-commits = false` before every fetch
      // (LEGION-84), so the fetch below that deletes the merged branch's bookmark keeps A's
      // commits — the removal, not the fetch, is what makes them leave B's log.
      await provisionIssueWorkspace("WIDGETS-42", deps);
      await provisionIssueWorkspace("WIDGETS-43", deps);
      // A's work: a described commit the bookmark follows (it was created on the unsnapshotted
      // `@`, so the snapshot moves it), then an empty working copy above it — the shape a worker
      // leaves. B commits its own file.
      await writeFile(path.join(workspaceDir, "a.txt"), "a\n", "utf8");
      await jj(["describe", "-m", "a work"], { cwd: workspaceDir });
      await jj(["new"], { cwd: workspaceDir });
      await writeFile(path.join(workspaceB, "b.txt"), "b\n", "utf8");
      await jj(["describe", "-m", "b work"], { cwd: workspaceB });
      const aWork = await commitOf(bookmark);
      const bWork = await commitOf("@", workspaceB);
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      // GitHub deletes the branch when the pull request merges; the production-check resume's
      // fetch (a provisioning) then deletes the local bookmark.
      await deleteRemoteBranch(bookmark);
      await provisionIssueWorkspace("WIDGETS-42", deps);
      expect((await jj(["bookmark", "list", bookmark], { cwd: repoCloneDir })).stdout, name).toBe(
        ""
      );
      const aWorkingCopy = await commitOf("@", workspaceDir);
      const logB = async () =>
        (
          await jj(["log", "-r", "all()", "--no-graph", "-T", 'commit_id ++ "\n"'], {
            cwd: workspaceB,
          })
        ).stdout.split("\n");
      const workspaceNames = async () =>
        (await jj(workspaceListCommand(repoCloneDir).slice(1))).stdout.split("\n");
      // Today's leftover, and the precondition for the assertion below: A's commits are visible
      // heads in B's log (`all()` is a superset of the default log revset).
      expect(await logB(), name).toEqual(expect.arrayContaining([aWork, aWorkingCopy, bWork]));
      expect(await workspaceNames(), name).toContain("widgets-42");

      calls.length = 0;
      await expect(removeIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        workspaceDir,
        removed: true,
        abandoned: [aWorkingCopy, aWork],
      });
      expect(calls, name).toEqual([
        workspaceListCommand(repoCloneDir),
        [
          "jj",
          "log",
          "-r",
          ownCommitsRevset("widgets-42"),
          "--no-graph",
          "-T",
          'commit_id ++ "\n"',
          "--ignore-working-copy",
          "-R",
          repoCloneDir,
        ],
        [
          "jj",
          "abandon",
          "-r",
          `${aWorkingCopy} | ${aWork}`,
          "--ignore-working-copy",
          "-R",
          repoCloneDir,
        ],
        ["jj", "workspace", "forget", "widgets-42", "--ignore-working-copy", "-R", repoCloneDir],
        ["git", `--git-dir=${path.join(repoCloneDir, ".git")}`, "worktree", "prune"],
      ]);
      expect(existsSync(workspaceDir), name).toBeFalse();
      expect(await workspaceNames(), name).not.toContain("widgets-42");
      expect(await workspaceNames(), name).toContain("widgets-43");
      expect(await gitWorktrees(repoCloneDir), name).not.toContain(workspaceDir);
      const afterRemoval = await logB();
      expect(afterRemoval, name).not.toContain(aWork);
      expect(afterRemoval, name).not.toContain(aWorkingCopy);
      expect(afterRemoval, name).toContain(bWork);
      expect(existsSync(path.join(workspaceB, "b.txt")), name).toBeTrue();

      // Idempotent: nothing registered, no directory — the registration check and nothing else.
      calls.length = 0;
      await expect(removeIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        workspaceDir,
        removed: false,
        abandoned: [],
      });
      expect(calls, name).toEqual([workspaceListCommand(repoCloneDir)]);

      // A worker resumed on the issue gets the brand-new-issue shape: a workspace at `main` with
      // the bookmark created on its fresh working copy (LEGION-70), nothing of the old one.
      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      expect(calls, name).toContainEqual(["jj", "bookmark", "set", bookmark, "-r", "@"]);
      // Snapshot before reading `@`: the bookmark sits on the unsnapshotted working copy and
      // moves with its first snapshot.
      await jj(["status"], { cwd: workspaceDir });
      expect(await commitOf("@-", workspaceDir), name).toBe(await commitOf("main"));
      expect(await commitOf(bookmark), name).toBe(await commitOf("@", workspaceDir));
      expect(existsSync(path.join(workspaceDir, "a.txt")), name).toBeFalse();
    }
  }, 60_000);

  test("keeps pushed work when removing a workspace whose bookmark still exists, and the next provisioning re-creates it on that bookmark", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = await realJjRig(
        command,
        stateDir
      );
      const workspaceB = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-43");
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      await provisionIssueWorkspace("WIDGETS-43", deps);
      await writeFile(path.join(workspaceDir, "a.txt"), "a\n", "utf8");
      await jj(["describe", "-m", "a pushed work"], { cwd: workspaceDir });
      await jj(["new"], { cwd: workspaceDir });
      await jj(["status"], { cwd: workspaceB });
      const aWork = await commitOf(bookmark);
      const aWorkingCopy = await commitOf("@", workspaceDir);
      // A closed-but-unmerged issue: its branch is still on GitHub, so the bookmark survives.
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );

      calls.length = 0;
      await expect(removeIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        workspaceDir,
        removed: true,
        abandoned: [aWorkingCopy],
      });
      expect(existsSync(workspaceDir), name).toBeFalse();
      expect(
        (await jj(workspaceListCommand(repoCloneDir).slice(1))).stdout.split("\n"),
        name
      ).not.toContain("widgets-42");
      // The pushed commit is kept, still under its bookmark, still in B's log.
      expect(await commitOf(bookmark), name).toBe(aWork);
      const logB = (
        await jj(["log", "-r", "all()", "--no-graph", "-T", 'commit_id ++ "\n"'], {
          cwd: workspaceB,
        })
      ).stdout.split("\n");
      expect(logB, name).toContain(aWork);
      expect(logB, name).not.toContain(aWorkingCopy);

      // Resumed later: re-created on top of the surviving bookmark, no bookmark write.
      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toMatchObject({
        workspaceDir,
      });
      expect(
        calls.some((cmd) => cmd[1] === "bookmark" && cmd[2] !== "list"),
        name
      ).toBeFalse();
      expect(await commitOf("@-", workspaceDir), name).toBe(aWork);
      expect(await commitOf(bookmark), name).toBe(aWork);
    }
  }, 60_000);

  test("repairs the shape a crash between the directory deletion and the forget leaves: the next provisioning forgets, prunes, and re-adds the still-registered workspace", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, commitOf, deps } = await realJjRig(
        command,
        stateDir
      );
      const bookmark = "legion/WIDGETS-42";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      await jj(["status"], { cwd: workspaceDir });
      const bookmarkCommit = await commitOf(bookmark);
      // Removal deletes the directory first, then forgets; a crash in between leaves this.
      await rm(workspaceDir, { recursive: true, force: true });
      expect(
        (await jj(workspaceListCommand(repoCloneDir).slice(1))).stdout.split("\n"),
        name
      ).toContain("widgets-42");

      calls.length = 0;
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toEqual({
        repoCloneDir,
        workspaceDir,
        bookmark,
      });
      // `createWorkspace`'s `already registered|exists` branch on a real binary: forget, prune,
      // add again at the bookmark's commit (the bookmark survived, so no bookmark write).
      expect(calls, name).toContainEqual([
        "jj",
        "workspace",
        "forget",
        "widgets-42",
        "-R",
        repoCloneDir,
      ]);
      expect(
        calls.some((cmd) => cmd[1] === "bookmark" && cmd[2] !== "list"),
        name
      ).toBeFalse();
      expect(existsSync(workspaceDir), name).toBeTrue();
      expect(await commitOf("@-", workspaceDir), name).toBe(bookmarkCommit);
    }
  }, 60_000);
});
