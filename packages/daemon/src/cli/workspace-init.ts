import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { IssueKey } from "@legion/contracts";
import { type ProvisionIssueWorkspaceDeps, provisionIssueWorkspace } from "@legion/workspace";
import { DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS } from "../daemon/config";
import { SESSIONS_SUBPATH } from "../daemon/k8s-manifests";
import { ISSUE_KEY_PATTERN } from "../daemon/legion-state";
import { type CommandRunner, defaultRunner } from "../state/fetch";
import { CliError } from "./errors";
import { parseRepo } from "./review-threads";
import { readSecretPointer } from "./secret-pointer";

export interface WorkspaceInitCommandDeps {
  env: NodeJS.ProcessEnv;
  run: ProvisionIssueWorkspaceDeps["run"];
  installGhShim(root: string): Promise<string>;
  /** `fs.promises.mkdir(path, { recursive: true })`: idempotent, parents included. */
  mkdir(path: string): Promise<unknown>;
  /** `fs.existsSync`: whether the recorded session file is on the volume. */
  exists(path: string): boolean;
  log(line: string): void;
}

/** The command runner both pod-side CLI entry points hand `@legion/workspace`: `legion
 * workspace-init` for provisioning and `legion worker-shim` for the working-copy adoption. Every
 * command runs with the process's own environment (`env`: the pod's PATH, HOME, the image's tool
 * locations) beneath the env the caller supplies — `createProvisioningCredential`'s for the clone
 * and the fetch (`GIT_ASKPASS`, `GIT_TERMINAL_PROMPT`, `LEGION_PROVISIONING_TOKEN`, and the
 * `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n` pairs that reset the clone's
 * credential-helper chain for those commands, LEGION-178); `JJ_USER`/`JJ_EMAIL` for the adoption —
 * the caller's winning. `defaultRunner` spawns with exactly the env it is given, and the caller's
 * env alone has no PATH: on the daemon host `createDaemonRunner` merges the resolved tool
 * environment the same way; a pod has no such wrapper, so this is where `jj` and `git` become
 * resolvable. */
export function processEnvRunner(
  env: NodeJS.ProcessEnv,
  runner: CommandRunner = defaultRunner
): ProvisionIssueWorkspaceDeps["run"] {
  return async (cmd, opts) => {
    const result = await runner(cmd, { ...opts, env: { ...env, ...opts?.env } });

    return { ...result, stderr: result.stderr ?? "" };
  };
}

/** `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS`: how long a `workspace-init` waits for another
 * pod's provisioning of the same repository. The daemon sets it on every pod it spawns
 * (`KubernetesRuntime.workspaceInitLockWaitSeconds`: its boot watchdog's registration deadline,
 * `worker_boot_timeout_seconds x worker_boot_registration_deadline_intervals`, plus one interval),
 * because the daemon is what bounds an initialising pod -- its probe reports one alive, its
 * watchdog re-arms through a lock wait for exactly that deadline, then retires the pod and spawns
 * the next generation. Sizing the wait from the same numbers means the init container never gives
 * up on a wait the daemon would still tolerate, whatever the deployment configures. The default
 * below is only for an invocation nobody is watching (a manual run without the variable): three
 * `DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS` budgets, covering a live holder's two network commands
 * (`jj git clone`, `jj git fetch`) at full budget plus its local ones. A dead holder releases the
 * instant its process is gone (see `withWorkspaceInitLock`), so the wait only ever guards a hung
 * one, and turns an endless wait into a failure that names the lock. */
const WORKSPACE_INIT_LOCK_WAIT_ENV = "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS";
const DEFAULT_WORKSPACE_INIT_LOCK_WAIT_SECONDS = 3 * DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS;

function workspaceInitLockWaitSeconds(env: NodeJS.ProcessEnv): number {
  const value = env[WORKSPACE_INIT_LOCK_WAIT_ENV];
  if (value === undefined) return DEFAULT_WORKSPACE_INIT_LOCK_WAIT_SECONDS;
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new CliError(
      `${WORKSPACE_INIT_LOCK_WAIT_ENV} must be a positive whole number of seconds (got ${JSON.stringify(value)})`
    );
  }
  return Number(value);
}

/** Serializes shared-clone provisioning across the tree volume's init containers: every issue's
 * pod provisions against one clone under `<root>/repos/github.com/<owner>/<repo>`, and two pods
 * admitted together would otherwise run `jj git clone`/`fetch` and the `git config` writes against
 * it at once (git's `.git/config` lock refuses the second writer). The lock is `flock(2)` — an
 * exclusive advisory lock on `<clone>.lock` on the volume itself, the only state two pods share —
 * taken by util-linux `flock` (in the image's debian base) and held by a child shell for exactly
 * this process's lifetime: the shell blocks on its stdin, so the lock releases when this process
 * releases it, or when this process dies and the pipe closes. No lease, no mtime, no takeover: a
 * holder that is alive holds, however long its provisioning takes; one that is dead holds
 * nothing. A refused non-blocking attempt logs one line, so a pod stuck behind another's
 * provisioning says so in its init-container log, then waits, bounded. */
