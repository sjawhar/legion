import { createHash } from "node:crypto";
import { mkdir, readFile, rm } from "node:fs/promises";
import path from "node:path";
import { controllerToken, type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import {
  awaitShutdown,
  DAEMON_CLI_ENTRYPOINT,
  type Locator,
  type ProbeResult,
  ProcessStopFailed,
  type Runtime,
  type SpawnSpec,
  shellPath,
  type TmuxLocator,
} from "./runtime";
import { writeSecretFile } from "./secrets";
import type { TmuxServer } from "./tmux";
import * as tmux from "./tmux";
import type { WorkerRpcClient } from "./worker-rpc";

/** `kill-pane` stderr shapes that mean "this pane is already gone" rather than "the kill
 * failed": the pane reaped itself; the private server exited (socket file left behind); or the
 * private socket was never created — on the daemon's own `-L legion-<project>` socket, no server
 * means no Legion pane. Every other non-zero exit is a real `ProcessStopFailed`. */
const PANE_GONE_STDERR =
  /can't find pane|no server running|error connecting to .*\(No such file or directory\)/;

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

/** Flattens an env record into repeated `-e KEY=VALUE` pairs for tmux; `undefined` values are omitted. */
function tmuxEnv(env: Record<string, string | undefined>): string[] {
  return Object.entries(env).flatMap(([key, value]) =>
    value === undefined ? [] : ["-e", `${key}=${value}`]
  );
}

function serialize<T>(
  queue: Map<string, Promise<unknown>>,
  key: string,
  fn: () => Promise<T>
): Promise<T> {
  const previous = queue.get(key) ?? Promise.resolve();
  const gated = previous.then(fn, fn);
  queue.set(
    key,
    gated.then(
      () => undefined,
      () => undefined
    )
  );
  return gated;
}

export interface TmuxRuntimeDeps {
  /** The private server (`{ run, socket: `legion-${project}` }`); `socket` doubles as the
   * session name and the `@legion_owner` value. */
  tmux: TmuxServer;
  /** Names each process's secret file: `roleToken(project, issue, role)` / `controllerToken(project)`. */
  project: string;
  /** `<stateDir>/workers/<name>.sock` and `<stateDir>/secrets/<token>`. */
  stateDir: string;
  connectWorkerRpc(socketPath: string, timeoutMs?: number): Promise<WorkerRpcClient>;
  /** The timeout `stop`'s own no-negotiate dial uses. */
  workerRpcTimeoutMs(): number;
  /** `reconcileOrphans`' grace clock. */
  now(): number;
  /** The locators state currently records for `issue` — the tree's first (when `issue` is a tree
   * root), then every role claim's — so a spawn can share the issue's window and a fresh window
   * can be written back onto them in place. */
  issueLocators(issue: IssueKey): readonly Locator[];
  /** Saves state after `probe` backfills a locator's pane id in place. */
  persist(): Promise<void>;
  readProcessCmdline?(pid: number): Promise<string>;
  /** Bounds a graceful stop's wait; overridable for tests, defaults to a real timer. */
  sleep?(ms: number): Promise<void>;
}

/**
 * The tmux implementation of `Runtime`: every Legion process is a `legion worker-shim` pane on
 * the daemon's private tmux server. One window per issue — the issue's first process (root
 * architect or phase worker alike) opens a window named for the issue and every later process
 * on that issue splits into it; the controller has a window of its own. That window sharing is
 * tmux's model and lives entirely here, never in the interface: `ProcessManager` only ever sees
 * opaque locators.
 */
export class TmuxRuntime implements Runtime {
  /** Serializes tmux window creation per issue, so two concurrent spawns never each see "no
   * window yet" and open two. */
  private readonly issueLaunchQueue = new Map<IssueKey, Promise<unknown>>();
  /** A just-opened window for an issue with no persisted claim yet (its first-ever worker, still
   * mid-launch): `recordedWindowId` falls back to this so a concurrent second spawn on the same
   * issue splits into it instead of racing to open its own. */
  private readonly issueWindowIds = new Map<IssueKey, string>();

  constructor(private readonly deps: TmuxRuntimeDeps) {}

  /**
   * Opens (or splits into) the tmux window for the spec's issue — running `legion worker-shim`
   * around the caller's inner OMP command — or the controller's own window, where the inner
   * command runs bare: the controller is an interactive OMP terminal session Sami can attach to,
   * with no shim and no socket. Delivers the spec's one secret as a 0600 file at
   * `<stateDir>/secrets/<role token>` and exports only its `<NAME>_FILE` path, appended after the
   * spec's own env pairs; the write happens before any tmux call, so an fs failure is an
   * ordinary launch failure. The caller owns that file's lifetime (hold/prune) — this method only
   * writes it.
   */
  async spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator> {
    const [secret, ...extraSecrets] = Object.entries(spec.secrets);
    if (!secret || extraSecrets.length > 0) {
      throw new Error("tmux runtime delivers exactly one secret per process");
    }
    if (kind === "controller") {
      return this.spawnController(spec, controllerToken(this.deps.project), secret);
    }
    if (!spec.issue) throw new Error(`spawn ${kind} requires spec.issue`);
    if (spec.role === "controller") {
      throw new Error(`spawn ${kind} requires a Legion role, not "controller"`);
    }
    return this.spawnIssueProcess(
      spec.issue,
      spec.role,
      spec,
      roleToken(this.deps.project, spec.issue, spec.role),
      secret
    );
  }

  private async spawnIssueProcess(
    issue: IssueKey,
    role: LegionRole,
    spec: SpawnSpec,
    token: string,
    secret: [string, string]
  ): Promise<TmuxLocator> {
    const { socketPath, paneArgv } = await this.preparePane(
      workerSocketBasename(issue, role),
      spec,
      token,
      secret
    );
    const session = this.deps.tmux.socket;
    const { tmuxWindowId, tmuxPaneId } = await serialize(this.issueLaunchQueue, issue, async () => {
      const existingWindowId = await this.probedWindowId(issue);
      if (existingWindowId) {
        const { paneId } = await tmux.splitWindow(this.deps.tmux, existingWindowId, paneArgv);
        return { tmuxWindowId: existingWindowId, tmuxPaneId: paneId };
      }
      const window = await tmux.openWindow(
        this.deps.tmux,
        session,
        treeName(issue),
        paneArgv,
        session
      );
      this.rewriteIssueWindowId(issue, window.windowId);
      return { tmuxWindowId: window.windowId, tmuxPaneId: window.paneId };
    });
    return { runtime: "tmux", tmuxSession: session, tmuxWindowId, tmuxPaneId, socketPath };
  }

  /** The controller's window: the inner command bare in its pane — no `legion worker-shim`, so
   * the locator carries no `socketPath` and `connect` refuses it; `stop` falls straight through
   * to the pane kill and `probe` reads the pane like any other. */
  private async spawnController(
    spec: SpawnSpec,
    token: string,
    [secretName, secretValue]: [string, string]
  ): Promise<TmuxLocator> {
    const shellCommand = `cd ${shellPath(spec.workspaceDir)} && ${spec.innerCommand}`;
    const secretFile = await writeSecretFile(this.deps.stateDir, token, secretValue);
    const paneArgv = [
      ...tmuxEnv(spec.env),
      ...tmuxEnv({ [`${secretName}_FILE`]: secretFile }),
      shellCommand,
    ];
    const session = this.deps.tmux.socket;
    const window = await tmux.openWindow(this.deps.tmux, session, "controller", paneArgv, session);
    return {
      runtime: "tmux",
      tmuxSession: session,
      tmuxWindowId: window.windowId,
      tmuxPaneId: window.paneId,
    };
  }

  /** Everything a new issue pane needs before any tmux call, in the order every spawn performs
   * it: a fresh shim socket path (its directory made, a stale socket removed), the process's one
   * secret written as a 0600 file, and the pane argv — the spec's env pairs, the secret's
   * `<NAME>_FILE` pointer, then the `legion worker-shim --socket <path> -- <inner>` command every
   * headless Legion OMP process (root architect, phase worker) runs inside its pane. */
  private async preparePane(
    socketName: string,
    spec: SpawnSpec,
    token: string,
    [secretName, secretValue]: [string, string]
  ): Promise<{ socketPath: string; paneArgv: string[] }> {
    const socketPath = await this.prepareSocket(socketName);
    const shellCommand = `cd ${shellPath(spec.workspaceDir)} && ${shellPath(process.execPath)} ${shellPath(DAEMON_CLI_ENTRYPOINT)} worker-shim --socket ${shellPath(socketPath)} -- ${spec.innerCommand}`;
    const secretFile = await writeSecretFile(this.deps.stateDir, token, secretValue);
    const pairs = [...tmuxEnv(spec.env), ...tmuxEnv({ [`${secretName}_FILE`]: secretFile })];
    return { socketPath, paneArgv: [...pairs, shellCommand] };
  }

  private async prepareSocket(name: string): Promise<string> {
    const socketPath = path.join(this.deps.stateDir, "workers", `${name}.sock`);
    await mkdir(path.dirname(socketPath), { recursive: true });
    await rm(socketPath, { force: true });
    return socketPath;
  }

  private recordedWindowId(issue: IssueKey): string | undefined {
    for (const locator of this.deps.issueLocators(issue)) {
      if (locator.runtime === "tmux") return locator.tmuxWindowId;
    }
    return this.issueWindowIds.get(issue);
  }

  /**
   * After opening a fresh window for `issue` (first spawn, or a fallback from a dead recorded
   * window), every locator state records for that issue must point at the new window id in the
   * same place, or `recordedWindowId` keeps handing a later spawn a stale id and each one opens
   * yet another window instead of splitting into it.
   */
  private rewriteIssueWindowId(issue: IssueKey, windowId: string): void {
    this.issueWindowIds.set(issue, windowId);
    for (const locator of this.deps.issueLocators(issue)) {
      if (locator.runtime === "tmux") locator.tmuxWindowId = windowId;
    }
  }

  /** Trusts no recorded window id until it is confirmed live, so a human-killed window falls back to a fresh one. */
  private async probedWindowId(issue: IssueKey): Promise<string | undefined> {
    const candidate = this.recordedWindowId(issue);
    if (!candidate) return undefined;
    return (await tmux.windowAlive(this.deps.tmux, candidate)) ? candidate : undefined;
  }

  /** Probes a locator's pane for a live OMP process. Backfills `locator.tmuxPaneId` in place
   * (and persists) once confirmed alive if it was never recorded (state predating the field, or
   * any other pane-id-less write) — see `reconcileOrphans` for why a locator missing its own pane
   * id exempts its whole window from pane-level reaping; this is what shrinks that exemption to
   * nothing over time. */
  async probe(locator: Locator): Promise<ProbeResult> {
    const tmuxLocator = this.tmuxLocator(locator);
    const target = tmuxLocator.tmuxPaneId ?? tmuxLocator.tmuxWindowId;
    const pid = await tmux.panePid(this.deps.tmux, target);
    if (pid === undefined) return { status: "dead" };
    if (!(await this.isOmpPane(pid))) return { status: "dead" };
    if (tmuxLocator.tmuxPaneId === undefined) {
      const paneId = await tmux.firstPaneId(this.deps.tmux, tmuxLocator.tmuxWindowId);
      if (paneId !== undefined) {
        tmuxLocator.tmuxPaneId = paneId;
        await this.deps.persist();
      }
    }
    return { status: "alive", pid };
  }

  private async isOmpPane(pid: number): Promise<boolean> {
    if ((await this.deps.tmux.run(["kill", "-0", String(pid)])).exitCode !== 0) return false;
    try {
      const cmdline = this.deps.readProcessCmdline
        ? await this.deps.readProcessCmdline(pid)
        : await readFile(`/proc/${pid}/cmdline`, "utf8");
      return cmdline.includes("omp");
    } catch {
      return false;
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
   * A dead/unreachable shim, or `skipGraceful` (the caller already confirmed nothing live is
   * there to ask), also skip straight to the kill. Every real locator carries a pane id (`spawn`
   * always records one); a locator without one is a corrupt or legacy record, not a case to
   * silently degrade for. Throws `ProcessStopFailed` for any `kill-pane` failure other than the
   * pane having already been reaped on its own (`"can't find pane"`) or the private server itself
   * not being there (`"no server running"` — the socket exists but its server exited; `"error
   * connecting to … (No such file or directory)"` — the socket was never created, the shape a
   * first boot after the upgrade runbook or a reboot that cleared `TMUX_TMPDIR` produces): no
   * server on this daemon's own socket means no Legion pane exists. See `PANE_GONE_STDERR`. The
   * caller must never treat the process as stopped when it cannot confirm that.
   */
  async stop(
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean }
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
    if (!tmuxLocator.tmuxPaneId) {
      throw new Error(`tmux locator for window ${tmuxLocator.tmuxWindowId} is missing a pane id`);
    }
    const killed = await tmux.killPane(this.deps.tmux, tmuxLocator.tmuxPaneId);
    if (killed.exitCode !== 0 && !PANE_GONE_STDERR.test(killed.stderr ?? "")) {
      throw new ProcessStopFailed(
        locator,
        `kill-pane ${tmuxLocator.tmuxPaneId} exited ${killed.exitCode}${killed.stderr ? `: ${killed.stderr}` : ""}`
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
   * root/controller/worker the daemon itself is still actively running. `probe` backfills the
   * pane id the next time it confirms that locator alive, so this exemption — and the ambiguity
   * it accepts — shrinks to nothing as every surviving locator gets its pane id recorded.
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

  private tmuxLocator(locator: Locator): TmuxLocator {
    if (locator.runtime === "kubernetes") {
      throw new Error("tmux runtime cannot operate a kubernetes locator");
    }
    return locator;
  }
}
