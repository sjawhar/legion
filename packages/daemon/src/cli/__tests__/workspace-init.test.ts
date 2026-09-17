import { afterAll, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { RunResult, WorkspaceCommandOptions } from "@legion/workspace";
import { installWorkerGhShim } from "../../daemon/worker-bin";
import { CliError, WorkspaceLostError } from "../errors";
import {
  cmdWorkspaceInit,
  processEnvRunner,
  type WorkspaceInitCommandDeps,
} from "../workspace-init";

const tempDirs: string[] = [];

async function tempRoot(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-workspace-init-"));
  tempDirs.push(dir);
  return dir;
}

interface RecordedCommand {
  cmd: string[];
  opts?: WorkspaceCommandOptions;
}

/** Records every command the workspace provisioning code runs, and — mirroring the two branches
 * `processes.test.ts` fakes for the same provisioning path — actually creates the clone's `.jj`
 * directory on `jj git clone` and the workspace directory on `jj workspace add`, so
 * `provisionIssueWorkspace`'s own existence checks see a completed clone/workspace. */
function recordingRun(jjLogCommitId = ""): {
  run: (cmd: string[], opts?: WorkspaceCommandOptions) => Promise<RunResult>;
  commands: RecordedCommand[];
} {
  const commands: RecordedCommand[] = [];
  const run = async (cmd: string[], opts?: WorkspaceCommandOptions): Promise<RunResult> => {
    commands.push({ cmd, opts });
    if (cmd.join(" ") === "jj log -r @ --no-graph -T commit_id") {
      return { exitCode: 0, stdout: `${jjLogCommitId}\n`, stderr: "" };
    }
    if (cmd[0] === "jj" && cmd[1] === "git" && cmd[2] === "clone") {
      const cloneDir = cmd[4];
      if (cloneDir) await mkdir(path.join(cloneDir, ".jj"), { recursive: true });
    }
    if (cmd[0] === "jj" && cmd[1] === "workspace" && cmd[2] === "add") {
      const workspaceDir = cmd[3];
      if (workspaceDir) await mkdir(workspaceDir, { recursive: true });
    }
    return { exitCode: 0, stdout: "", stderr: "" };
  };
  return { run, commands };
}

async function tokenFile(root: string, contents = "ghs_x\n"): Promise<string> {
  const file = path.join(root, "token");
  await writeFile(file, contents, "utf8");
  return file;
}

describe("cmdWorkspaceInit", () => {
  it("clones and provisions the issue's workspace, installs the gh shim, and creates the fixed directories", async () => {
    const root = await tempRoot();
    const file = await tokenFile(root);
    const { run, commands } = recordingRun();
    const logs: string[] = [];
    const credentialHelper = "!/opt/legion/bin/legion credential";
    const deps: WorkspaceInitCommandDeps = {
      env: { LEGION_PROVISION_TOKEN_FILE: file },
      run,
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (p) => mkdir(p, { recursive: true }),
      log: (line) => logs.push(line),
    };

    await cmdWorkspaceInit(
      { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper },
      deps
    );

    const cloneDir = path.join(root, "repos", "github.com", "acme", "widgets");
    const workspaceDir = path.join(root, "workspaces", "acme", "widgets", "legion-42");

    expect(commands[0]?.cmd).toEqual([
      "jj",
      "git",
      "clone",
      "https://github.com/acme/widgets",
      expect.stringContaining(`${cloneDir}.clone-`),
    ]);
    // The clone and the fetch run with the one provisioning environment `@legion/workspace`
    // builds, exactly: the askpass credential, and the five pairs that reset the clone's
    // credential-helper chain and re-enable askpass for that command — without them the pane
    // helper the clone's config names runs in an init container that has no grant, and git >= 2.44
    // (the worker image's) then refuses the askpass fallback (LEGION-178).
    const provisioningEnv = {
      GIT_ASKPASS: expect.stringContaining(`${root}/`),
      GIT_TERMINAL_PROMPT: "0",
      LEGION_PROVISIONING_TOKEN: "ghs_x",
      GIT_CONFIG_COUNT: "2",
      GIT_CONFIG_KEY_0: "credential.helper",
      GIT_CONFIG_VALUE_0: "",
      GIT_CONFIG_KEY_1: "credential.interactive",
      GIT_CONFIG_VALUE_1: "true",
    };
    expect(commands[0]?.opts?.env).toEqual(provisioningEnv);
    const fetchCommand = commands.find(
      (c) => c.cmd[0] === "jj" && c.cmd[1] === "git" && c.cmd[2] === "fetch"
    );
    expect(fetchCommand?.cmd).toEqual(["jj", "git", "fetch", "-R", cloneDir]);
    expect(fetchCommand?.opts?.env).toEqual(provisioningEnv);

    expect(commands.map((c) => c.cmd)).toContainEqual([
      "jj",
      "workspace",
      "add",
      workspaceDir,
      "--name",
      "legion-42",
      "--revision",
      "main",
      "-R",
      cloneDir,
    ]);

    expect(commands.map((c) => c.cmd)).toContainEqual([
      "git",
      expect.stringContaining("--git-dir="),
      "config",
      "--add",
      "credential.helper",
      credentialHelper,
    ]);
    expect(commands.map((c) => c.cmd)).toContainEqual([
      "git",
      expect.stringContaining("--git-dir="),
      "config",
      "credential.interactive",
      "false",
    ]);

    const ghShim = path.join(root, "worker-bin", "gh");
    expect((await stat(ghShim)).mode & 0o777).toBe(0o700);
    expect(await readFile(ghShim, "utf8")).toContain('exec legion gh -- "$@"');

    expect((await stat(path.join(root, "sessions"))).isDirectory()).toBe(true);
    expect((await stat(path.join(root, "gh"))).isDirectory()).toBe(true);
    expect(logs).toEqual([`workspace-init: ${workspaceDir} on legion/LEGION-42`]);
  });

  it("when the tree clone exists but a resumed OMP session is missing, exits with the session-file error and proceeds when it is present", async () => {
    // A mounted clone makes this a lost-session condition, rather than a lost-volume condition.
    const root = await tempRoot();
    const file = await tokenFile(root);
    const sessionFile = path.join(root, "sessions", "legion-42-planner.jsonl");
    await mkdir(path.join(root, "repos", "github.com", "acme", "widgets"), { recursive: true });
    const depsWith = (log: (line: string) => void): WorkspaceInitCommandDeps => ({
      env: { LEGION_PROVISION_TOKEN_FILE: file, LEGION_RESUME_SESSION_FILE: sessionFile },
      run: recordingRun().run,
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (p) => mkdir(p, { recursive: true }),
      log,
    });
    const logs: string[] = [];
    await expect(
      cmdWorkspaceInit(
        { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "!legion credential" },
        depsWith((line) => logs.push(line))
      )
    ).rejects.toThrow(
      `Refusing to start LEGION-42 fresh: recorded OMP session file is missing from the tree volume: ${sessionFile}`
    );
    // Refused before the repository lock and before provisioning: no workspace, no log line, and
    // the lock is free for the next pod.
    expect(existsSync(path.join(root, "workspaces", "acme", "widgets", "legion-42"))).toBe(false);
    expect(logs).toEqual([]);

    await writeFile(sessionFile, "{}\n", "utf8");
    await cmdWorkspaceInit(
      { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "!legion credential" },
      depsWith((line) => logs.push(line))
    );
    expect(logs).toEqual([
      `workspace-init: ${path.join(root, "workspaces", "acme", "widgets", "legion-42")} on legion/LEGION-42`,
    ]);
  });

  it("distinguishes a lost volume from a missing recorded session", async () => {
    const root = await tempRoot();
    const provisionToken = await tokenFile(root);
    const missing = path.join(root, "sessions", "gone.jsonl");
    const flags = {
      issue: "LEGSMOKE-1",
      repo: "sjawhar/legion",
      root,
      credentialHelper: "legion credential",
    };
    const deps: WorkspaceInitCommandDeps = {
      env: { LEGION_PROVISION_TOKEN_FILE: provisionToken, LEGION_RESUME_SESSION_FILE: missing },
      run: recordingRun().run,
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (target) => mkdir(target, { recursive: true }),
      log: () => {},
    };

    const lost = await cmdWorkspaceInit(flags, deps).catch((error) => error);
    expect(lost).toBeInstanceOf(WorkspaceLostError);
    expect((lost as WorkspaceLostError).code).toBe(3);
    expect((lost as WorkspaceLostError).message).toBe(
      `Tree volume for LEGSMOKE-1 holds neither the clone (${path.join(root, "repos", "github.com", "sjawhar/legion")}) nor the recorded OMP session file (${missing}): the volume was lost`
    );

    await mkdir(path.join(root, "repos", "github.com", "sjawhar", "legion"), { recursive: true });
    const missingSession = await cmdWorkspaceInit(flags, deps).catch((error) => error);
    expect(missingSession).toBeInstanceOf(CliError);
    expect((missingSession as CliError).code).toBe(1);
    expect((missingSession as CliError).message).toContain(
      "recorded OMP session file is missing from the tree volume"
    );
  });

  it("records the branch and commit when a lost workspace is recreated", async () => {
    const root = await tempRoot();
    const provisionToken = await tokenFile(root);
    const { run } = recordingRun("abc123def456");
    await cmdWorkspaceInit(
      {
        issue: "LEGSMOKE-1",
        repo: "sjawhar/legion",
        root,
        credentialHelper: "legion credential",
      },
      {
        env: {
          LEGION_PROVISION_TOKEN_FILE: provisionToken,
          LEGION_WORKSPACE_RECOVERED_FROM: "legion/LEGSMOKE-1",
        },
        run,
        installGhShim: installWorkerGhShim,
        exists: existsSync,
        mkdir: (target) => mkdir(target, { recursive: true }),
        log: () => {},
      }
    );

    expect(
      JSON.parse(
        await readFile(
          path.join(root, "workspaces", "sjawhar", "legion", "legsmoke-1", ".legion", "workspace-recovered.json"),
          "utf8"
        )
      )
    ).toEqual({
      recoveredAt: expect.any(String),
      fromRef: "legion/LEGSMOKE-1",
      sha: "abc123def456",
      reason: "volume-missing",
    });
  });

  it("throws naming LEGION_PROVISION_TOKEN_FILE when it is unset, before any command runs", async () => {
    const root = await tempRoot();
    const { run, commands } = recordingRun();
    const deps: WorkspaceInitCommandDeps = {
      env: {},
      run,
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (p) => mkdir(p, { recursive: true }),
      log: () => {},
    };

    await expect(
      cmdWorkspaceInit(
        { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "x" },
        deps
      )
    ).rejects.toThrow("LEGION_PROVISION_TOKEN_FILE is not set");
    expect(commands).toHaveLength(0);
  });

  it("rejects an invalid --issue before any command runs", async () => {
    const root = await tempRoot();
    const file = await tokenFile(root);
    const { run, commands } = recordingRun();
    const deps: WorkspaceInitCommandDeps = {
      env: { LEGION_PROVISION_TOKEN_FILE: file },
      run,
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (p) => mkdir(p, { recursive: true }),
      log: () => {},
    };

    await expect(
      cmdWorkspaceInit({ issue: "nope", repo: "acme/widgets", root, credentialHelper: "x" }, deps)
    ).rejects.toThrow("--issue");
    expect(commands).toHaveLength(0);
  });

  it("serializes two concurrent invocations on one tree volume: the second waits for the first's provisioning to finish, then both succeed", async () => {
    // Two pods of one tree admitted together (a root and its child issue, say) each run this
    // init container against the same volume, so the same shared clone under
    // `<root>/repos/github.com/acme/widgets`. Unserialized, the second's `jj git clone`/`fetch`
    // and `git config` writes would land on the clone while the first's are still running --
    // git's `.git/config` lock refuses the second writer. The first invocation is held at its
    // first command while the second is started; the second's admission into provisioning is
    // witnessed by its first command, its wait by the lock's own log line -- whichever comes
    // first decides, so nothing here waits on the clock.
    const root = await tempRoot();
    const file = await tokenFile(root);
    const commands: Array<{ who: "first" | "second"; cmd: string[] }> = [];
    const gate = Promise.withResolvers<void>();
    const firstHeld = Promise.withResolvers<void>();
    const secondCommand = Promise.withResolvers<void>();
    const secondWaiting = Promise.withResolvers<void>();
    const secondLogs: string[] = [];
    const runAs = (who: "first" | "second") => {
      const { run } = recordingRun();
      return async (cmd: string[], opts?: WorkspaceCommandOptions): Promise<RunResult> => {
        commands.push({ who, cmd });
        if (who === "first" && commands.length === 1) {
          firstHeld.resolve();
          await gate.promise;
        }
        if (who === "second") secondCommand.resolve();
        return run(cmd, opts);
      };
    };
    const depsFor = (
      who: "first" | "second",
      log: (line: string) => void
    ): WorkspaceInitCommandDeps => ({
      env: { LEGION_PROVISION_TOKEN_FILE: file },
      run: runAs(who),
      installGhShim: installWorkerGhShim,
      exists: existsSync,
      mkdir: (p) => mkdir(p, { recursive: true }),
      log,
    });

    const first = cmdWorkspaceInit(
      { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "!legion credential" },
      depsFor("first", () => {})
    );
    await firstHeld.promise;
    const second = cmdWorkspaceInit(
      { issue: "LEGION-43", repo: "acme/widgets", root, credentialHelper: "!legion credential" },
      depsFor("second", (line) => {
        secondLogs.push(line);
        if (line.startsWith("workspace-init: waiting for ")) secondWaiting.resolve();
      })
    );
    await Promise.race([secondWaiting.promise, secondCommand.promise]);
    // Held first, started second: the second has run nothing against the clone.
    expect(commands.filter((c) => c.who === "second").map((c) => c.cmd)).toEqual([]);
    expect(secondLogs).toEqual([
      `workspace-init: waiting for ${path.join(root, "repos", "github.com", "acme", "widgets.lock")} (another pod is provisioning acme/widgets)`,
    ]);

    gate.resolve();
    await Promise.all([first, second]);

    // Two contiguous blocks: every command of the first (its clone through its last `git
    // config` write) before the second's opener -- the clone-settings read that precedes its `jj
    // git fetch`, the clone the first landed being reused, never a second clone.
    const secondStartsAt = commands.findIndex((c) => c.who === "second");
    expect(secondStartsAt).toBeGreaterThan(0);
    expect(commands.slice(0, secondStartsAt).every((c) => c.who === "first")).toBe(true);
    expect(commands.slice(secondStartsAt).every((c) => c.who === "second")).toBe(true);
    expect(commands[0]?.cmd.slice(0, 3)).toEqual(["jj", "git", "clone"]);
    const cloneDir = path.join(root, "repos", "github.com", "acme", "widgets");
    expect(commands[secondStartsAt]?.cmd).toEqual([
      "jj",
      "config",
      "get",
      "git.abandon-unreachable-commits",
      "-R",
      cloneDir,
    ]);
    expect(commands.filter((c) => c.who === "second" && c.cmd[2] === "clone")).toEqual([]);
    const workspaceAdded = (who: "first" | "second") =>
      commands
        .filter((c) => c.who === who && c.cmd[0] === "jj" && c.cmd[1] === "workspace")
        .map((c) => c.cmd[3]);
    expect(workspaceAdded("first")).toEqual([
      path.join(root, "workspaces", "acme", "widgets", "legion-42"),
    ]);
    expect(workspaceAdded("second")).toEqual([
      path.join(root, "workspaces", "acme", "widgets", "legion-43"),
    ]);
    expect(secondLogs.at(-1)).toBe(
      `workspace-init: ${path.join(root, "workspaces", "acme", "widgets", "legion-43")} on legion/LEGION-43`
    );
    // The lock is released once its holder is done, so the next pod's init container never
    // waits on a finished one: a non-blocking flock on the same file succeeds at once.
    expect(await Bun.spawn(["flock", "--nonblock", `${cloneDir}.lock`, "true"]).exited).toBe(0);
  });

  it("waits behind a live holder of the repository lock and proceeds the moment that holder's process dies -- no lease to expire, no stale-lock takeover", async () => {
    // The lock is `flock(2)` on `<clone>.lock`, held for the holder's process lifetime: a pod whose
    // init container is killed mid-provisioning releases it as its process goes, and the next pod
    // takes over right then -- never by judging the lock file stale, which could steal a lock from
    // a live but slow provisioning (and so put two writers on the shared clone). Here another
    // "pod" is a real process holding the lock through the same `flock` the CLI uses; the
    // invocation must log that it waits and run nothing, then complete after the holder is
    // SIGKILLed -- an untimed release the test triggers, not a clock the code watches.
    const root = await tempRoot();
    const file = await tokenFile(root);
    const lockPath = path.join(root, "repos", "github.com", "acme", "widgets.lock");
    await mkdir(path.dirname(lockPath), { recursive: true });
    const holder = Bun.spawn(["flock", lockPath, "sh", "-c", "echo && read _"], {
      stdin: "pipe",
      stdout: "pipe",
    });
    // The holder has the lock once its shell prints.
    const reader = holder.stdout.getReader();
    expect((await reader.read()).done).toBe(false);

    const { run, commands } = recordingRun();
    const waiting = Promise.withResolvers<void>();
    const logs: string[] = [];
    const init = cmdWorkspaceInit(
      { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "!legion credential" },
      {
        env: { LEGION_PROVISION_TOKEN_FILE: file },
        run,
        installGhShim: installWorkerGhShim,
        exists: existsSync,
        mkdir: (p) => mkdir(p, { recursive: true }),
        log: (line) => {
          logs.push(line);
          if (line.startsWith("workspace-init: waiting for ")) waiting.resolve();
        },
      }
    );
    // Either it reports waiting, or (unlocked) it runs straight through and finishes first.
    await Promise.race([waiting.promise, init]);
    expect(commands).toEqual([]);
    expect(logs).toEqual([
      `workspace-init: waiting for ${lockPath} (another pod is provisioning acme/widgets)`,
    ]);

    holder.kill("SIGKILL");
    await holder.exited;
    await init;
    expect(commands[0]?.cmd.slice(0, 3)).toEqual(["jj", "git", "clone"]);
    expect(logs.at(-1)).toBe(
      `workspace-init: ${path.join(root, "workspaces", "acme", "widgets", "legion-42")} on legion/LEGION-42`
    );
  });

  it("waits behind a live holder only as long as LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS says, then fails naming that wait and the lock", async () => {
    // The daemon sizes this variable from its own boot deadline (the time its watchdog tolerates
    // an initialising pod), so the flock wait must follow it -- here 1 s against a holder that
    // never releases, observed as the timeout failure naming that 1 s. Nothing runs against the
    // clone; the holder is released only after the verdict.
    const root = await tempRoot();
    const file = await tokenFile(root);
    const lockPath = path.join(root, "repos", "github.com", "acme", "widgets.lock");
    await mkdir(path.dirname(lockPath), { recursive: true });
    const holder = Bun.spawn(["flock", lockPath, "sh", "-c", "echo && read _"], {
      stdin: "pipe",
      stdout: "pipe",
    });
    expect((await holder.stdout.getReader().read()).done).toBe(false);
    const { run, commands } = recordingRun();
    const logs: string[] = [];
    try {
      await expect(
        cmdWorkspaceInit(
          {
            issue: "LEGION-42",
            repo: "acme/widgets",
            root,
            credentialHelper: "!legion credential",
          },
          {
            env: {
              LEGION_PROVISION_TOKEN_FILE: file,
              LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS: "1",
            },
            run,
            installGhShim: installWorkerGhShim,
            exists: existsSync,
            mkdir: (p) => mkdir(p, { recursive: true }),
            log: (line) => logs.push(line),
          }
        )
      ).rejects.toThrow(`Timed out after 1 s waiting for workspace-init lock ${lockPath}`);
    } finally {
      holder.kill("SIGKILL");
      await holder.exited;
    }
    expect(commands).toEqual([]);
    expect(logs).toEqual([
      `workspace-init: waiting for ${lockPath} (another pod is provisioning acme/widgets)`,
    ]);
  });

  it("refuses a malformed LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS before any command runs", async () => {
    const root = await tempRoot();
    const file = await tokenFile(root);
    const { run, commands } = recordingRun();
    await expect(
      cmdWorkspaceInit(
        { issue: "LEGION-42", repo: "acme/widgets", root, credentialHelper: "x" },
        {
          env: {
            LEGION_PROVISION_TOKEN_FILE: file,
            LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS: "0.5",
          },
          run,
          installGhShim: installWorkerGhShim,
          exists: existsSync,
          mkdir: (p) => mkdir(p, { recursive: true }),
          log: () => {},
        }
      )
    ).rejects.toThrow(
      'LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS must be a positive whole number of seconds (got "0.5")'
    );
    expect(commands).toHaveLength(0);
  });
});

describe("processEnvRunner", () => {
  it("spawns jj and git through the pod's own environment (PATH resolution) with the provisioning env layered on top", async () => {
    // The runner really spawns: a stub `jj` on a temp PATH records the environment it ran with.
    // `provisionIssueWorkspace` hands the runner only its credential env (`GIT_ASKPASS`,
    // `GIT_TERMINAL_PROMPT`, `LEGION_PROVISIONING_TOKEN`) -- no PATH -- so a runner that passes
    // that env through unmerged cannot find `jj` at all inside the init container.
    const root = await tempRoot();
    const binDir = path.join(root, "bin");
    await mkdir(binDir);
    const record = path.join(root, "jj-ran.env");
    await writeFile(
      path.join(binDir, "jj"),
      `#!/bin/sh\nprintf 'argv=%s\\nASKPASS=%s\\nTOKEN=%s\\nMARKER=%s\\n' "$*" "$GIT_ASKPASS" "$LEGION_PROVISIONING_TOKEN" "$POD_MARKER" > ${JSON.stringify(record)}\necho stub-jj-ok\n`,
      { mode: 0o755 }
    );
    const podEnv: NodeJS.ProcessEnv = {
      PATH: `${binDir}:/usr/bin:/bin`,
      HOME: root,
      POD_MARKER: "from-the-pod",
    };
    const run = processEnvRunner(podEnv);

    const result = await run(["jj", "git", "fetch", "-R", root], {
      env: {
        GIT_ASKPASS: "/tmp/askpass",
        GIT_TERMINAL_PROMPT: "0",
        LEGION_PROVISIONING_TOKEN: "ghs_x",
      },
      timeoutMs: 10_000,
    });

    expect(result.exitCode).toBe(0);
    expect(result.stdout.trim()).toBe("stub-jj-ok");
    expect(await readFile(record, "utf8")).toBe(
      `argv=git fetch -R ${root}\nASKPASS=/tmp/askpass\nTOKEN=ghs_x\nMARKER=from-the-pod\n`
    );
  });

  it("lets the provisioning env override a same-named variable from the pod", async () => {
    const root = await tempRoot();
    const binDir = path.join(root, "bin");
    await mkdir(binDir);
    await writeFile(path.join(binDir, "git"), `#!/bin/sh\nprintf '%s' "$GIT_TERMINAL_PROMPT"\n`, {
      mode: 0o755,
    });
    const run = processEnvRunner({ PATH: `${binDir}:/usr/bin:/bin`, GIT_TERMINAL_PROMPT: "1" });
    const result = await run(["git", "config"], { env: { GIT_TERMINAL_PROMPT: "0" } });
    expect(result.stdout).toBe("0");
  });
});

afterAll(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});
