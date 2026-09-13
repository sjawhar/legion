import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { existsSync, watch } from "node:fs";
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { provisionIssueWorkspace, type RunResult, type WorkspaceSpec } from "./workspace";

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
const extensionPackage = path.resolve(import.meta.dir, "../../pi-envoy");

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
/** The command `createWorkspace` runs before anything else: the bookmark's local targets, one
 * commit id per line — none, one, or (conflicted) several. */
function resolveBookmarkCommand(bookmark: string, repoCloneDir: string): string[] {
  return [
    "jj",
    "log",
    "-r",
    `bookmarks(exact:${bookmark})`,
    "--no-graph",
    "-T",
    'commit_id ++ "\n"',
    "--ignore-working-copy",
    "-R",
    repoCloneDir,
  ];
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
    extensionPackage,
    repo: "acme/widgets" as const,
    stateDir,
    provisioningToken: async () => "installation-token",
    credentialHelper,
    commandTimeoutMs,
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
  return { repoCloneDir, workspaceDir, remoteDir, calls, jj, commitOf, deps };
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
    process.env.LEGION_MAX_RECURSION_DEPTH = "11";
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let spec: WorkspaceSpec;
    let logged: unknown[][];
    try {
      spec = await provisionIssueWorkspace(issue, {
        extensionPackage,
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
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

    const [clone, fetch] = calls;
    if (!clone || !fetch) throw new Error("Provisioning did not clone and fetch the repository");
    const cloneEnv = provisioningEnv(clone);
    expect(provisioningEnv(fetch)).toEqual(cloneEnv);
    expect(existsSync(cloneEnv.GIT_ASKPASS)).toBeFalse();
    expect(calls.map((call) => call.cmd)).toEqual([
      [
        "jj",
        "git",
        "clone",
        "https://github.com/acme/widgets",
        expect.stringMatching(new RegExp(`^${escapeRegExp(repoCloneDir)}\\.clone-`)),
      ],
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand(bookmark, repoCloneDir),
      ["git", `--git-dir=${repoCloneDir}/.git`, "worktree", "prune"],
      [
        "jj",
        "workspace",
        "add",
        workspaceDir,
        "--name",
        "widgets-42",
        "--revision",
        "main",
        "-R",
        repoCloneDir,
      ],
      ["jj", "bookmark", "set", bookmark, "-r", "@"],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    // The bookmark is created in the new workspace, on its own working copy — and a brand-new
    // issue has no bookmark to miss, so nothing is logged.
    const bookmarkSet = calls.find((call) => call.cmd[1] === "bookmark");
    expect(bookmarkSet?.opts?.cwd).toBe(workspaceDir);
    expect(logged).toEqual([]);
    // Every provisioning command runs under the slow budget, not the runner's generic default.
    expect(calls.map((call) => call.opts?.timeoutMs)).toEqual(calls.map(() => commandTimeoutMs));
    // Provisioning reads the clone's jj config (the identity probes) and never sets a key in it.
    expect(
      calls.some(({ cmd }) => cmd[0] === "jj" && cmd[1] === "config" && cmd[2] === "set")
    ).toBeFalse();
    expect(existsSync(path.join(repoCloneDir, ".jj"))).toBeTrue();
    expect(
      (await readdir(path.dirname(repoCloneDir))).filter((entry) => entry.startsWith("widgets."))
    ).toEqual([]);
    expect(await readFile(path.join(workspaceDir, ".omp", "config.yml"), "utf8")).toBe("");
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
    process.env.LEGION_MAX_RECURSION_DEPTH = "11";

    const deps = {
      extensionPackage,
      repo: "acme/widgets" as const,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
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

  test("creates a workspace on top of a resolving bookmark and runs no bookmark command", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const bookmark = "legion/WIDGETS-42";
    const commit = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d";
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: unknown[][];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          extensionPackage,
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "log") {
              return { exitCode: 0, stdout: `${commit}\n`, stderr: "" };
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
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand(bookmark, repoCloneDir),
      ["git", `--git-dir=${repoCloneDir}/.git`, "worktree", "prune"],
      [
        "jj",
        "workspace",
        "add",
        workspaceDir,
        "--name",
        "widgets-42",
        "--revision",
        commit,
        "-R",
        repoCloneDir,
      ],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
    expect(calls.some((call) => call.cmd[1] === "bookmark")).toBeFalse();
    expect(logged).toEqual([]);
  });

  test("creates a missing workspace parent before adding an issue workspace", async () => {
    const stateDir = path.join(await temporaryDirectory(), "state");
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
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
    // What a pre-LEGION-44 worker boot left behind on the shared clone.
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
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
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
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";

    await provisionIssueWorkspace(issue, {
      extensionPackage,
      repo: "acme/widgets",
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper: pinnedHelper,
      commandTimeoutMs,
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

  test("does not add a second workspace or run a bookmark command when an issue is reactivated", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    const deps = {
      extensionPackage,
      repo: "acme/widgets" as const,
      stateDir,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
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
    const fetch = calls[1];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "workspace", "update-stale"],
      ["jj", "git", "fetch", "-R", repoCloneDir],
      ...credentialConfigCommands(`${repoCloneDir}/.git`, credentialHelper),
      ...identityProbeCommands(repoCloneDir),
    ]);
  });

  test("removes a repository-scoped jj identity an earlier worker boot left on the shared clone, logging each key", async () => {
    // Before LEGION-44 the extension ran `jj config set --repo user.name`/`user.email` at every
    // worker boot. `--repo` on a workspace writes the one config file every workspace of the clone
    // shares, so the last worker to boot — in any tree — set the author and committer for every
    // other tree's commits. Identity rides each pane's environment now; a leftover value is removed
    // here, where the clone's config writes already live, and nothing writes it again.
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
        extensionPackage,
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
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

    expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
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
        extensionPackage,
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
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
    expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
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
          extensionPackage,
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
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
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      // The worker's first jj command snapshots the `.omp/config.yml` provisioning wrote, moving the
      // bookmark with `@` on the main operation line. Without it, the second provision's
      // `update-stale` snapshots on a divergent operation (jj loads the repo at the workspace's last
      // recorded operation), moves the bookmark there too, and the reconciliation conflicts it with
      // the `sibling-advance` move below — a jj behaviour, not a bookmark this code touched.
      await jj(["status"], { cwd: workspaceDir });
      await jj([
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

  test("leaves the bookmark missing after its merged pull request's branch is deleted", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, remoteDir, calls, jj, deps } = await realJjRig(
        command,
        stateDir
      );
      const bookmark = "legion/WIDGETS-42";
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      // Snapshot the `.omp/config.yml` provisioning wrote, so the commit pushed below is the
      // bookmark's final target: jj deletes a local bookmark on fetch only while it still points
      // where the deleted remote branch did.
      await jj(["status"], { cwd: workspaceDir });
      await jj(
        ["git", "push", "--remote", "origin", "--bookmark", bookmark, "--allow-empty-description"],
        { cwd: repoCloneDir }
      );
      // GitHub deletes the branch when the pull request merges.
      const deleted = await runCommand([
        SYSTEM_GIT,
        `--git-dir=${remoteDir}/.git`,
        "branch",
        "-D",
        bookmark,
      ]);
      expect(deleted.exitCode, `${name}: ${deleted.stderr}`).toBe(0);

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
      expect((await jj(["bookmark", "list", bookmark], { cwd: repoCloneDir })).stdout, name).toBe(
        ""
      );

      // LEGION-28 was re-prompted twice after its merge: the next resume finds the bookmark still
      // gone and, through its own `update-stale`, a usable working copy — the fetch above abandoned
      // the merged branch's commits and left the workspace stale until then.
      await expect(provisionIssueWorkspace("WIDGETS-42", deps)).resolves.toMatchObject({
        workspaceDir,
      });
      expect((await jj(["bookmark", "list", bookmark], { cwd: repoCloneDir })).stdout, name).toBe(
        ""
      );
      await jj(["status"], { cwd: workspaceDir });
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
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

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
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

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
        calls.some((cmd) => cmd[1] === "bookmark"),
        name
      ).toBeFalse();
      // Before this change the fresh path started at `main` and the `bookmark set -r @` that
      // followed refused to move the surviving bookmark sideways.
      expect(await commitOf("@-", workspaceDir), name).toBe(bookmarkCommit);
      expect(await commitOf(bookmark), name).toBe(bookmarkCommit);
      expect(await pointOperations(), name).toBe(1);
    }
  }, 60_000);

  test("refuses to create a workspace on a conflicted bookmark and leaves no directory behind, on consecutive attempts", async () => {
    for (const { name, command } of JJ_BINARIES) {
      const stateDir = path.join(await temporaryDirectory(), "state");
      const { repoCloneDir, workspaceDir, calls, jj, deps } = await realJjRig(command, stateDir);
      const bookmark = "legion/WIDGETS-42";
      process.env.LEGION_MAX_RECURSION_DEPTH = "8";

      await provisionIssueWorkspace("WIDGETS-42", deps);
      await jj(["new", "-m", "later work"], { cwd: workspaceDir });
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
        await jj(resolveBookmarkCommand(bookmark, repoCloneDir).slice(1))
      ).stdout
        .split("\n")
        .filter((line) => line !== "");
      expect(conflictedTargets, name).toHaveLength(2);

      for (const attempt of [1, 2]) {
        calls.length = 0;
        await expect(
          provisionIssueWorkspace("WIDGETS-42", deps),
          `${name}, attempt ${attempt}`
        ).rejects.toThrow(`Bookmark ${bookmark} is conflicted (${conflictedTargets.join(", ")})`);
        // The resolution is the last command: nothing was added, so nothing exists or is
        // registered for the next resume to adopt.
        expect(
          calls.map((cmd) => cmd[1]),
          `${name}, attempt ${attempt}`
        ).toEqual(["git", "log"]);
        expect(existsSync(workspaceDir), `${name}, attempt ${attempt}`).toBeFalse();
        expect(
          (await jj(["workspace", "list", "-R", repoCloneDir])).stdout,
          `${name}, attempt ${attempt}`
        ).not.toContain("widgets-42:");
      }
    }
  }, 60_000);

  test("re-adds a forgotten issue workspace at its bookmark's commit when the bookmark resolves, running no bookmark command", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const gitDir = path.join(repoCloneDir, ".git");
    const commit = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d";
    const calls: RunCall[] = [];
    let workspaceAddAttempts = 0;
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: unknown[][];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          extensionPackage,
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
          run: async (cmd, opts) => {
            calls.push({ cmd, opts });
            if (cmd[0] === "jj" && cmd[1] === "log") {
              return { exitCode: 0, stdout: `${commit}\n`, stderr: "" };
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

    const fetch = calls[0];
    if (!fetch) throw new Error("Provisioning did not fetch the repository");
    provisioningEnv(fetch);
    const addAt = (revision: string) => [
      "jj",
      "workspace",
      "add",
      workspaceDir,
      "--name",
      "widgets-42",
      "--revision",
      revision,
      "-R",
      repoCloneDir,
    ];
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand("legion/WIDGETS-42", repoCloneDir),
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      addAt(commit),
      ["jj", "workspace", "forget", "widgets-42", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      addAt(commit),
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
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });
    await mkdir(workspaceDir, { recursive: true });
    await rm(workspaceDir, { recursive: true });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let logged: string[];
    try {
      await expect(
        provisionIssueWorkspace(issue, {
          extensionPackage,
          repo: "acme/widgets",
          stateDir,
          provisioningToken: async () => "installation-token",
          credentialHelper,
          commandTimeoutMs,
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

    const addAtMain = [
      "jj",
      "workspace",
      "add",
      workspaceDir,
      "--name",
      "widgets-42",
      "--revision",
      "main",
      "-R",
      repoCloneDir,
    ];
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand("legion/WIDGETS-42", repoCloneDir),
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      addAtMain,
      ["jj", "workspace", "forget", "widgets-42", "-R", repoCloneDir],
      ["git", `--git-dir=${gitDir}`, "worktree", "prune"],
      addAtMain,
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
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        extensionPackage,
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        run: async (cmd, opts) => {
          calls.push({ cmd, opts });
          if (cmd[0] === "jj" && cmd[1] === "log") {
            return { exitCode: 0, stdout: `${commits[0]}\n${commits[1]}\n`, stderr: "" };
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      })
    ).rejects.toThrow(
      `Bookmark legion/WIDGETS-42 is conflicted (${commits[0]}, ${commits[1]}); workspace ${workspaceDir} was not created. Resolve it with \`jj bookmark set legion/WIDGETS-42 -r <commit> -R ${repoCloneDir}\`.`
    );
    // The resolution is the last command: no prune, no add, nothing registered for the next
    // resume to adopt.
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand("legion/WIDGETS-42", repoCloneDir),
    ]);
    expect(existsSync(workspaceDir)).toBeFalse();
  });

  test("fails provisioning when the bookmark resolution fails, before any workspace add", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const calls: RunCall[] = [];
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    await mkdir(path.join(repoCloneDir, ".jj"), { recursive: true });

    await expect(
      provisionIssueWorkspace(issue, {
        extensionPackage,
        repo: "acme/widgets",
        stateDir,
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        run: async (cmd, opts) => {
          calls.push({ cmd, opts });
          if (cmd[0] === "jj" && cmd[1] === "log") {
            return { exitCode: 1, stdout: "", stderr: "Error: Failed to read the operation log" };
          }
          return { exitCode: 0, stdout: "", stderr: "" };
        },
      })
    ).rejects.toThrow(
      `Bookmark legion/WIDGETS-42 could not be resolved; workspace ${workspaceDir} was not created.\nCommand failed (exit 1): ${resolveBookmarkCommand("legion/WIDGETS-42", repoCloneDir).join(" ")}\nError: Failed to read the operation log`
    );
    // Nothing is guessed from a failed resolution: no prune, no add, no bookmark command.
    expect(calls.map((call) => call.cmd)).toEqual([
      ["jj", "git", "fetch", "-R", repoCloneDir],
      resolveBookmarkCommand("legion/WIDGETS-42", repoCloneDir),
    ]);
    expect(existsSync(workspaceDir)).toBeFalse();
  });

  test("reports a clone killed at its budget as a timeout, leaves nothing at the final path, and clones fresh on the next attempt", async () => {
    const stateDir = await temporaryDirectory();
    const issue = "WIDGETS-42";
    const repoCloneDir = path.join(stateDir, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(stateDir, "workspaces", "acme", "widgets", "widgets-42");
    const cloneTargets: string[] = [];
    let clones = 0;
    const deps = {
      extensionPackage,
      stateDir,
      repo: "acme/widgets" as const,
      provisioningToken: async () => "installation-token",
      credentialHelper,
      commandTimeoutMs,
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
        extensionPackage,
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
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
            extensionPackage,
            stateDir,
            repo: "acme/widgets",
            provisioningToken: async () => "installation-token",
            credentialHelper,
            commandTimeoutMs,
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
        extensionPackage,
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
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
        extensionPackage,
        stateDir,
        repo: "acme/widgets",
        provisioningToken: async () => "installation-token",
        credentialHelper,
        commandTimeoutMs,
        run: async (cmd) =>
          cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "fetch"
            ? { exitCode: 1, stdout: "", stderr: "fatal: x" }
            : { exitCode: 0, stdout: "", stderr: "" },
      })
    ).rejects.toThrow(`Command failed (exit 1): jj git fetch -R ${repoCloneDir}\nfatal: x`);
  });
});
