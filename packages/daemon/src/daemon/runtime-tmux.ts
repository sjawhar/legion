import { createHash } from "node:crypto";
import { mkdir, readFile, rm } from "node:fs/promises";
import path from "node:path";
import { controllerToken, type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import { parseProcStatStartTicks } from "./proc-stat";
import {
  awaitShutdown,
  DAEMON_CLI_ENTRYPOINT,
  type Locator,
  type ProbeResult,
  ProcessStopFailed,
  type Runtime,
  type SpawnSpec,
  sameProcess,
  shellPath,
  type TmuxLocator,
} from "./runtime";
import { writeSecretFile } from "./secrets";
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
 * entirely here, never in the interface: `ProcessManager` only ever sees opaque locators.
 *
 * A pane id is never taken as proof of the process a locator recorded. A recreated tmux server
 * hands out ids from `%1` again, so a stale locator's id can name some other role's live pane --
 * one running OMP, so a command-line check alone passes. Every locator this runtime writes
 * therefore records the pane's root pid and that process's `/proc/<pid>/stat` start ticks at
 * launch, and `verifyPaneProcess` -- the one chokepoint -- re-checks pid, then start ticks, then
 * OMP before `probe` reports a pane alive, before `stop` kills it, and before `probedWindowId`
 * splits a new pane into its window.
 */
export class TmuxRuntime implements Runtime {
  /** Serializes tmux window creation per issue, so two concurrent spawns never each see "no
   * window yet" and open two. */
  private readonly issueLaunchQueue = new Map<IssueKey, Promise<unknown>>();
  /** The pane this runtime most recently opened a fresh window with for an issue, identity
   * included: one more candidate for `probedWindowId`, so a concurrent second spawn on the same
   * issue (or any later one, while every persisted locator still names a window nothing
   * verifies in) can verify that pane and split into its window instead of racing to open its
   * own. Never trusted by window id alone -- after a tmux server recreate the same `@N` can name
   * some other issue's window, so an entry whose pane no longer verifies is dropped. */
  private readonly issueWindows = new Map<IssueKey, TmuxLocator>();

  constructor(private readonly deps: TmuxRuntimeDeps) {}

  /**
   * Opens (or splits into) the tmux window for the spec's issue — or the controller's own
   * window — running `legion worker-shim` around the caller's inner OMP command. Delivers the
   * spec's one secret as a 0600 file at `<stateDir>/secrets/<role token>` and exports only its
   * `<NAME>_FILE` path, appended after the spec's own env pairs; the write happens before any
   * tmux call, so an fs failure is an ordinary launch failure. The caller owns that file's
   * lifetime (hold/prune) — this method only writes it.
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
      const window = await tmux.openWindow(
        this.deps.tmux,
        session,
        treeName(issue),
        paneArgv,
        session
      );
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

  private async spawnController(
    spec: SpawnSpec,
    token: string,
    secret: [string, string]
  ): Promise<TmuxLocator> {
    const { socketPath, paneArgv } = await this.preparePane("controller", spec, token, secret);
    const session = this.deps.tmux.socket;
    const window = await tmux.openWindow(this.deps.tmux, session, "controller", paneArgv, session);
    const identity = await this.recordedPaneIdentity(window.paneId, window.pid, token);
    return {
      runtime: "tmux",
      tmuxSession: session,
      tmuxWindowId: window.windowId,
      tmuxPaneId: window.paneId,
      socketPath,
      ...identity,
    };
  }

  /** Everything a new pane needs before any tmux call, in the order every spawn performs it: a
   * fresh shim socket path (its directory made, a stale socket removed), the process's one secret
   * written as a 0600 file, and the pane argv — the spec's env pairs, the secret's `<NAME>_FILE`
   * pointer, then the `legion worker-shim --socket <path> -- <inner>` command every Legion OMP
   * process (root, phase worker, controller) runs inside its pane. */
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
   * `paneEnv` does not carry and returns the removed names, sorted and de-duplicated. A candidate
   * the table dump yielded is confirmed against the table (`environmentHas`) before it is unset or
   * recorded: a continuation line of a multi-line value can look like `NAME=` in that dump, and a
   * fragment of a value must never reach the boot log. Nothing is removed on a server this daemon
   * forked itself (its global table is `paneEnv` by construction plus tmux's own `PWD`/`SHLVL`; a
   * fresh session table holds only `-NAME` unset markers) or when no server is running; a server
   * without the daemon's session still has its global table scrubbed and only the session step
   * skipped. Panes already open are untouched: a process's environment is copied at exec. Boot
   * calls this before the launch hold releases, so no pane opens into an unscrubbed server.
   */
  async scrubServerEnvironment(paneEnv: NodeJS.ProcessEnv): Promise<string[]> {
    const removed = new Set<string>();
    const unsetForbidden = async (table: tmux.EnvironmentTable, candidates: readonly string[]) => {
      for (const name of candidates) {
        if (Object.hasOwn(paneEnv, name) || Object.hasOwn(TMUX_OWN_GLOBALS, name)) continue;
        if (!(await tmux.environmentHas(this.deps.tmux, table, name))) continue;
        await tmux.unsetEnvironment(this.deps.tmux, table, name);
        removed.add(name);
      }
    };
    const globalCandidates = await tmux.environmentCandidates(this.deps.tmux, undefined);
    // No server on the daemon's socket: no table to scrub and no session to configure —
    // `openWindow` will fork one under `paneEnv` with `update-environment` already empty.
    if (globalCandidates === undefined) return [];
    await unsetForbidden(undefined, globalCandidates);
    const session = { session: this.deps.tmux.socket };
    if (await tmux.disableEnvironmentUpdates(this.deps.tmux, session.session)) {
      const sessionCandidates = await tmux.environmentCandidates(this.deps.tmux, session);
      if (sessionCandidates !== undefined) await unsetForbidden(session, sessionCandidates);
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
  async probe(locator: Locator): Promise<ProbeResult> {
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
   * A dead/unreachable shim, or `skipGraceful` (the caller already confirmed nothing live is
   * there to ask), also skip straight to the kill. The kill itself is gated on
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
    locator: Locator,
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

  private tmuxLocator(locator: Locator): TmuxLocator {
    if (locator.runtime === "kubernetes") {
      throw new Error("tmux runtime cannot operate a kubernetes locator");
    }
    return locator;
  }
}
