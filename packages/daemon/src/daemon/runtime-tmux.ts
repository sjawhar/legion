import { createHash } from "node:crypto";
import { mkdir, readFile, rm, stat } from "node:fs/promises";
import path from "node:path";
import { controllerToken, type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import {
  issueWorkspaceDir,
  type JjIdentity,
  type ProvisionIssueWorkspaceDeps,
  provisionIssueWorkspace,
  runAdoptWorkingCopy,
  type WorkspaceSpec,
} from "@legion/workspace";
import type { CommandResult, CommandRunnerOptions } from "../state/fetch";
import { parseProcStatStartTicks } from "./proc-stat";
import {
  assertResumeSessionFile,
  awaitShutdown,
  DAEMON_CLI_ENTRYPOINT,
  type ControllerLocator,
  type Locator,
  type ProbeResult,
  ProcessStopFailed,
  type Runtime,
  type SpawnSpec,
  sameProcess,
  serialize,
  shellPath,
  type TmuxLocator,
} from "./runtime";
import { extraSecretName, isSharedSecretName, writeSecretFile } from "./secrets";
import * as tmux from "./tmux";
import { PANE_GONE_STDERR, type TmuxServer } from "./tmux";
import type { WorkerRpcClient } from "./worker-rpc";

/** Variables tmux 3.7 itself writes into a server's global environment at start (`PWD` from the
 * server's cwd, `SHLVL=0`), whatever the client that forked it carried: present on a freshly
 * forked server too, so they are never "left by an earlier daemon". */
const TMUX_OWN_GLOBALS: Record<string, true> = { PWD: true, SHLVL: true };

const MAX_TMUX_WINDOW_NAME_LENGTH = 160;

function treeName(issue: IssueKey): string {
  const fullName = issue.toLowerCase();
  if (fullName.length <= MAX_TMUX_WINDOW_NAME_LENGTH) return fullName;

  const suffix = createHash("sha256").update(fullName).digest("hex").slice(0, 16);
  return `${fullName.slice(0, MAX_TMUX_WINDOW_NAME_LENGTH - suffix.length - 1)}-${suffix}`;
}

/**
 * A worker socket's basename must stay well under the ~100-byte Unix socket path limit
 * regardless of the issue key's length (unlike `treeName`, which only bounds itself to tmux's
 * much longer window-name limit): derived from the role and a short hash of the full issue key,
 * so an unusually long Dispatch project token still produces a bounded, unique name.
 */
function workerSocketBasename(issue: IssueKey, role: LegionRole): string {
  const hash = createHash("sha256").update(issue).digest("hex").slice(0, 8);
  return `${role}-${hash}`;
}

/** Flattens an env record into repeated `-e KEY=VALUE` pairs for tmux; `undefined` values are
 * omitted. Never given PATH — tmux replaces a pane's PATH from the unattached client's
 * environment after copying the `-e` pairs, so `paneEnvPairs` returns the `export PATH` prefix
 * for the pane's shell command instead. */
function tmuxEnv(env: Record<string, string | undefined>): string[] {
  return Object.entries(env).flatMap(([key, value]) =>
    value === undefined ? [] : ["-e", `${key}=${value}`]
  );
}

/** Escapes `text` for the inside of a POSIX double-quoted shell word: `\`, `"`, `$`, and `` ` ``
 * are the four characters the shell still interprets there. Used for the inline addressing text
 * that shares one double-quoted `--append-system-prompt` value with the `$(cat …)` fragments. */
function shellDoubleQuoted(text: string): string {
  return text.replaceAll(/[\\"$`]/g, (character) => `\\${character}`);
}

/** The one `--append-system-prompt` argument every daemon-launched OMP process receives. OMP's
 * flag is last-wins (its argv handler assigns `appendSystemPrompt`), so several flags would hand
 * the model only the final fragment — with deployment instructions configured, a pane would get
 * neither its role prompt nor its addressing line nor the root's gate policy. Every fragment is
 * therefore joined into a single value, in order: the packaged role prompt, the addressing
 * fragment (every root and phase worker; the controller has none), then the deployment
 * instructions file when configured, separated by a blank line. The value is one double-quoted
 * shell word: the file-backed fragments are `$(cat <path>)` expanded by the process's own shell —
 * never inlined into the command (size and quoting) — and the addressing text is escaped for the
 * double quotes; the blank lines are literal newlines inside the word, which every POSIX shell
 * accepts. One builder for `issueInnerCommand` and `spawnController` alike, so the two launch
 * sites cannot drift. `KubernetesRuntime` joins the same fragments, as text, the same way. */
export function systemPromptArguments(
  promptPath: string,
  addressingPrompt: string | undefined,
  deploymentInstructionsFile: string | undefined
): string {
  const fragments = [`$(cat ${shellPath(promptPath)})`];
  if (addressingPrompt !== undefined) fragments.push(shellDoubleQuoted(addressingPrompt));
  if (deploymentInstructionsFile !== undefined) {
    fragments.push(`$(cat ${shellPath(deploymentInstructionsFile)})`);
  }
  return `--append-system-prompt "${fragments.join("\n\n")}"`;
}

/** Prepends the configured `omp_launch_prefix` (see `DaemonConfig.ompLaunchPrefix`) to an OMP
 * invocation shell fragment, so provider credentials or any other launch wrapper are obtained
 * *inside* the pane process rather than carried by the daemon itself — the daemon never exports
 * provider keys to its own environment or to a pane's tmux `-e` argv. Each prefix element is
 * shell-quoted independently. Used for every OMP invocation the daemon builds: spawned
 * root/worker/controller panes (`runtime-tmux.ts`) and the startup capability probes
 * (`boot-probes.ts`) — one launch path, never duplicated. */
export function withOmpLaunchPrefix(
  launchPrefix: readonly string[],
  ompInvocation: string
): string {
  if (launchPrefix.length === 0) return ompInvocation;
  return `${launchPrefix.map(shellPath).join(" ")} ${ompInvocation}`;
}

export interface TmuxRuntimeDeps {
  /** The private server (`{ run, socket: `legion-${project}` }`); `socket` doubles as the
   * session name and the `@legion_owner` value. */
  tmux: TmuxServer;
  /** Names each process's secret file: `roleToken(project, issue, role)` / `controllerToken(project)`. */
  project: string;
  /** `<stateDir>/workers/<name>.sock` and `<stateDir>/secrets/<token>`. */
  stateDir: string;
  /** The resolved OMP invocation every pane runs (`environment.ompInvocation`). */
  ompInvocation: string;
  /** `config.ompLaunchPrefix`. */
  ompLaunchPrefix: readonly string[];
  /** `<state_dir>/deployment-instructions.md` when configured — appended to every launched
   * pane's system prompt as its last `--append-system-prompt "$(cat <this file>)"` fragment.
   * Undefined: no fragment. */
  deploymentInstructionsFile?: string;
  /** Overridable for tests: the role-prompt existence check (`stat` by default). */
  statPrompt?(promptPath: string): Promise<unknown>;
  /** The GitHub App installation token `provisionIssueWorkspace` clones with. */
  provisioningToken(owner: string): Promise<string>;
  /** The daemon's command runner, for provisioning commands (`jj`, `git`). */
  run(
    cmd: string[],
    options?: CommandRunnerOptions
  ): Promise<{
    stdout: string;
    stderr?: string;
    exitCode: number;
    timedOut?: CommandResult["timedOut"];
    aborted?: CommandResult["aborted"];
  }>;
  /** `config.repo`: the one repository every issue provisions against. */
  repo: `${string}/${string}`;
  /** The git `credential.helper` value written into the clone (`daemonCredentialHelper()`). */
  credentialHelper: string;
  /** `config.slowCommandTimeoutSeconds * 1000`, the per-command provisioning budget. */
  slowCommandTimeoutMs: number;
  connectWorkerRpc(socketPath: string, timeoutMs?: number): Promise<WorkerRpcClient>;
  /** The timeout `stop`'s own no-negotiate dial uses. */
  workerRpcTimeoutMs(): number;
  /** `reconcileOrphans`' grace clock. */
  now(): number;
  /** The locators state currently records for `issue` — the tree's first (when `issue` is a tree
   * root), then every role claim's — the windows a spawn may share, each trusted only while a
   * pane in it still verifies as its recorded process. Never written to. */
  issueLocators(issue: IssueKey): readonly Locator[];
  readProcessCmdline?(pid: number): Promise<string>;
  /** Reads `/proc/<pid>/stat` -- overridable for tests exactly like `readProcessCmdline`;
   * rejects once the process is gone. */
  readProcessStat?(pid: number): Promise<string>;
  /** Bounds a graceful stop's wait; overridable for tests, defaults to a real timer. */
  sleep?(ms: number): Promise<void>;
}

/** `verifyPaneProcess`'s verdict. Every `verified: false` names why and carries whatever the
 * pane currently reports, so `describePaneVerdict` can show both identities side by side.
 * `listing-failed` is the one reason that is not a verdict about the pane: the listing itself
 * failed (see `tmux.lookupPane`), so the pane is neither confirmed nor refuted. */
type PaneVerdict =
  | { verified: true; pid: number; paneId: string }
  | { verified: false; reason: "no-identity" | "pane-gone" }
  | { verified: false; reason: "listing-failed"; detail: string }
  | {
      verified: false;
      reason: "pid-mismatch" | "stat-unreadable" | "not-omp";
      observedPid: number;
    }
  | { verified: false; reason: "start-mismatch"; observedPid: number; observedStartTicks: number };

function describePaneVerdict(
  locator: TmuxLocator,
  verdict: Exclude<PaneVerdict, { verified: true }>
): string {
  const pane = locator.tmuxPaneId ?? locator.tmuxWindowId;
  const recorded = `recorded pid ${locator.panePid ?? "?"} start ${locator.paneStartTicks ?? "?"}`;
  switch (verdict.reason) {
    case "no-identity":
      return `pane ${pane} has no recorded process identity (locator predates identity tracking)`;
    case "pane-gone":
      return `pane ${pane} is gone`;
    case "listing-failed":
      return `cannot verify pane ${pane}: ${verdict.detail}`;
    case "pid-mismatch":
      return `pane ${pane} now runs pid ${verdict.observedPid} (${recorded})`;
    case "stat-unreadable":
      return `pane ${pane} pid ${verdict.observedPid} has no readable /proc stat (${recorded})`;
    case "start-mismatch":
      return `pane ${pane} runs pid ${verdict.observedPid} started at ${verdict.observedStartTicks} (${recorded})`;
    case "not-omp":
      return `pane ${pane} pid ${verdict.observedPid} is not running OMP (${recorded})`;
  }
}

/** A `/proc/<pid>/...` read that failed because the process is gone: ENOENT (the directory has
 * been reaped) or ESRCH (the kernel reports it while the entry lingers). Everything else is a
 * host fault the caller must not read as a verdict. */
function isProcessGoneError(error: unknown): boolean {
  const code = (error as { code?: unknown } | null)?.code;
  return code === "ENOENT" || code === "ESRCH";
}

/**
 * The tmux implementation of `Runtime`: every Legion process is a `legion worker-shim` pane on
 * the daemon's private tmux server. One window per issue — the issue's first process (root
 * architect or phase worker alike) opens a window named for the issue and every later process
 * on that issue splits into it while a pane recorded there still verifies as its recorded
 * process; the controller has a window of its own. That window sharing is tmux's model and lives
 * entirely here, never in the interface: `ProcessManager` only ever sees opaque locators. So
 * does the private session's first creation: one lane across every issue and the controller
 * (`ensureSession`), so two first spawns that arrive together share one `new-session` instead
 * of the second failing `duplicate session`.
 *
 * A pane id is never taken as proof of the process a locator recorded. A recreated tmux server
 * hands out ids from `%1` again, so a stale locator's id can name some other role's live pane --
 * one running OMP, so a command-line check alone passes. Every locator this runtime writes
 * therefore records the pane's root pid and that process's `/proc/<pid>/stat` start ticks at
 * launch, and `verifyPaneProcess` -- the one chokepoint -- re-checks pid, then start ticks, then
 * OMP before `probe` reports a pane alive, before `stop` kills it, and before `probedWindowId`
 * splits a new pane into its window.
 */
/** The session lane's result distinguishes bootstrap-window ownership from a session another
 * caller created while this caller waited, so every caller that needs a recovery retry can make
 * one without more than one caller trying to clean up the bootstrap window. */
interface SessionEnsureResult {
  createdByCaller: boolean;
  sessionCreated: boolean;
}

export class TmuxRuntime implements Runtime {
  readonly controllerLaunch = "daemon" as const;
  readonly removesWorkspacesOnTreeClose = true;
  /** Serializes tmux window creation per issue, so two concurrent spawns never each see "no
   * window yet" and open two. */
  private readonly issueLaunchQueue = new Map<IssueKey, Promise<unknown>>();
  /** Serializes workspace provisioning per repository clone (see `provisionWorkspace`). */
  private readonly provisionQueue = new Map<string, Promise<unknown>>();
  /** The pane this runtime most recently opened a fresh window with for an issue, identity
   * included: one more candidate for `probedWindowId`, so a concurrent second spawn on the same
   * issue (or any later one, while every persisted locator still names a window nothing
   * verifies in) can verify that pane and split into its window instead of racing to open its
   * own. Never trusted by window id alone -- after a tmux server recreate the same `@N` can name
   * some other issue's window, so an entry whose pane no longer verifies is dropped. */
  private readonly issueWindows = new Map<IssueKey, TmuxLocator>();
  /** The private session's creation in flight, if any — `tmux.ensureSession`'s promise, shared
   * by every spawn that reaches the session step while it runs, and cleared the moment it
   * settles either way (see `ensureSession`). */
  private sessionCreation: Promise<boolean> | undefined;

  constructor(private readonly deps: TmuxRuntimeDeps) {}

  /** The daemon's own `spawn("controller", …)` recorded the pane; a ready call adds nothing. */
  controllerReadyLocator(): undefined {
    return undefined;
  }

  /**
   * The one cross-issue lane: makes the private session exist, running `has-session` and (when
   * needed) `new-session` at most once at a time across every issue and the controller. The
   * caller that finds nothing in flight starts `tmux.ensureSession`; `createdByCaller` is `true`
   * only when that caller made the session and owns the bootstrap cleanup. A caller that arrives
   * while one is in flight joins that same creation, has `createdByCaller: false`, and shares
   * the creator's rejection if the creation fails. `sessionCreated` tells a recovery caller
   * whether anyone made a replacement session while it waited. The clearing handler is
   * registered before any waiter chains on the promise, so the lane is already empty by the time
   * any caller resumes: a spawn arriving after settlement runs a fresh `has-session`, and a
   * failed `new-session` is retried, never replayed. Because the clear handles the rejection
   * branch too, a failed creation nobody waited on never surfaces as an unhandled rejection.
   *
   * `has-session` runs inside the lane, not before it: a `false` observed outside could be stale
   * by the time the caller looked at the lane (the creator finishing and the lane clearing in
   * between), and that caller would run a second `new-session` and hit `duplicate session`.
   */
  private ensureSession(): Promise<SessionEnsureResult> {
    const inFlight = this.sessionCreation;
    if (inFlight) {
      return inFlight.then((sessionCreated) => ({
        createdByCaller: false,
        sessionCreated,
      }));
    }
    const session = this.deps.tmux.socket;
    const creation = tmux.ensureSession(this.deps.tmux, session, session);
    this.sessionCreation = creation;
    const clear = () => {
      this.sessionCreation = undefined;
    };
    creation.then(clear, clear);
    return creation.then((sessionCreated) => ({
      createdByCaller: sessionCreated,
      sessionCreated,
    }));
  }

  /** Opens a fresh window named `name` running `paneArgv`. Both the first session check and the
   * recheck after a `new-window` finds the session gone run through `ensureSession`, never
   * directly through `tmux.ensureSession`: concurrent creators therefore share one creation.
   * A caller that created the session does not retry a failed first window; its error did not
   * arise from a session another process had already torn down. */
  private async openWindow(
    name: string,
    paneArgv: string[]
  ): Promise<{ windowId: string; paneId: string; pid: number }> {
    const session = this.deps.tmux.socket;
    const initial = await this.ensureSession();
    try {
      return await tmux.openWindow(
        this.deps.tmux,
        session,
        name,
        paneArgv,
        session,
        initial.createdByCaller
      );
    } catch (firstError) {
      if (initial.createdByCaller) throw firstError;
      const recovery = await this.ensureSession();
      if (!recovery.sessionCreated) throw firstError;
      try {
        const window = await tmux.openWindow(
          this.deps.tmux,
          session,
          name,
          paneArgv,
          session,
          recovery.createdByCaller
        );
        const first = firstError instanceof Error ? firstError.message : String(firstError);
        console.error(
          `[legion] tmux new-window for ${name} failed after has-session reported ${session} present, and the session was gone by the time the window opened (${first}); recreated it and opened the window on a second attempt`
        );
        return window;
      } catch (retryError) {
        const first = firstError instanceof Error ? firstError.message : String(firstError);
        const retry = retryError instanceof Error ? retryError.message : String(retryError);
        throw new Error(
          `${retry} on the second attempt, after has-session reported ${session} present and then gone (first attempt: ${first})`
        );
      }
    }
  }

  /**
   * Provisions the issue's working copy (`provisionIssueWorkspace`, on the daemon's disk),
   * assembles the OMP command from the spec's launch description, then opens (or splits into)
   * the tmux window for the spec's issue — running `legion worker-shim` around that command —
   * or the controller's own window, where the inner command runs bare: the controller is an
   * interactive OMP terminal session Sami can attach to, with no shim and no socket. Delivers
   * every secret in the spec as its own 0600 file under `<stateDir>/secrets` — the process's own
   * (the boot token, or the controller secret: the one secret not in `SHARED_SECRET_NAMES`) as
   * `<role token>`, each shared one as `<role token>-<lowercased name>` (`extraSecretName`) — and
   * exports only their `<NAME>_FILE` paths, appended after the spec's own env pairs in the spec's
   * order; the writes happen before any tmux call, so an fs failure is an ordinary launch
   * failure. The caller owns those files' lifetimes (hold/prune) — this method only writes them.
   * `env.PATH` is exported in the pane's shell command rather than passed as a `-e` pair, which
   * tmux would discard (see `paneEnvPairs`).
   */
  async spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator> {
    const secrets = Object.entries(spec.secrets);
    const own = secrets.filter(([name]) => !isSharedSecretName(name));
    if (own.length !== 1) {
      throw new Error(
        `tmux runtime delivers exactly one secret of the process's own per process (its boot token or controller secret) beside the shared ones; got ${own.length === 0 ? "none" : own.map(([name]) => name).join(", ")}`
      );
    }
    if (kind === "controller") {
      return this.spawnController(spec, controllerToken(this.deps.project), secrets);
    }
    if (!spec.issue) throw new Error(`spawn ${kind} requires spec.issue`);
    if (spec.role === "controller") {
      throw new Error(`spawn ${kind} requires a Legion role, not "controller"`);
    }
    return this.spawnIssueProcess(
      kind,
      spec.issue,
      spec.role,
      spec,
      roleToken(this.deps.project, spec.issue, spec.role),
      secrets
    );
  }

  /** `--resume=<file>` when a recorded session is being resumed; a missing file is a launch
   * failure (`assertResumeSessionFile`, the same-agent invariant), never a silent fresh start. */
  private async resumeArgument(
    subject: string,
    resumeSessionFile: string | undefined,
    logVerb: string
  ): Promise<string> {
    if (!resumeSessionFile) return "";
    await assertResumeSessionFile(subject, resumeSessionFile, logVerb);
    console.info(`[legion] ${logVerb} ${subject} by resuming OMP session ${resumeSessionFile}`);
    return ` --resume=${shellPath(resumeSessionFile)}`;
  }

  /** The OMP command every issue process — root architect and phase worker alike — runs inside
   * its shim: the configured launch prefix and invocation, `--resume` when a recorded session is
   * being resumed (a missing session file is a launch failure, see `resumeArgument`), RPC mode,
   * and the system-prompt fragments from `systemPromptArguments`. The prompt file is stat'ed
   * first so a missing role prompt fails before any resume decision or spawn. */
  private async issueInnerCommand(
    issue: IssueKey,
    launch: SpawnSpec["launch"],
    logVerb: string
  ): Promise<string> {
    await (this.deps.statPrompt ?? stat)(launch.promptPath);
    const resume = await this.resumeArgument(issue, launch.resumeSessionFile, logVerb);
    return `${withOmpLaunchPrefix(this.deps.ompLaunchPrefix, this.deps.ompInvocation)}${resume} --mode rpc ${systemPromptArguments(launch.promptPath, launch.addressingPrompt, this.deps.deploymentInstructionsFile)}`;
  }

  /** Provisions the jj workspace and credential wiring shared by every issue's process — the
   * root architect and every phase worker alike (`@legion/workspace`, on the daemon's disk).
   * Serialized per repository: every issue of a daemon shares one clone under
   * `<state_dir>/repos/…`, and two trees admitted in one sweep would otherwise run `jj git clone`
   * /`fetch` and the `git config` writes against it at once — git's `.git/config` lock refuses
   * the second writer (`could not lock config file … File exists`), a launch failure that says
   * nothing about the launch. */
  private provisionWorkspace(issue: IssueKey): Promise<WorkspaceSpec> {
    const [owner] = this.deps.repo.split("/") as [string, string];
    return serialize(this.provisionQueue, this.deps.repo, () =>
      provisionIssueWorkspace(issue, {
        repo: this.deps.repo,
        stateDir: this.deps.stateDir,
        provisioningToken: () => this.deps.provisioningToken(owner),
        credentialHelper: this.deps.credentialHelper,
        commandTimeoutMs: this.deps.slowCommandTimeoutMs,
        run: this.workspaceRun,
      })
    );
  }

  /** `deps.run` as `@legion/workspace` expects it: the daemon's runner may omit `stderr`
   * (`CommandResult.stderr` is optional on a killed command); the workspace contract requires
   * it, since `commandFailure` quotes it. */
  private readonly workspaceRun: ProvisionIssueWorkspaceDeps["run"] = async (command, options) => {
    const result = await this.deps.run(command, options);
    return { ...result, stderr: result.stderr ?? "" };
  };

  /** Runs the shared adoption command (`adoptWorkingCopyCommand`) on the daemon-host workspace
   * before `ProcessManager` sends an assignment prompt. The Kubernetes runtime sends the same
   * command to the pod's shim instead, because that workspace only exists on the mounted tree
   * volume. */
  async adoptWorkingCopy(
    issue: IssueKey,
    role: LegionRole,
    identity: JjIdentity,
    timeoutMs: number
  ): Promise<void> {
    try {
      await runAdoptWorkingCopy(
        this.workspaceRun,
        issueWorkspaceDir(this.deps.stateDir, this.deps.repo, issue),
        identity,
        timeoutMs
      );
    } catch (error) {
      throw new Error(
        `Could not adopt ${issue}'s working copy for ${role}: ${error instanceof Error ? error.message : String(error)}`
      );
    }
  }

  private async spawnIssueProcess(
    kind: "root" | "worker",
    issue: IssueKey,
    role: LegionRole,
    spec: SpawnSpec,
    token: string,
    secrets: Array<[string, string]>
  ): Promise<TmuxLocator> {
    // Today's order, kept: provision, then the prompt stat and session-file stat inside the
    // command assembly, then the socket, secret file, and tmux argv.
    const workspace = await this.provisionWorkspace(issue);
    const innerCommand = await this.issueInnerCommand(
      issue,
      spec.launch,
      kind === "root" ? "resurrecting" : "respawning"
    );
    const env = {
      ...spec.env,
      [kind === "root" ? "LEGION_ROOT_WORKSPACE" : "LEGION_WORKSPACE"]: workspace.workspaceDir,
    };
    const { socketPath, paneArgv } = await this.preparePane(
      workerSocketBasename(issue, role),
      workspace.workspaceDir,
      env,
      innerCommand,
      token,
      secrets
    );
    const session = this.deps.tmux.socket;
    // The identity is read inside the per-issue lane: a concurrent second spawn on this issue
    // queued behind this one decides whether to split into the window it opened by verifying
    // this very pane (`probedWindowId` via `issueWindows`), so the pane must be fully recorded --
    // pid and start ticks -- before the lane is released to it.
    return serialize(this.issueLaunchQueue, issue, async () => {
      const existingWindowId = await this.probedWindowId(issue);
      if (existingWindowId) {
        const { paneId, pid } = await tmux.splitWindow(this.deps.tmux, existingWindowId, paneArgv);
        const identity = await this.recordedPaneIdentity(paneId, pid, token);
        return {
          runtime: "tmux",
          tmuxSession: session,
          tmuxWindowId: existingWindowId,
          tmuxPaneId: paneId,
          socketPath,
          ...identity,
        };
      }
      const window = await this.openWindow(treeName(issue), paneArgv);
      const identity = await this.recordedPaneIdentity(window.paneId, window.pid, token);
      const opened: TmuxLocator = {
        runtime: "tmux",
        tmuxSession: session,
        tmuxWindowId: window.windowId,
        tmuxPaneId: window.paneId,
        socketPath,
        ...identity,
      };
      this.issueWindows.set(issue, opened);
      return opened;
    });
  }

  /** The controller's window: the inner command bare in its pane — no `legion worker-shim`, so
   * the locator carries no `socketPath` and `connect` refuses it; `stop` falls straight through
   * to the pane kill and `probe` reads the pane like any other. */
  private async spawnController(
    spec: SpawnSpec,
    token: string,
    secrets: Array<[string, string]>
  ): Promise<TmuxLocator> {
    const controllerDir = path.join(this.deps.stateDir, "controller");
    await (this.deps.statPrompt ?? stat)(spec.launch.promptPath);
    await mkdir(controllerDir, { recursive: true });
    const resume = await this.resumeArgument(
      token,
      spec.launch.resumeSessionFile,
      "resurrecting the controller"
    );
    // Interactive: no `--mode rpc`, and the pane runs this command bare — no `legion worker-shim`,
    // no socket.
    const innerCommand = `${withOmpLaunchPrefix(this.deps.ompLaunchPrefix, this.deps.ompInvocation)}${resume} ${systemPromptArguments(spec.launch.promptPath, undefined, this.deps.deploymentInstructionsFile)}`;
    const { pairs, exportPath } = await this.paneEnvPairs(spec.env, token, secrets);
    const paneArgv = [...pairs, `${exportPath}cd ${shellPath(controllerDir)} && ${innerCommand}`];
    const session = this.deps.tmux.socket;
    const window = await this.openWindow("controller", paneArgv);
    const identity = await this.recordedPaneIdentity(window.paneId, window.pid, token);
    return {
      runtime: "tmux",
      tmuxSession: session,
      tmuxWindowId: window.windowId,
      tmuxPaneId: window.paneId,
      ...identity,
    };
  }

  /** Everything a new issue pane needs before any tmux call, in the order every spawn performs
   * it: a fresh shim socket path (its directory made, a stale socket removed), the pane's env
   * pairs with the process's secrets each written as a 0600 file (`paneEnvPairs`), then the
   * `legion worker-shim --socket <path> -- <inner>` command every headless Legion OMP process
   * (root architect, phase worker) runs inside its pane. The controller's pane is prepared by
   * `spawnController` from the same env pairs, without a socket or shim. */
  private async preparePane(
    socketName: string,
    workspaceDir: string,
    env: Record<string, string | undefined>,
    innerCommand: string,
    token: string,
    secrets: Array<[string, string]>
  ): Promise<{ socketPath: string; paneArgv: string[] }> {
    const socketPath = await this.prepareSocket(socketName);
    const { pairs, exportPath } = await this.paneEnvPairs(env, token, secrets);
    const shellCommand = `${exportPath}cd ${shellPath(workspaceDir)} && ${shellPath(process.execPath)} ${shellPath(DAEMON_CLI_ENTRYPOINT)} worker-shim --socket ${shellPath(socketPath)} -- ${innerCommand}`;
    return { socketPath, paneArgv: [...pairs, shellCommand] };
  }

  /** The `-e KEY=VALUE` pairs every pane — an issue's or the controller's — opens with, in the
   * one order the contract tests pin: the env pairs (PATH excepted, below), then one `<NAME>_FILE`
   * pointer per secret — each secret written first as a 0600 file under `<stateDir>/secrets`
   * (the process's own as `<token>`, a shared one as `extraSecretName`), never passed as a value.
   * Also returns the `export PATH=… && ` prefix the pane's shell command starts with.
   *
   * PATH is the one env variable that does not ride a `-e` pair. tmux copies the server's global
   * table, the session table, and every `-e` pair into a new pane's environment and then, for a
   * pane spawned by an unattached client — every daemon `tmux -L … new-window|split-window|new-session`
   * is one — replaces PATH from that client's environment (spawn.c, `spawn_pane`: "The session one
   * is replaced from the client if there is one"), so a `-e PATH=…` never reaches a pane
   * (LEGION-91). The pane shell therefore exports `env.PATH` before `cd`, and the worker-shim (or
   * the controller's bare OMP) and the OMP it spawns inherit exactly that value. An env record
   * without PATH gets no prefix and inherits like any other unset variable. This is tmux-only: a
   * Kubernetes runtime maps env to the pod environment, which is honoured verbatim, and needs no
   * prefix. */
  private async paneEnvPairs(
    env: Record<string, string | undefined>,
    token: string,
    secrets: Array<[string, string]>
  ): Promise<{ pairs: string[]; exportPath: string }> {
    const { PATH: panePath, ...pairEnv } = env;
    const exportPath = panePath === undefined ? "" : `export PATH=${shellPath(panePath)} && `;
    const pointers: Record<string, string> = {};
    for (const [name, value] of secrets) {
      pointers[`${name}_FILE`] = await writeSecretFile(
        this.deps.stateDir,
        isSharedSecretName(name) ? extraSecretName(token, name) : token,
        value
      );
    }
    return { pairs: [...tmuxEnv(pairEnv), ...tmuxEnv(pointers)], exportPath };
  }

  private async prepareSocket(name: string): Promise<string> {
    const socketPath = path.join(this.deps.stateDir, "workers", `${name}.sock`);
    await mkdir(path.dirname(socketPath), { recursive: true });
    await rm(socketPath, { force: true });
    return socketPath;
  }

  /**
   * Every tmux locator recorded for `issue`, in the order a spawn should prefer their windows:
   * what state records (the tree's root locator first, then each worker claim's -- see
   * `deps.issueLocators`), then the pane this runtime opened `issue`'s newest window with
   * (`issueWindows`, kept until that pane fails to verify). Once that launch's claim is
   * persisted, state records the same pane too -- as a copy, not the object `spawn` returned:
   * `/process/started` and `/worker/started` replace the stored locator with a spread of it --
   * so the entry is deduplicated by process identity (`sameProcess`: pane id, pid, start
   * ticks), and each pane is verified once.
   */
  private recordedLocators(issue: IssueKey): TmuxLocator[] {
    const found: TmuxLocator[] = [];
    for (const locator of this.deps.issueLocators(issue)) {
      if (locator.runtime === "tmux") found.push(locator);
    }
    const opened = this.issueWindows.get(issue);
    if (opened && !found.some((locator) => sameProcess(locator, opened))) found.push(opened);
    return found;
  }

  /**
   * The window a new pane for `issue` splits into: the first recorded window (see
   * `recordedLocators` for the order) that still holds a pane verifying as the process its
   * locator recorded. A live window alone proves nothing: after the private tmux server is
   * recreated, the same `@N` names some other issue's window, and the in-memory `issueWindows`
   * entry outlives the cleared locators that once pointed there -- splitting into it would put
   * this issue's worker in a stranger's window. When no recorded window verifies, returns
   * `undefined` and the caller opens a fresh one, recorded in `issueWindows` alone: a locator's
   * `tmuxWindowId` is a fact about where its pane lives and is never rewritten while that pane
   * may be live. A legacy identity-less locator therefore keeps naming its real window (so
   * `reconcileOrphans` keeps it known until that locator clears through its own probe), new
   * workers land in the fresh window, and a dead window's stale locators clear through their own
   * probes. A pane that could not be verified either way (`listing-failed`) is simply not reused:
   * opening a fresh window has no destructive consequence, unlike the verdicts `probe` and `stop`
   * refuse to fake. Any `issueWindows` entry that did not verify is dropped here -- including one
   * that merely could not be listed -- so nothing hands it out again; when a tmux hiccup, not a
   * dead pane, was the reason, the cost is one extra window for the issue (the pane itself is
   * untouched and its persisted locator still names its window), which is the cheap side of that
   * trade.
   */
  private async probedWindowId(issue: IssueKey): Promise<string | undefined> {
    for (const locator of this.recordedLocators(issue)) {
      if ((await this.verifyPaneProcess(locator)).verified) return locator.tmuxWindowId;
    }
    // Reaching here means the in-memory entry (if any) was among the panes that failed.
    this.issueWindows.delete(issue);
    return undefined;
  }

  /** Field 22 of `/proc/<pid>/stat`, or `undefined` when the process is gone (ENOENT/ESRCH). Any
   * other read error -- EACCES, EIO, EMFILE -- is a fault of this host, not evidence about the
   * process, and is rethrown so it crashes loud instead of every pane at once reading as "not
   * the recorded process". A malformed line throws from `parseProcStatStartTicks`: that is a bug,
   * never "dead". */
  private async readPaneStartTicks(pid: number): Promise<number | undefined> {
    let stat: string;
    try {
      stat = this.deps.readProcessStat
        ? await this.deps.readProcessStat(pid)
        : await readFile(`/proc/${pid}/stat`, "utf8");
    } catch (error) {
      if (isProcessGoneError(error)) return undefined;
      throw error;
    }
    return parseProcStatStartTicks(stat);
  }

  /** The identity every spawn records for a pane tmux just reported. Throws when its process is
   * already gone -- a launch failure through the caller's existing path (`ProcessManager`'s
   * launch-failure accounting); the pane closes itself with its process, so there is nothing left
   * to record or reap. */
  private async recordedPaneIdentity(
    paneId: string,
    pid: number,
    token: string
  ): Promise<{ panePid: number; paneStartTicks: number }> {
    const paneStartTicks = await this.readPaneStartTicks(pid);
    if (paneStartTicks === undefined) {
      throw new Error(
        `pane ${paneId} for ${token} exited before its process identity could be recorded (/proc/${pid}/stat unreadable)`
      );
    }
    return { panePid: pid, paneStartTicks };
  }

  /**
   * Is the process this locator recorded still this pane's process? Requires the pane's current
   * pid to equal the recorded `panePid`, its `/proc/<pid>/stat` start ticks to equal the recorded
   * `paneStartTicks`, then the OMP command-line check. A locator without identity (persisted
   * before the fields existed, or without a pane id at all) never verifies, and never touches
   * tmux. A `list-panes` that fails for a reason that does not prove the pane gone is
   * `listing-failed`: not a verdict about the pane at all, and every caller treats it as "cannot
   * verify" rather than as either alive or gone. `probe`, `stop`'s kill gate, and
   * `probedWindowId` all go through this.
   */
  private async verifyPaneProcess(locator: TmuxLocator): Promise<PaneVerdict> {
    if (
      locator.tmuxPaneId === undefined ||
      locator.panePid === undefined ||
      locator.paneStartTicks === undefined
    ) {
      return { verified: false, reason: "no-identity" };
    }
    const pane = await tmux.lookupPane(this.deps.tmux, locator.tmuxPaneId);
    if (pane.status === "failed") {
      return { verified: false, reason: "listing-failed", detail: pane.detail };
    }
    if (pane.status === "absent") return { verified: false, reason: "pane-gone" };
    const pid = pane.pid;
    if (pid !== locator.panePid) {
      return { verified: false, reason: "pid-mismatch", observedPid: pid };
    }
    const ticks = await this.readPaneStartTicks(pid);
    if (ticks === undefined)
      return { verified: false, reason: "stat-unreadable", observedPid: pid };
    if (ticks !== locator.paneStartTicks) {
      return {
        verified: false,
        reason: "start-mismatch",
        observedPid: pid,
        observedStartTicks: ticks,
      };
    }
    if (!(await this.isOmpPane(pid)))
      return { verified: false, reason: "not-omp", observedPid: pid };
    return { verified: true, pid, paneId: locator.tmuxPaneId };
  }

  /**
   * The private server outlives the daemon and hands every pane it opens two environment tables
   * beneath that pane's own `-e` pairs: its global table (the environment it was forked with — an
   * earlier daemon's, plus whatever the operator's `tmux.conf` set) and the daemon's session's
   * table (which tmux's default `update-environment` fills from every attaching client: the
   * operator's `SSH_AUTH_SOCK`, `SSH_CONNECTION`, `DISPLAY`, …). Empties the session's
   * `update-environment` first — before the session table is read, so an attach landing in
   * between cannot slip a copy in behind the read — then removes from both tables every variable
   * `paneEnv` does not carry and returns the removed names, sorted and de-duplicated. Names come
   * from `show-environment -s` parsed as a whole (`environmentNames`), so a multi-line value's
   * continuation line can never read as a name and nothing is probed per name. After the removals
   * each table is listed again and two invariants checked, throwing (names only) on a violation:
   * every allow-listed name that was present is still present, and no removed name remains — the
   * guard against any argv re-tokenisation surprise on the unset side (a name ending in `;` is
   * passed as `\;`, and this check is what proves it landed on the right entry). Nothing is
   * removed on a server this daemon forked itself (its global table is `paneEnv` by construction
   * plus tmux's own `PWD`/`SHLVL`; a fresh session table holds only unset markers) or when no
   * server is running; a server without the daemon's session still has its global table scrubbed
   * and only the session step skipped. Panes already open are untouched: a process's environment
   * is copied at exec. Boot calls this before the launch hold releases, so no pane opens into an
   * unscrubbed server.
   */
  async scrubServerEnvironment(paneEnv: NodeJS.ProcessEnv): Promise<string[]> {
    const removed = new Set<string>();
    const scrubTable = async (table: tmux.EnvironmentTable, names: readonly string[]) => {
      const kept: string[] = [];
      const dropped: string[] = [];
      for (const name of names) {
        if (Object.hasOwn(paneEnv, name) || Object.hasOwn(TMUX_OWN_GLOBALS, name)) {
          kept.push(name);
          continue;
        }
        await tmux.unsetEnvironment(this.deps.tmux, table, name);
        dropped.push(name);
        removed.add(name);
      }
      if (dropped.length === 0) return;
      const after = new Set(await tmux.environmentNames(this.deps.tmux, table));
      const missing = kept.filter((name) => !after.has(name));
      const lingering = dropped.filter((name) => after.has(name));
      if (missing.length > 0 || lingering.length > 0) {
        const where = table === undefined ? "global" : `session ${table.session}`;
        throw new Error(
          `tmux ${where} environment scrub did not land as intended` +
            (missing.length > 0
              ? `; allow-listed name(s) now missing: ${missing.join(", ")}`
              : "") +
            (lingering.length > 0 ? `; removed name(s) still present: ${lingering.join(", ")}` : "")
        );
      }
    };
    const globalNames = await tmux.environmentNames(this.deps.tmux, undefined);
    // No server on the daemon's socket: no table to scrub and no session to configure —
    // `openWindow` will fork one under `paneEnv` with `update-environment` already empty.
    if (globalNames === undefined) return [];
    await scrubTable(undefined, globalNames);
    const session = { session: this.deps.tmux.socket };
    if (await tmux.disableEnvironmentUpdates(this.deps.tmux, session.session)) {
      const sessionNames = await tmux.environmentNames(this.deps.tmux, session);
      if (sessionNames !== undefined) await scrubTable(session, sessionNames);
    }
    return [...removed].sort();
  }

  /** Alive only when `verifyPaneProcess` confirms the pane still runs the process the locator
   * recorded. A pane that is gone is `dead`/`gone`; a pane that is present but fails the identity
   * check -- a reissued id, or a legacy locator with nothing to verify -- is
   * `dead`/`not-recorded-process` with both identities in `detail`, so the caller can still ask
   * the locator's own process to shut down over its role-scoped socket and log what it decided.
   * A pane that could not be verified either way (`list-panes` itself failed for a reason that
   * does not prove the pane gone) throws: it is neither alive nor dead, and every caller's own
   * failure handling logs and retries instead of clearing anything. */
  async probe(locator: ControllerLocator): Promise<ProbeResult> {
    const tmuxLocator = this.tmuxLocator(locator);
    const verdict = await this.verifyPaneProcess(tmuxLocator);
    if (verdict.verified) return { status: "alive", pid: verdict.pid };
    if (verdict.reason === "pane-gone") return { status: "dead", reason: "gone" };
    if (verdict.reason === "listing-failed") {
      throw new Error(describePaneVerdict(tmuxLocator, verdict));
    }
    return {
      status: "dead",
      reason: "not-recorded-process",
      detail: describePaneVerdict(tmuxLocator, verdict),
    };
  }

  /** Is `pid` running OMP? Consulted only after `/proc/<pid>/stat` was just read, so the process
   * existed a moment ago; a cmdline that has vanished since (ENOENT/ESRCH) means it exited in
   * between and is not OMP any more. Any other read error crashes loud: it is a fault of this
   * host, never evidence about the pane. */
  private async isOmpPane(pid: number): Promise<boolean> {
    try {
      const cmdline = this.deps.readProcessCmdline
        ? await this.deps.readProcessCmdline(pid)
        : await readFile(`/proc/${pid}/cmdline`, "utf8");
      return cmdline.includes("omp");
    } catch (error) {
      if (isProcessGoneError(error)) return false;
      throw error;
    }
  }

  /** A raw dial of the locator's shim socket: never negotiates, never caches. */
  async connect(locator: Locator, timeoutMs?: number): Promise<WorkerRpcClient> {
    const tmuxLocator = this.tmuxLocator(locator);
    if (!tmuxLocator.socketPath) {
      throw new Error(`tmux locator ${tmuxLocator.tmuxWindowId} has no shim socket`);
    }
    return this.deps.connectWorkerRpc(tmuxLocator.socketPath, timeoutMs);
  }

  /**
   * Sends the shim a `{type:"shutdown"}` frame over a fresh connection so it closes the wrapped
   * OMP process's stdin, then waits up to `timeoutMs` for the shim's own socket to close before
   * falling back to killing the pane. Never SIGTERMs OMP. The dial deliberately never negotiates:
   * the shim intercepts the shutdown frame itself (`worker-shim.ts`) and never forwards it to
   * OMP, so it needs no RPC round-trip OMP must be free to answer — negotiating first would make
   * stopping a busy worker depend on that same busy worker responding to an unrelated RPC
   * command, exactly the grace period this exists to give it. Only a clean resolve of
   * `client.closed` is a confirmed graceful close and skips the kill; a socket error while
   * waiting is NOT proof the process exited (a reset proves nothing about the pane), so it is
   * treated exactly like a timeout — fall through to `client.close()` and the kill-pane attempt.
   * A dead/unreachable shim, a locator with no socket at all (the controller's interactive pane),
   * or `skipGraceful` (the caller already confirmed nothing live is there to ask) also skip
   * straight to the kill. The kill itself is gated on
   * `verifyPaneProcess`, and only a verified pane is ever killed:
   * - A pane whose process is not the one this locator recorded -- some other role's pane
   *   wearing a reissued id, or a locator with no identity to verify (persisted before identity
   *   tracking, or without a pane id at all) -- is never killed by this runtime. It is treated
   *   exactly like a pane already gone: the stop returns normally so the caller clears the
   *   locator, and the refusal is logged with both identities unless the caller's own probe
   *   already decided and logged it (`options.refuseKill`, the `ProbeResult` detail it saw). Once
   *   such a locator clears, an orphaned pane is an unrecorded shim pane `reconcileOrphans` reaps
   *   once it has been idle past the grace period (idle-bounded, never on a schedule: a legacy
   *   process that ignores the shutdown and keeps producing output stays until it goes quiet).
   * - A pane that could not be verified either way -- `list-panes` itself failed for a reason
   *   that does not prove the pane gone (a client killed by the runner's timeout, a server not
   *   responding) -- throws `ProcessStopFailed`, exactly like a `kill-pane` failure: the caller
   *   must never treat the process as stopped when it cannot confirm that.
   * Throws `ProcessStopFailed` for any `kill-pane` failure other than the pane having already been
   * reaped on its own or the private server itself not being there (`PANE_GONE_STDERR`: no server
   * on this daemon's own socket means no Legion pane exists).
   */
  async stop(
    locator: ControllerLocator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void> {
    const tmuxLocator = this.tmuxLocator(locator);
    const client =
      !options?.skipGraceful && tmuxLocator.socketPath
        ? await this.dialForShutdown(tmuxLocator.socketPath)
        : undefined;
    if (client) {
      const confirmed = await awaitShutdown(client, timeoutMs, this.deps.sleep);
      if (confirmed) return;
      client.close();
    }
    if (options?.refuseKill) return;
    const verdict = await this.verifyPaneProcess(tmuxLocator);
    if (verdict.verified) {
      const killed = await tmux.killPane(this.deps.tmux, verdict.paneId);
      if (killed.exitCode !== 0 && !PANE_GONE_STDERR.test(killed.stderr ?? "")) {
        throw new ProcessStopFailed(
          locator,
          `kill-pane ${verdict.paneId} exited ${killed.exitCode}${killed.stderr ? `: ${killed.stderr}` : ""}`
        );
      }
      return;
    }
    if (verdict.reason === "listing-failed") {
      throw new ProcessStopFailed(locator, describePaneVerdict(tmuxLocator, verdict));
    }
    if (verdict.reason !== "pane-gone") {
      console.error(
        `[legion] not killing pane ${tmuxLocator.tmuxPaneId ?? tmuxLocator.tmuxWindowId}, treating it as already gone: ${describePaneVerdict(tmuxLocator, verdict)}`
      );
    }
  }

  /** Connects to a shim purely to send `{type:"shutdown"}`. Returns `undefined` only when the
   * socket itself is unreachable (the shim is already gone). */
  private async dialForShutdown(socketPath: string): Promise<WorkerRpcClient | undefined> {
    try {
      return await this.deps.connectWorkerRpc(socketPath, this.deps.workerRpcTimeoutMs());
    } catch {
      return undefined;
    }
  }

  /**
   * Kills every `@legion_owner`-marked tmux window `known` does not name and that has been idle
   * at least `graceMs` — long enough that a window mid-creation (activity not yet recorded) is
   * never mistaken for an orphan. Boot calls this with `graceMs: 0`: it never trusts a window it
   * did not itself record, so there is no such race to protect against there. Also reaps
   * unrecorded worker-shim *panes* split into a window `known` DOES still name — a window-level
   * orphan check alone can never see these: a locator is persisted only after its pane opens, so
   * a crash in that exact gap leaves a real, running process inside a window nothing else ever
   * flags as unowned.
   *
   * `known` carries `locatorHandles` (runtime.ts): a window id, a pane id, or `<windowId>/*` —
   * the last for a locator with a known window but no recorded `tmuxPaneId`, whose whole window
   * is exempted from the pane-level pass entirely: every pane in it (including the owner's own,
   * since we cannot tell which one it is without the id) would otherwise look exactly like the
   * unrecorded-crash-window-orphan this pass exists to catch, and killing it would kill a live
   * root/controller/worker the daemon itself is still actively running. A locator without a pane
   * id also carries no process identity, so `probe` never confirms it alive: its first probe
   * retires or resurrects it onto a fully-recorded locator, and this exemption — and the
   * ambiguity it accepts — shrinks to nothing as those legacy locators clear.
   */
  async reconcileOrphans(known: ReadonlySet<string>, graceMs: number): Promise<void> {
    const session = this.deps.tmux.socket;
    const owner = session;
    const knownWindows = new Set<string>();
    const knownPanes = new Set<string>();
    const exemptWindows = new Set<string>();
    for (const handle of known) {
      if (handle.startsWith("%")) knownPanes.add(handle);
      else if (handle.endsWith("/*")) exemptWindows.add(handle.slice(0, -"/*".length));
      else knownWindows.add(handle);
    }
    const unknownWindows = await tmux.listUnknownOwnedWindows(
      this.deps.tmux,
      session,
      owner,
      knownWindows
    );
    for (const { windowId, activityAt } of unknownWindows) {
      if (this.deps.now() - activityAt < graceMs) continue;
      await tmux.killWindow(this.deps.tmux, windowId);
    }

    const unknownPanes = await tmux.listUnknownPanes(this.deps.tmux, owner, knownPanes);
    for (const { paneId, windowId, activityAt } of unknownPanes) {
      if (exemptWindows.has(windowId)) continue;
      if (this.deps.now() - activityAt < graceMs) continue;
      await tmux.killPane(this.deps.tmux, paneId);
    }
  }

  private tmuxLocator(locator: ControllerLocator): TmuxLocator {
    if (locator.runtime === "kubernetes") {
      throw new Error(
        "tmux runtime cannot operate a kubernetes or operator-launched controller locator"
      );
    }
    return locator;
  }
}