async function withWorkspaceInitLock<T>(
  root: string,
  repo: string,
  waitSeconds: number,
  log: (line: string) => void,
  operation: () => Promise<T>
): Promise<T> {
  const lockPath = path.join(root, "repos", "github.com", `${repo}.lock`);
  await fs.promises.mkdir(path.dirname(lockPath), { recursive: true });
  let release = await holdFlock(lockPath, ["--nonblock"]);
  if (release === undefined) {
    log(`workspace-init: waiting for ${lockPath} (another pod is provisioning ${repo})`);
    release = await holdFlock(lockPath, ["--timeout", String(waitSeconds)]);
    if (release === undefined) {
      throw new CliError(
        `Timed out after ${waitSeconds} s waiting for workspace-init lock ${lockPath}`
      );
    }
  }
  try {
    return await operation();
  } finally {
    await release();
  }
}

/** Runs `flock <options> <lockPath> sh -c 'echo && read _'`: once flock holds the file's exclusive
 * lock the shell prints one line and blocks on its stdin, holding the lock (the locked descriptor
 * is the shell's) until that stdin closes. Resolves the releaser — closes the stdin, awaits the
 * exit — when the line arrives; `undefined` when flock exits 1 instead, its one conflict code
 * (`--nonblock` refused, or `--timeout` elapsed). Any other exit is an error naming it; flock's
 * own stderr is inherited. */
async function holdFlock(
  lockPath: string,
  options: string[]
): Promise<(() => Promise<void>) | undefined> {
  const child = spawn("flock", [...options, lockPath, "sh", "-c", "echo && read _"], {
    stdio: ["pipe", "pipe", "inherit"],
  });
  const acquired = Promise.withResolvers<number | undefined>();
  child.once("error", acquired.reject);
  child.stdout.once("data", () => acquired.resolve(undefined));
  child.once("exit", (code) => acquired.resolve(code ?? 1));
  const exitCode = await acquired.promise;
  if (exitCode === undefined) {
    return async () => {
      const exited = Promise.withResolvers<void>();
      child.once("exit", () => exited.resolve());
      child.stdin.end();
      await exited.promise;
    };
  }
  if (exitCode === 1) return undefined;
  throw new CliError(`flock ${options.join(" ")} ${lockPath} exited ${exitCode}`);
}

/** `legion workspace-init` (the Kubernetes runtime's init container): prepares an issue's jj
 * working copy on the tree's persistent volume before the main container's `worker-shim` starts —
 * the same provisioning path `TmuxRuntime` calls in-process (`provisionIssueWorkspace`), the
 * daemon's own `gh` shim (`installWorkerGhShim`), and the volume's fixed directories the main
 * container's mounts expect (`sessions` for its `subPath: sessions` mount, `gh` for
 * `GH_CONFIG_DIR`). Both flags are validated, and the provisioning token file is read, before
 * anything is installed or run. */
export async function cmdWorkspaceInit(
  flags: { issue: string; repo: string; root: string; credentialHelper: string },
  deps: WorkspaceInitCommandDeps
): Promise<void> {
  if (!ISSUE_KEY_PATTERN.test(flags.issue)) {
    throw new CliError(
      `--issue must be a Dispatch issue key like LEGION-1 (got ${JSON.stringify(flags.issue)})`
    );
  }
  parseRepo(flags.repo);
  const tokenFile = deps.env.LEGION_PROVISION_TOKEN_FILE;
  if (tokenFile === undefined) {
    throw new CliError("LEGION_PROVISION_TOKEN_FILE is not set");
  }
  const token = readSecretPointer("LEGION_PROVISION_TOKEN_FILE", tokenFile);
  const lockWaitSeconds = workspaceInitLockWaitSeconds(deps.env);
  await deps.installGhShim(flags.root);
  await deps.mkdir(path.join(flags.root, SESSIONS_SUBPATH));
  await deps.mkdir(path.join(flags.root, "gh"));
  // The same-agent invariant the tmux runtime enforces with a `stat` on the daemon host: a
  // recorded session file that is gone from the volume is a launch failure (this container exits
  // non-zero, the pod goes Failed, the daemon counts it), never a silent fresh agent -- which is
  // exactly what the pinned OMP does with a missing `--resume` path (exit 0, `ready`, new agent).
  // Checked before the repository lock: the volume is mounted either way, and a doomed pod must
  // not hold the shared clone's lock while it fails.
  const resumeSessionFile = deps.env.LEGION_RESUME_SESSION_FILE;
  if (resumeSessionFile !== undefined && !deps.exists(resumeSessionFile)) {
    throw new CliError(
      `Refusing to start ${flags.issue} fresh: recorded OMP session file is missing from the tree volume: ${resumeSessionFile}`
    );
  }
  const spec = await withWorkspaceInitLock(flags.root, flags.repo, lockWaitSeconds, deps.log, () =>
    provisionIssueWorkspace(flags.issue as IssueKey, {
      repo: flags.repo as `${string}/${string}`,
      stateDir: flags.root,
      provisioningToken: async () => token,
      credentialHelper: flags.credentialHelper,
      commandTimeoutMs: DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS * 1000,
      run: deps.run,
    })
  );
  deps.log(`workspace-init: ${spec.workspaceDir} on ${spec.bookmark}`);
}
