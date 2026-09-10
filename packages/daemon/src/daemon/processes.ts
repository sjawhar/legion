import { createHash } from "node:crypto";
import { mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  type LegionRole,
  parseRoleToken,
  roleToken,
  roleTopic,
  type SpawnWorkerResponse,
  sanitizeToken,
} from "@legion/contracts";
import { provisionIssueWorkspace, type WorkspaceSpec } from "@legion/workspace";
import type { CommandRunnerOptions } from "../state/fetch";
import { secretHash } from "./api/auth";
import { rootForIssue as resolveRootForIssue } from "./api/context";
import { createCancellableSleep } from "./cancellable-sleep";
import { type WorkerCatchupDeps, workerCatchup } from "./catchup";
import type { DaemonConfig } from "./config";
import { type DispatchClient, writeStatus } from "./dispatch-client";
import type { ExceptionInfo } from "./events";
import type { LegionState, TreeState, WorkerLocator, WorkerRoleClaim } from "./legion-state";
import { StopFailed, TreeClosingError } from "./process-errors";
import * as tmux from "./tmux";
import { MAX_LAUNCH_FAILURES, WorkerAdmission } from "./worker-admission";
import { WorkerBootWatchdog } from "./worker-boot-watchdog";
import { probeWorkerSocket, type WorkerRpcClient } from "./worker-rpc";

const HOUR_MS = 60 * 60 * 1000;

const EXTENSION_PACKAGE = path.resolve(import.meta.dir, "../../../pi-envoy");
const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../cli/index.ts");

/**
 * Wraps a `saveState` rejection that occurs after `spawnTree` has already
 * succeeded (a real tmux window was running when the save was attempted).
 * Distinguishes this from a genuine launch failure so `startRoot` never
 * rolls back generation/status/launchFailures or requeues on it — that
 * spawn is not the problem. The window itself is killed before this is
 * thrown (see `spawnRoot`), so nothing is left running unrecorded; the
 * failure is left to propagate so a caller running this inside a durable
 * transaction (the linger effect's promotion, via
 * `advancePromotionSweep`/`startRoot`) fails and goes fatal, consistent
 * with every other durable effect whose post-mutation save fails.
 */
class SpawnPersistenceFailure extends Error {
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause));
    this.name = "SpawnPersistenceFailure";
  }
}

type Redelivery = { topic: string; payload: string; eventId: string };

export type ControlDirective =
  | { type: "reclaim-architect"; issue: IssueKey; redeliver: Redelivery }
  | { type: "shutdown" };

export interface ProcessManagerDeps {
  state: LegionState;
  saveState(): Promise<void>;
  config: DaemonConfig;
  ompInvocation: string;
  panePath: string;
  credentialHelper: string;
  run(
    cmd: string[],
    options?: CommandRunnerOptions
  ): Promise<{ stdout: string; stderr?: string; exitCode: number }>;
  natsPublish(subject: string, json: string): void;
  natsRequest(subject: string, json: string): Promise<string>;
  mintControllerCapability(): Promise<string>;
  mintBootToken(tree: IssueKey, generation: number): Promise<string>;
  mintWorkerBootToken(
    tree: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    generation: number,
    expectedSessionId?: string
  ): Promise<string>;
  connectWorkerRpc(socketPath: string): Promise<WorkerRpcClient>;
  provisioningToken(owner: string): Promise<string>;
  statPrompt?(promptPath: string): Promise<unknown>;
  readProcessCmdline?(pid: number): Promise<string>;
  /** Used to bound the wait for any process's graceful shutdown — a single worker's own
   * retirement, or every process under a closing tree — before it is killed outright.
   * Overridable for tests; defaults to a real timer. */
  sleep?(ms: number): Promise<void>;
  /** Overridable for tests; defaults to a real macrotask boundary (`setTimeout(fn, 0)`). The
   * boot watchdog calls this once per re-arm — see `WorkerBootWatchdog.arm`'s doc comment for
   * why a real timer, not merely another microtask, matters here even under an injected fake
   * clock/`sleep`. */
  yield?(): Promise<void>;
  /** Overridable for tests only, to observe the ordering of `WorkerAdmission`'s own
   * reservation-release and queue-drain-trigger relative to the `launchWorker`/
   * `promptExistingWorker` call preceding them — see `WorkerAdmissionDeps.onAdmissionEvent`'s
   * doc comment. Threaded straight through in the constructor below; production never
   * supplies this. */
  onAdmissionEvent?(token: string, event: "reservation-released" | "queue-drain-triggered"): void;
  workerCatchup: WorkerCatchupDeps;
  dispatchClient: DispatchClient;
  /** Invalidates a session's daemon-minted capability the moment its process is observed dead, so a stale credential file cannot keep minting grants until a respawn overwrites it. */
  revokeSessionCapability(sessionId: string): void;
  now(): number;
}

const MAX_TMUX_WINDOW_NAME_LENGTH = 160;
const TMUX_RECONCILIATION_GRACE_MS = 120_000;

/** Thrown by `spawnWorker`/`workerReady` when the tree is mid-teardown (tracked only in the
 * in-memory `closingTrees` map `closeTree` populates before awaiting any stop) — a route
 * handler translates this into an HTTP 409, distinct from any other error these methods can
 * throw. */
export { StopFailed, TreeClosingError } from "./process-errors";

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

/** Builds the second `--append-system-prompt` fragment every root and phase-worker process gets,
 * so the model can address the architect (and derive a sibling's topic) without hand-encoding a
 * `roleToken` itself — the encoding escapes `_`/`.`/`-` and a hand-built token silently misses. */
export function addressingFragment(
  project: string,
  treeKey: IssueKey,
  issue: IssueKey,
  role: LegionRole
): string {
  const ownTopic = roleTopic(roleToken(project, issue, role));
  const architectTopic = roleTopic(roleToken(project, treeKey, "architect"));
  return (
    `Legion addressing: your role topic is \`${ownTopic}\`; your tree's architect is ` +
    `\`${architectTopic}\`; a sibling role on your issue is your topic with the trailing ` +
    "`-<role>` replaced."
  );
}

function shellPath(value: string): string {
  return /[^A-Za-z0-9_./:-]/.test(value) ? `'${value.replaceAll("'", "'\\''")}'` : value;
}
/** Flattens an env record into repeated `-e KEY=VALUE` pairs for tmux; `undefined` values are omitted. */
function tmuxEnv(env: Record<string, string | undefined>): string[] {
  return Object.entries(env).flatMap(([key, value]) =>
    value === undefined ? [] : ["-e", `${key}=${value}`]
  );
}
export function daemonCredentialHelper(
  runtime = process.execPath,
  entrypoint = DAEMON_CLI_ENTRYPOINT
): string {
  if (!path.isAbsolute(runtime) || !path.isAbsolute(entrypoint)) {
    throw new Error("Legion credential helper requires absolute runtime and CLI paths");
  }
  return `!${shellPath(runtime)} ${shellPath(entrypoint)} credential`;
}
function controlReplyType(raw: string): "ack" | "nack" {
  const payload: unknown = JSON.parse(raw);
  if (
    typeof payload !== "object" ||
    payload === null ||
    !("type" in payload) ||
    (payload.type !== "ack" && payload.type !== "nack")
  ) {
    throw new Error("Invalid Legion control directive reply");
  }
  return payload.type;
}

/** Starts and supervises only the tmux trees whose locator it records in Legion state. */
export class ProcessManager {
  private readonly resurrecting = new Map<IssueKey, Promise<void>>();
  private readonly workerClients = new Map<string, WorkerRpcClient>();
  private readonly workerConnections = new Map<string, Promise<WorkerRpcClient>>();
  /** Serializes tmux window creation per issue, so two concurrent spawns never each see "no
   * window yet" and open two. */
  private readonly issueLaunchQueue = new Map<IssueKey, Promise<unknown>>();
  /** Tracks, per `<token>:<generation>`, whether a worker's socket close has already spent its
   * one reconnect attempt for that generation — `onWorkerClientClosed` consults this so a
   * reconnect's own close goes straight to `markWorkerDead` instead of chaining into another
   * reconnect attempt (no loop across successive closes). Never pruned: grows by one entry per
   * generation a worker's socket ever closes, for the process's lifetime. */
  private readonly reconnectAttempted = new Set<string>();
  /** A just-opened window for an issue with no persisted claim yet (its first-ever worker, still
   * mid-launch): recordedWindowId falls back to this so a concurrent second spawn on the same
   * issue splits into it instead of racing to open its own. */
  private readonly issueWindowIds = new Map<IssueKey, string>();
  private controllerSpawn?: Promise<void>;
  /** Set while a bounded wait for the controller to claim its role is in flight (see
   * `ensureController`'s doc comment). Bound to the exact locator observed when armed, by
   * reference: the expiry callback only ever acts if `controllerLocator` is still this same
   * object -- a fresh spawn or an explicit cancel replaces or clears this field first, so a
   * stale timer that still fires late can never touch whatever now occupies the role. `cancel`
   * releases the underlying real timer (a no-op when a test's injected `sleep` stands in for
   * one; the identity check above is what actually neutralizes a stale fire in that case). */
  private controllerRegistrationWait?: {
    locator: NonNullable<LegionState["controllerLocator"]>;
    cancel: () => void;
  };
  private promotionSweep?: { attempted: Set<IssueKey>; inFlight: number };
  /** Every in-flight `startRoot` call, including ones fired without being awaited (`admit`'s promotion). `drainSpawns` awaits these so `stop()`'s final save observes each spawn's own persisted state instead of racing it. */
  private readonly spawns = new Set<Promise<void>>();
  /** Owns the running-worker cap: admission decisions, the FIFO queue, the reservation set, and
   * the promotion drain. See `worker-admission.ts` for the full design. */
  private readonly workerAdmission: WorkerAdmission;
  /** In-memory only, never persisted: a tree currently being torn down by `closeTree`. A save
   * during the up-to-60s teardown window must never durably record a mid-close state a boot
   * cannot resume -- the durable status stays whatever it was (`active`/`lingering`) until the
   * final `closed` save, or (on a stop failure) `lingering` with a fresh `lingerUntil` for the
   * periodic sweep to retry. Also makes `closeTree` idempotent: a second call for the same tree
   * while one is already running awaits the same in-flight promise instead of racing it. */
  private readonly closingTrees = new Map<IssueKey, Promise<void>>();
  /** In-flight `launchWorker` calls, per tree, removed on settle regardless of outcome. Once
   * `closingTrees` names a tree no new launch can start for it (`spawnWorker`'s entry and
   * in-queue checks both throw `TreeClosingError` first) -- so `closeTreeLocked`'s fixed-point
   * loop awaits this set before every re-snapshot of `state.roles`, guaranteeing a launch that
   * raced past the fence just before the tree started closing is never invisible to a snapshot
   * taken while it is still mid-flight. */
  private readonly inFlightLaunches = new Map<IssueKey, Set<Promise<unknown>>>();
  /** Owns the per-role-token boot watchdog: watches a freshly-launched worker's boot against
   * `workerBootTimeoutSeconds`, probing liveness before ever retiring an unconfirmed boot. See
   * `worker-boot-watchdog.ts` for the full design. */
  private readonly bootWatchdog: WorkerBootWatchdog;

  /**
   * The stop hierarchy every graceful-shutdown path funnels through, from lowest level up:
   * `stopProcess` (the one implementation: shim shutdown frame, race against a timeout, kill-pane
   * fallback) is wrapped by `stopProcessSerialized` (acquires the token's `WorkerAdmission`
   * critical section first, for a caller not already running inside it) or called raw by
   * `retireWorkerLocator` (for a caller — `launchWorker`, `markWorkerDeadLocked` — already running
   * inside that same section, where re-acquiring it would deadlock). `markWorkerDeadLocked` is
   * itself reached two ways: directly by `markWorkerDead` (a boot-time reconnect failure or a
   * runtime socket close), or via `WorkerAdmissionDeps.retireDeadClaim` (the prompt-failure
   * circuit breaker retiring a persistently-broken but still-queued worker).
   */
  constructor(private readonly deps: ProcessManagerDeps) {
    this.workerAdmission = new WorkerAdmission({
      state: deps.state,
      config: deps.config,
      getWorkerClient: (token) => this.workerClients.get(token),
      persist: () => this.persist(),
      publishArchitect: (treeKey, payload) => this.publishArchitect(treeKey, payload),
      // Wrapped in trackLaunch so a promotion-triggered launch (drainWorkerQueue ->
      // promoteQueuedWorker -> here) is just as visible to closeTreeLocked's inFlightLaunches
      // fixed point as a direct spawnWorker launch is -- see launchWorker's own entry check for
      // the other half of this fence (closingTrees, re-checked before this call ever opens a
      // pane).
      launchWorker: (treeKey, issue, role, claim, task) =>
        this.trackLaunch(treeKey, () => this.launchWorker(treeKey, issue, role, claim, task)),
      promptExistingWorker: (client, token, issue, role, sessionId, task) =>
        this.promptExistingWorker(client, token, issue, role, sessionId, task),
      retireDeadClaim: (token, locator) => this.markWorkerDeadLocked(token, locator),
      onAdmissionEvent: deps.onAdmissionEvent,
      rootForIssue: (issue) => this.rootForIssue(issue),
    });
    this.bootWatchdog = new WorkerBootWatchdog({
      workerBootTimeoutSeconds: () => this.deps.config.workerBootTimeoutSeconds,
      registrationDeadlineIntervals: () => this.deps.config.workerBootRegistrationDeadlineIntervals,
      now: () => this.deps.now(),
      run: this.deps.run,
      isOmpPane: (pid) => this.isOmpPane(pid),
      workerClient: (token, socketPath) => this.workerClient(token, socketPath),
      sleep: this.deps.sleep,
      yield: this.deps.yield,
      getClaim: (token) => {
        const claim = this.deps.state.roles[token];
        return claim && "issue" in claim
          ? { generation: claim.generation, sessionId: claim.sessionId }
          : undefined;
      },
      retireUnconfirmedBoot: (token, locator, generation, retry) =>
        this.retireUnconfirmedBoot(token, locator, generation, retry),
    });
  }

  /** The only way worker-queue promotion is ever allowed to actually launch or prompt
   * something. Call once, after the daemon's HTTP `api` is assigned, before the first explicit
   * `reconcileWorkerAdmission()` -- see `WorkerAdmission`'s own doc comment for why. */
  enableWorkerPromotion(): void {
    this.workerAdmission.enableWorkerPromotion();
  }

  /** Cancels the armed boot watchdog for `token`, if any — a no-op if none is armed, or if
   * `generation` is given and does not match the armed watchdog's (a stale caller from an
   * earlier attempt must never cancel a newer one's watch). */
  cancelBootWatchdog(token: string, generation?: number): void {
    this.bootWatchdog.cancel(token, generation);
  }

  /** Cancels every armed boot watchdog and any pending controller-registration wait. Daemon
   * shutdown calls this once before drain and again after — an in-flight handler during drain (a
   * launch's own success path, `reconnectWorkers`) can still arm a watchdog after the first call,
   * and this is the only guaranteed-safe way to catch that: no background timer may outlive the
   * ProcessManager. Idempotent. */
  dispose(): void {
    this.bootWatchdog.cancelAll();
    this.cancelControllerRegistrationDeadline();
  }

  private serialize<T>(
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

  admit(issue: IssueKey): "spawned" | "queued" {
    const tree = this.ensureTree(issue);
    const admission = this.deps.state.admission;
    admission.cap = this.deps.config.admissionCap;
    if (admission.active.includes(issue)) return "spawned";

    const queuedIndex = admission.queue.indexOf(issue);
    if (queuedIndex !== -1 || tree.status === "launch-failed") {
      if (queuedIndex !== -1) admission.queue.splice(queuedIndex, 1);
      if (tree.status === "launch-failed") tree.launchFailures = 0;
      tree.status = "queued";
      admission.queue.push(issue);
      void this.beginPromotionSweep();
      return "queued";
    }

    if (admission.active.length >= admission.cap) {
      tree.status = "queued";
      admission.queue.push(issue);
      void this.persist();
      return "queued";
    }

    admission.active.push(issue);
    tree.status = "active";
    void this.persist();
    void this.startRoot(issue);
    return "spawned";
  }

  /**
   * Converges stored admission with the configured cap, promoting queued trees
   * until active slots fill or no eligible queued tree remains. Runs at boot
   * because a cap raised between restarts opens slots no release event fills.
   */
  async reconcileAdmission(): Promise<void> {
    // Boot never trusts a window it did not record: reap every
    // `@legion_owner`-marked window no tree or the controller currently
    // names, with no grace period, before the demote/promote below decide
    // what to run. An orphan-active-no-locator tree demoted below (its
    // prior window, if one exists, was never recorded in its locator) is
    // caught by this same criterion, so it is reaped here rather than left
    // running alongside the fresh spawn the promotion loop later gives the
    // requeued tree.
    await this.reconcileTmuxWindows(0);

    const admission = this.deps.state.admission;
    admission.cap = this.deps.config.admissionCap;
    // An "active" tree with no recorded locator never finished spawning
    // before the daemon last stopped: advancePromotionSweep persists the
    // promotion before startRoot/spawnRoot ever records a locator, so a
    // crash in that exact window leaves this on disk. Demote it back to
    // queued so the promotion loop below re-spawns it, instead of leaving
    // it silently consuming a slot with nothing running forever.
    for (const issue of [...admission.active]) {
      const tree = this.deps.state.trees[issue];
      if (tree?.status !== "active" || tree.locator) continue;
      const activeIndex = admission.active.indexOf(issue);
      if (activeIndex !== -1) admission.active.splice(activeIndex, 1);
      tree.status = "queued";
      if (!admission.queue.includes(issue)) admission.queue.push(issue);
      console.error(
        `[legion] demoted ${issue} from active to queued at boot: no recorded locator (a prior spawn never completed before the daemon stopped)`
      );
    }

    let queued = admission.queue.length;
    while (admission.active.length < admission.cap && queued > 0) {
      await this.beginPromotionSweep();
      // No progress means every remaining queued issue is ineligible.
      if (admission.queue.length === queued) break;
      queued = admission.queue.length;
    }
    await this.persist();
  }

  async releaseSlot(issue: IssueKey): Promise<void> {
    const admission = this.deps.state.admission;
    const activeIndex = admission.active.indexOf(issue);
    if (activeIndex === -1) return;

    admission.active.splice(activeIndex, 1);
    await this.beginPromotionSweep();
  }

  async spawnWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    task: string
  ): Promise<SpawnWorkerResponse> {
    if (this.rootForIssue(issue) !== treeKey) {
      throw new Error(`Issue ${issue} does not belong to Legion tree ${treeKey}`);
    }
    // The same closed/absent/closing predicate `launchWorker` itself enforces, not merely
    // `closingTrees.has` -- a spawn against a tree that has already fully closed (durable
    // `status === "closed"`, `closingTrees` long since cleared) must 409 here, before ever
    // enqueueing at cap: an enqueued locator-less claim for an already-closed tree can only ever
    // hit `launchWorker`'s own `isTreeGone` check on promotion, and nothing else will ever prune
    // that queue entry for a tree with no `closeTreeLocked` call left to run its own pruning.
    if (this.isTreeGone(treeKey, issue)) {
      throw new TreeClosingError(treeKey);
    }
    if (role === "architect" && this.issueDepth(issue) >= this.deps.config.maxRecursionDepth) {
      throw new Error(
        `Refusing to spawn a sub-architect for ${issue}: recursion depth already at the configured maximum (${this.deps.config.maxRecursionDepth})`
      );
    }
    const token = roleToken(this.deps.state.project, issue, role);
    return this.workerAdmission.mutateClaim(token, () =>
      this.trackLaunch(treeKey, async () => {
        // Re-checked here, not only at entry above: a `closeTree` call can start and set this
        // tree closing while this exact callback was already queued behind a prior launch/stop
        // for this same role token -- the entry check above only excludes calls that started
        // after the tree was already closing, not ones queued just before. Re-checked again
        // below after every awaited step (the liveness probe, the old-locator retire): each is
        // a window a `closeTree` call can enter and see this token's OLD claim before this
        // decision has replaced it -- `inFlightLaunches` (see `trackLaunch`) keeps that close
        // from concluding the tree is empty until this whole decision settles, but only these
        // rechecks stop the decision itself from opening a fresh pane after the tree has
        // already started tearing down.
        if (this.closingTrees.has(treeKey)) {
          throw new TreeClosingError(treeKey);
        }
        const existing = this.deps.state.roles[token];
        const claim = existing && "issue" in existing ? existing : undefined;

        if (claim?.locator) {
          if (!claim.sessionId) {
            // Booting: launchWorker opened the pane but /worker/started has not yet registered
            // this generation's session. Never launch a second pane while a boot is in flight —
            // queue the task and let /worker/ready deliver it once the worker registers.
            claim.pendingAssignment = task;
            await this.deps.saveState();
            return { status: "resumed", roleToken: token };
          }
          const socketPath = claim.locator.socketPath;
          const probe = await probeWorkerSocket(
            (path) => this.workerClient(token, path),
            socketPath
          );
          if (this.closingTrees.has(treeKey)) {
            throw new TreeClosingError(treeKey);
          }
          if (probe.client) {
            // Connected — whether or not `get_state` itself answered. A client whose
            // `get_state` call never resolved simply keeps its unset/`"unknown"` `runState`,
            // which `resumeOrQueueExisting`'s own idle check already treats as
            // not-currently-promptable (queued exactly like a known-busy client) rather than a
            // reason to retire a socket that plainly still connects — matching the "connect
            // failure means dead, a connected socket whose `get_state` fails means only busy"
            // contract every other liveness check in this file follows. Only an *idle* client
            // is prompted directly, and only below `config.workerCap` (an idle client is not
            // currently counted by `runningWorkerCount()`, so prompting it unconditionally
            // would flip an uncounted-idle worker to running past the cap); a non-idle client
            // (mid-turn, or still unknown) is queued exactly like the at-cap case instead of
            // having a prompt injected into it, and delivered once its own idle transition
            // re-drains the queue. `WorkerAdmission.resumeOrQueueExisting` gates both cases
            // through the same `admissionLock` decision every other admission goes through, and
            // itself owns the actual `promptExistingWorker` call and releasing that reservation
            // once it settles (mirroring `launchOrQueue`'s admitted branch, which likewise owns
            // its own `launchWorker` call end to end).
            const decision = await this.workerAdmission.resumeOrQueueExisting(
              token,
              treeKey,
              issue,
              role,
              claim,
              claim.sessionId,
              task,
              probe.client
            );
            if (decision.kind === "queued") {
              return { status: "queued", roleToken: token };
            }
            return { status: "resumed", roleToken: token };
          }
          // May throw StopFailed (a real kill-pane failure, not the routine dead-socket case):
          // never launch a replacement over a pane that might still be alive.
          await this.retireWorkerLocator(token, claim.locator);
          if (this.closingTrees.has(treeKey)) {
            throw new TreeClosingError(treeKey);
          }
          return this.workerAdmission.launchOrQueue(token, treeKey, issue, role, claim, task);
        }

        return this.workerAdmission.launchOrQueue(token, treeKey, issue, role, claim, task);
      })
    );
  }

  /** Registers `fn`'s own promise in `inFlightLaunches` for `treeKey` for its whole duration,
   * removed on settle either way -- see that field's comment for why `closeTreeLocked`'s
   * fixed-point loop needs to await this set before every re-snapshot. Wraps the ENTIRE
   * post-fence spawn decision, not merely the pane-opening step inside it: a stale-claim
   * respawn's liveness probe and old-locator retire are themselves awaited steps a `closeTree`
   * call can enter behind, so the whole decision -- not just `launchWorker` -- must be visible
   * to a concurrent close before it can safely conclude the tree is empty. */
  private trackLaunch<T>(treeKey: IssueKey, fn: () => Promise<T>): Promise<T> {
    const inFlight = this.inFlightLaunches.get(treeKey) ?? new Set<Promise<unknown>>();
    this.inFlightLaunches.set(treeKey, inFlight);
    const running = fn();
    inFlight.add(running);
    const untrack = (): void => {
      inFlight.delete(running);
      if (inFlight.size === 0) this.inFlightLaunches.delete(treeKey);
    };
    running.then(untrack, untrack);
    return running;
  }

  /** Prompts an already-connected, already-live worker client with `task` — the shared
   * implementation behind every "resume an existing worker" path (`WorkerAdmission`'s
   * `resumeOrQueueExisting` admitted branch, its `"prompt"` queue-promotion decision, and
   * `/worker/ready`), as opposed to `launchWorker`, which opens a fresh pane. Does not touch the
   * running-worker admission count itself — the caller owns reserving and releasing that slot
   * (mirroring `launchWorker`, which is likewise unaware of admission bookkeeping) since only
   * the caller knows whether this prompt represents a new admission (`resumeOrQueueExisting`'s
   * below-cap idle-resume, or a queue promotion) or none at all (`/worker/ready` resuming a
   * worker whose slot was already counted via its locator from the moment `launchWorker` wrote
   * it, so nothing here needs releasing or re-checking). Registers the current phase
   * (`state.phases[issue]`) so `phase/complete` can find it, and clears the claim's
   * `pendingAssignment` if it was still set. Throws (without touching
   * phases/`pendingAssignment`/persisting) only if `prompt()` itself rejects — the caller
   * decides what "the prompt failed" means for its own bookkeeping. A `persist` failure
   * *after* `prompt()` already succeeded is a durable-state persistence issue, not a prompt
   * failure: the worker is already working, mirroring `launchWorker`'s post-locator-write
   * save handling. Retries the persist once; if that also fails, logs it and returns
   * normally — never rethrown, since the caller (and, transitively, the architect) would
   * otherwise see a failure for a worker that is actually already running the task. */
  private async promptExistingWorker(
    client: WorkerRpcClient,
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    task: string
  ): Promise<void> {
    await client.prompt(task);
    this.deps.state.phases[issue] = { phase: role, sessionId };
    const claim = this.deps.state.roles[token];
    if (claim && "issue" in claim) {
      delete claim.pendingAssignment;
      // A successful prompt confirms this worker is responsive again — any accumulated
      // rejection count from a prior transient failure must never carry into a future one.
      claim.promptFailures = 0;
    }
    try {
      await this.persist();
    } catch (persistError) {
      console.error(
        `[legion] failed to persist ${token}'s phase/pendingAssignment after a successful prompt (worker is already working regardless):`,
        persistError
      );
      try {
        await this.persist();
      } catch (retryError) {
        console.error(
          `[legion] retry-persist for ${token} also failed; in-memory claim remains authoritative:`,
          retryError
        );
      }
    }
  }

  /** Re-evaluates the running-worker queue against the current `config.workerCap` — called at
   * boot (after `reconnectWorkers` has rebuilt live shim connections) and by the periodic linger
   * sweep, so a cap raised between restarts, or a below-threshold launch failure that rotated
   * its head to the tail with nothing else to trigger a retry, both eventually get another
   * promotion attempt. See `WorkerAdmission.reconcileWorkerAdmission` for the gate this respects.
   */
  async reconcileWorkerAdmission(): Promise<void> {
    await this.workerAdmission.reconcileWorkerAdmission();
  }

  private publishArchitect(
    treeKey: IssueKey,
    payload:
      | { type: "worker-queued"; issue: IssueKey; role: LegionRole }
      | { type: "worker-started"; issue: IssueKey; role: LegionRole }
  ): void {
    this.deps.natsPublish(
      roleTopic(roleToken(this.deps.state.project, treeKey, "architect")),
      JSON.stringify(payload)
    );
  }

  async workerReady(
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    generation: number
  ): Promise<void> {
    const root = this.rootForIssue(issue);
    if (root && this.closingTrees.has(root)) {
      throw new TreeClosingError(root);
    }
    const token = roleToken(this.deps.state.project, issue, role);
    // Serialized through the same per-role queue spawnWorker/stopProcessSerialized use, so a
    // queued task can never be delivered to a worker a concurrent closeTree is mid-stopping (or
    // vice versa) -- whichever entered the queue first completes before the other starts.
    await this.workerAdmission.mutateClaim(token, async () => {
      if (root && this.closingTrees.has(root)) {
        throw new TreeClosingError(root);
      }
      const claim = this.deps.state.roles[token];
      if (
        !claim ||
        !("issue" in claim) ||
        claim.sessionId !== sessionId ||
        claim.generation !== generation
      ) {
        return;
      }
      const task = claim.pendingAssignment;
      if (!task || !claim.locator) return;
      const client = await this.workerClient(token, claim.locator.socketPath);
      await this.promptExistingWorker(client, token, issue, role, sessionId, task);
    });
  }

  /** The inner logic behind a worker's socket being confirmed dead -- a boot-time reconnect
   * probe failed, or a live connection's own `client.closed` handler tried and failed its one
   * reconnect attempt, or the prompt-failure circuit breaker retired a persistently-broken
   * worker (see `WorkerAdmissionDeps.retireDeadClaim`) -- so its locator is retired and cleared:
   * retires whatever pane it may still be running, then clears the locator -- moving its
   * `ompSessionFile` to `resumeSessionFile` so the eventual respawn/promotion still resumes the
   * same agent -- leaving `pendingAssignment` so it still delivers the queued task. Assumes the
   * caller already holds this token's `roleLaunchQueue` critical section -- the same one
   * `launchWorker`/`promoteQueuedWorker` use -- and re-checks that the claim's current locator
   * still matches the one it was handed before touching anything: by the time this call gets
   * its turn, a newer launch for the same token may already have replaced it (or the claim may
   * be gone entirely, e.g. `closeTree`), and this must never delete a newer launch's locator or
   * retire a pane that isn't the one it was told to. */
  private async markWorkerDeadLocked(token: string, locator: WorkerLocator): Promise<void> {
    const current = this.deps.state.roles[token];
    if (!current || !("issue" in current) || current.locator?.tmuxPaneId !== locator.tmuxPaneId) {
      return;
    }
    await this.retireWorkerLocator(token, locator);
    const resumeSessionFile = current.locator?.ompSessionFile ?? current.resumeSessionFile;
    delete current.locator;
    if (resumeSessionFile) current.resumeSessionFile = resumeSessionFile;
    await this.persist();
  }

  /** Acquires this token's `roleLaunchQueue` critical section (see `markWorkerDeadLocked` for
   * the actual logic) then re-checks the running-worker queue, since clearing the locator may
   * have freed the slot this worker was occupying. */
  private async markWorkerDead(token: string, locator: WorkerLocator): Promise<void> {
    await this.workerAdmission.mutateClaim(token, () => this.markWorkerDeadLocked(token, locator));
    this.workerAdmission.promoteWorkerQueue();
  }

  /** Reconnects to every live worker's shim socket after a daemon restart, probing liveness. A
   * connect failure against an already-confirmed claim means the worker is confirmed dead; the
   * same failure against an unconfirmed boot (never reached `/worker/started`) is routed through
   * `retireUnconfirmedBoot` instead, so it retries or gives up exactly like the boot watchdog
   * would rather than merely clearing the locator and stranding the claim with nothing left to
   * ever retry it. A connect that succeeds but whose follow-up `get_state` fails (times out,
   * say) means only that the shim is busy answering this one request in time — never a reason
   * to kill a live worker — so the claim is left exactly as is, with its conservative
   * "unknown"-counts-as-running `runState`. An unconfirmed boot that survives this reconnect
   * gets its watchdog re-armed: the in-memory `WorkerBootWatchdog` registry does not survive a
   * restart, so without this a boot still awaiting `/worker/started` would never be probed
   * again. */
  async reconnectWorkers(): Promise<void> {
    const claims = Object.entries(this.deps.state.roles).filter(
      (entry): entry is [string, WorkerRoleClaim & { locator: WorkerLocator }] =>
        "issue" in entry[1] && entry[1].locator !== undefined
    );
    await Promise.all(
      claims.map(async ([token, claim]) => {
        // Captured once, immutably, before any await: a concurrent respawn replacing this
        // claim's locator mid-probe (e.g. a dead-socket `spawnWorker` decision finishing while
        // this exact connect is still in flight) must never be mistaken for the locator this
        // call is actually probing — `retireUnconfirmedBoot`'s/`markWorkerDeadLocked`'s own
        // pane-id identity check only protects against retiring the WRONG locator if this one
        // is passed correctly.
        const probedLocator = claim.locator;
        const parsed = parseRoleToken(this.deps.state.project, token);
        let treeKey: IssueKey | undefined;
        let role: LegionRole | undefined;
        if (parsed && !("controller" in parsed)) {
          role = parsed.role;
          treeKey = this.rootForIssue(parsed.issue);
        }
        const probe = await probeWorkerSocket(
          (socketPath) => this.workerClient(token, socketPath),
          probedLocator.socketPath
        );
        if (!probe.client) {
          console.error(`[legion] failed to reconnect worker ${token}:`, probe.connectError);
          if (claim.sessionId) {
            await this.markWorkerDead(token, probedLocator);
          } else {
            await this.retireUnconfirmedBoot(
              token,
              probedLocator,
              claim.generation,
              this.deriveRetryContext(token, claim.issue)
            );
          }
          return;
        }
        // `probeWorkerSocket`'s `get_state` call seeds `runState` from the response's
        // `isStreaming` field on this same client: a worker that was already idle before this
        // restart is not left stuck at the conservative "unknown" default (counted as running)
        // forever, since nothing else will ever observe an `agent_start`/`agent_end` frame to
        // correct it if it never runs another turn. A `get_state` failure only means the shim
        // is busy answering this one request — never a reason to treat a connected worker as
        // dead.
        if (!probe.stateAnswered) {
          console.error(
            `[legion] worker ${token} reconnected but get_state failed (treating as busy, not dead):`,
            probe.stateError
          );
        }
        if (!claim.sessionId && treeKey && role) {
          this.bootWatchdog.arm(
            treeKey,
            claim.issue,
            role,
            token,
            claim.locator,
            claim.generation ?? 1
          );
        }
      })
    );
  }

  /**
   * Shared by `markProcessDead` and `reportRootExit`: the root architect's own self-report of
   * its exit, always still-alive-and-blocked on this very HTTP response inside its
   * `session_shutdown` hook (`packages/pi-envoy/extensions/legion.ts`) — never a case with
   * anything live left to gracefully stop or probe (asking that same process's own shim to
   * close its stdin would deadlock: the shim cannot close until OMP exits `session_shutdown`,
   * which cannot happen until this responds). Releases the admission slot and persists
   * unconditionally. Clears the tree's locator too, UNLESS a `closeTree` call is already in
   * flight for this tree: that close's root leg captured its own copy of the locator before
   * this could race it (see `closeTreeLocked`), and owns clearing/using it from here — clearing
   * it again here as well would just be redundant, but leaving it be documents that ownership
   * unambiguously and avoids ever touching a field a concurrent close still reads. `status` is
   * set only when given (`markProcessDead` passes `"dead"`; `reportRootExit` leaves the terminal
   * status to whichever `closeTree` is or becomes responsible for the tree).
   */
  private async recordRootExit(treeKey: IssueKey, tree: TreeState, status?: "dead"): Promise<void> {
    if (!this.closingTrees.has(treeKey)) {
      delete tree.locator;
    }
    if (status) tree.status = status;
    await this.releaseSlot(treeKey);
    await this.deps.saveState();
  }

  /**
   * Handles a boot confirmed dead — never reached `/worker/started`, and its pane/socket are
   * both gone: retires whatever is left of the pane, clears the locator (stashing its
   * `ompSessionFile` into `resumeSessionFile`, mirroring `markWorkerDeadLocked`, so a later
   * `spawnWorker` call — or the retry below — resumes the same agent instead of finding a stale
   * locator and concluding a boot is still in flight forever). The clear always happens, even
   * when `treeKey` cannot be resolved (an issue whose tree no longer exists) — a permanently
   * dangling stale locator is never acceptable — but the retry-or-give-up accounting
   * (`launchFailures`, enqueue, or `worker-died` at the threshold) only runs when there is a
   * tree left to retry against or notify. Below the threshold, the retry is *enqueued*
   * (`WorkerAdmission.enqueueForRetryPending`, folded into this same locked save so the
   * locator-clear and the queue-push are one durable transition), never launched directly here:
   * a direct `launchWorker`
   * call would bypass the running-worker cap, the reservation, and the per-role launch lock —
   * over-admission, or a second pane racing a concurrent same-role spawn — and would also
   * dereference `api` before it exists when this runs from `reconnectWorkers` at boot (before
   * `enableWorkerPromotion()`). The whole decision runs under `mutateClaim(token)`, serialized
   * against every other admission/retirement decision for this token (`spawnWorker`,
   * `handleWorkerStarted` via `mutateLiveRoleClaim`) — and re-validates the claim it was handed
   * against the current one before touching anything, since the caller may have captured it,
   * or decided this boot was dead, some time before this actually runs: a claim that has since
   * been confirmed (`sessionId` now set), superseded by a newer launch (a different generation
   * or pane), or deleted entirely (`closeTree`) means this retirement is stale and must never
   * touch what replaced it. Also never fights a `closeTree` already tearing this claim's tree
   * down: `closeTreeLocked`'s own fixed-point loop already owns stopping (and deleting) every
   * worker under a closing/closed tree through its own `stopProcessSerialized` call — retiring
   * this same token again here would either find nothing left to stop or, worse, stop a
   * respawned generation `closeTree` never asked for, so a tree that `isTreeGone` reports gone
   * makes this a no-op instead. Shared by the boot watchdog's own dead verdict,
   * `reconnectWorkers`' restart-time probe, and a pre-confirmation socket close
   * (`onWorkerClientClosed`).
   */
  private async retireUnconfirmedBoot(
    token: string,
    locator: WorkerLocator,
    generation: number | undefined,
    retry?: { treeKey: IssueKey; issue: IssueKey; role: LegionRole }
  ): Promise<void> {
    await this.workerAdmission.mutateClaim(token, async () => {
      const claim = this.deps.state.roles[token];
      if (
        !claim ||
        !("issue" in claim) ||
        claim.sessionId !== undefined ||
        claim.generation !== generation ||
        claim.locator?.tmuxPaneId !== locator.tmuxPaneId
      ) {
        return;
      }
      const treeKey = retry?.treeKey ?? this.rootForIssue(claim.issue);
      // A closeTree already tearing down (or having already torn down) this claim's tree owns
      // stopping (and deleting) this exact worker through its own fixed-point loop — retiring
      // it again here would either find nothing left to stop or, worse, stop a respawned
      // generation closeTree never asked for. Skipped for that reason only: an issue whose
      // tree cannot be resolved at all still gets its dangling locator cleared below (never
      // left permanently stale) — it just never reaches the retry-or-give-up accounting past
      // it, exactly like the `!retry` early return already handles.
      if (
        treeKey &&
        (this.deps.state.trees[treeKey]?.status === "closed" || this.closingTrees.has(treeKey))
      ) {
        return;
      }
      await this.retireWorkerLocator(token, locator);
      const resumeSessionFile = claim.locator?.ompSessionFile ?? claim.resumeSessionFile;
      delete claim.locator;
      if (resumeSessionFile) claim.resumeSessionFile = resumeSessionFile;
      if (!retry) {
        await this.deps.saveState();
        return;
      }
      const { treeKey: retryTreeKey, issue, role } = retry;
      const failures = (claim.launchFailures ?? 0) + 1;
      claim.launchFailures = failures;
      // `===`, not `>=`: fires exactly once, at the tick `failures` first reaches the threshold —
      // see `launchWorker`'s own publish for why.
      if (failures === MAX_LAUNCH_FAILURES) {
        this.publishWorkerDied(retryTreeKey, issue, role);
        await this.deps.saveState();
        return;
      }
      // Pushed onto the queue *before* this save (not after, and not via `enqueueForRetry`'s
      // own separate persist) so the locator-clear above and this queue-push land in the same
      // durable transition: two separate saves would let a crash, or a persist failure,
      // between them strand this claim — locator-less, unqueued, and invisible to
      // `reconnectWorkers`' locator-only filter, with nothing left to ever retry it.
      await this.workerAdmission.enqueueForRetryPending(token);
      await this.deps.saveState();
      this.workerAdmission.promoteWorkerQueue();
    });
  }

  async markProcessDead(treeKey: IssueKey, generation?: number): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.generation !== (generation ?? tree.generation)) return;
    await this.recordRootExit(treeKey, tree, "dead");
  }

  /**
   * The root architect's own `/process/exit` self-report on a CLOSED issue. Unlike
   * `markProcessDead`, the caller here is not being resurrected — the tree is being torn down,
   * so this never awaits (or otherwise joins) `closeTree` itself: `closeTree`'s root leg would
   * gracefully ask this exact process's own shim to close its stdin, but the caller of THIS
   * method is that same process, still blocked on this very HTTP response inside its
   * `session_shutdown` hook — awaiting `closeTree` here would deadlock identically to
   * `markProcessDead`'s case, and OMP's `session_shutdown` handler itself is capped at ~2s
   * (`oh-my-pi/packages/coding-agent/src/session/runner.ts:105-124`), so blocking this response
   * on up to a 60s tree close was never sound regardless. Records the exit (see
   * `recordRootExit`) and returns immediately, leaving the terminal tree status to whichever
   * `closeTree` owns it: if one is already in flight (the common case — this self-report is
   * usually the linger sweep's `closeTree` unblocking because this very response is about to
   * let the root finish exiting), nothing more is done here; otherwise a background
   * `closeTree(treeKey, { stopRoot: false })` is started (never awaited) to stop the tree's
   * workers.
   */
  async reportRootExit(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    const closing = this.closingTrees.has(treeKey);
    await this.recordRootExit(treeKey, tree);
    if (!closing) {
      void this.closeTree(treeKey, { stopRoot: false }).catch((error) => {
        console.error(
          `[legion] background closeTree for ${treeKey} failed after a root self-report:`,
          error
        );
      });
    }
  }

  /**
   * Gracefully stops every process this tree's root and worker claims recorded — root and every
   * worker concurrently, each asked to shut down over its own shim socket and given up to the
   * configured tree stop timeout before `stopProcess` kills its own pane directly. Idempotent: a
   * second call for a tree already closing awaits the same in-flight close instead of starting a
   * new one (`closingTrees`, in-memory only — see its field comment for why this is never
   * persisted). `stopRoot: false` names the one caller for whom the root's OWN shutdown must be
   * skipped entirely — `reportRootExit`, the root architect reporting its own exit on a closed
   * issue (see that method's doc comment for why gracefully stopping that same process would
   * deadlock); its own locator is still cleared, since the caller already confirmed it is gone.
   * Every other (unilateral) caller — `expireLinger` — stops the root too, but only if `probe`
   * confirms it is still alive; a dead root has nothing to gracefully close, so `stopProcess`
   * skips straight to reaping whatever pane is left rather than burning the full stop timeout
   * finding that out again. Its first durable act (before any stop) is marking the tree
   * `lingering` with a fresh `lingerUntil` if it is not already — so a crash anywhere during the
   * close leaves a retryable `lingering` tree the periodic sweep re-closes, never a durably
   * `active` record with an already-half-stopped process underneath it. Runs to a fixed point:
   * `inFlightLaunches` (see its field comment) is awaited before every re-snapshot of
   * `state.roles`, so a `spawnWorker` call that crossed the closing check just before
   * `closingTrees` was populated always gets to finish (and be seen) before a snapshot can
   * report the tree empty — every claim present at any point during this call gets exactly one
   * stop attempt. A claim whose stop fails (`StopFailed` — a real `kill-pane` failure, not the
   * routine "process already gone" case) is never deleted and its locator never cleared: the
   * tree is left `lingering` with a fresh `lingerUntil` so the periodic sweep retries the close,
   * and this call throws `StopFailed` rather than reporting a false success.
   */
  async closeTree(treeKey: IssueKey, options?: { stopRoot?: boolean }): Promise<void> {
    const inFlight = this.closingTrees.get(treeKey);
    if (inFlight) return inFlight;
    const closing = this.closeTreeLocked(treeKey, options).finally(() => {
      this.closingTrees.delete(treeKey);
    });
    this.closingTrees.set(treeKey, closing);
    return closing;
  }

  private async closeTreeLocked(
    treeKey: IssueKey,
    options?: { stopRoot?: boolean }
  ): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.status !== "lingering") {
      tree.status = "lingering";
      tree.lingerUntil = new Date(this.deps.now()).toISOString();
      await this.deps.saveState();
    }
    const stopRoot = options?.stopRoot ?? true;
    let anyFailed = false;

    if (tree.locator) {
      if (!stopRoot) {
        // Nothing to gracefully stop — see this method's doc comment — but the caller already
        // confirmed its own exit, so the locator it names is always gone.
        delete tree.locator;
      } else {
        // Captured once, immutably, before any await: a concurrent `reportRootExit` racing this
        // same tree must never be able to yank the locator out from under this leg between the
        // probe and the stop call that follows it. (`recordRootExit` guards the reverse
        // direction too — it skips clearing the locator whenever `closingTrees` already names
        // this tree, precisely so this captured copy stays valid for as long as this leg needs
        // it.)
        const architectToken = roleToken(this.deps.state.project, treeKey, "architect");
        const rootLocator = tree.locator;
        const architectClaim = this.deps.state.roles[architectToken];
        this.revokeRoleClaim(
          architectClaim && "issue" in architectClaim ? architectClaim : undefined
        );
        const alive = (await this.probe(treeKey)) === "alive";
        try {
          await this.stopProcessSerialized(architectToken, rootLocator, this.treeStopTimeoutMs, {
            skipGraceful: !alive,
          });
          delete tree.locator;
        } catch (error) {
          anyFailed = true;
          console.error(`[legion] root process failed to stop while closing ${treeKey}:`, error);
        }
      }
    }

    const attemptedTokens = new Set<string>();
    for (;;) {
      await Promise.allSettled([...(this.inFlightLaunches.get(treeKey) ?? [])]);
      const batch = Object.entries(this.deps.state.roles).filter(
        (entry): entry is [string, WorkerRoleClaim & { locator: WorkerLocator }] =>
          "issue" in entry[1] &&
          this.rootForIssue(entry[1].issue) === treeKey &&
          entry[1].locator !== undefined &&
          !attemptedTokens.has(entry[0])
      );
      if (batch.length === 0) break;
      for (const [token] of batch) attemptedTokens.add(token);
      // Captured synchronously here, alongside `batch` itself and before any `await` below —
      // `batch`'s claim objects are the live `state.roles` entries, not copies, so a concurrent
      // mutation of that exact same object (e.g. a respawn deleting/replacing its `locator` in
      // place) between this point and the delete step below would otherwise change what
      // `claim.locator` reads as by the time it is read again.
      const stoppedPaneIds = new Map(
        batch.map(([token, claim]) => [token, claim.locator.tmuxPaneId])
      );
      const settled = await Promise.allSettled(
        batch.map(([token, claim]) =>
          this.stopProcessSerialized(token, claim.locator, this.treeStopTimeoutMs)
        )
      );
      for (const [index, [token]] of batch.entries()) {
        const result = settled[index];
        if (result?.status === "fulfilled") {
          // Re-acquires the token's own critical section for the delete itself, re-checking the
          // claim's identity (pane id) against the locator that was actually stopped — mirrors
          // `retireUnconfirmedBoot`'s stale-identity guard. `stopProcessSerialized` above already
          // ran under this same lock, but only for the stop call; without re-acquiring it here,
          // a live writer (e.g. `/worker/started`) that raced in in-between (took the lock after
          // the stop released it but before this delete runs) and wrote a fresh locator for a
          // respawned generation would have that fresh claim silently deleted by a stop that
          // targeted the OLD, now-irrelevant pane.
          await this.workerAdmission.mutateClaim(token, async () => {
            const current = this.deps.state.roles[token];
            if (
              current &&
              "issue" in current &&
              current.locator?.tmuxPaneId === stoppedPaneIds.get(token)
            ) {
              // Cancels this claim's boot watchdog (a no-op if none is armed) and revokes its
              // session capability before deleting it — the single chokepoint every path that
              // retires or deletes a role's claim must go through, or a stale process could
              // keep minting grants after this tree has stopped trusting it.
              this.cancelBootWatchdog(token);
              this.revokeRoleClaim(current);
              delete this.deps.state.roles[token];
            }
          });
        } else {
          anyFailed = true;
          console.error(
            `[legion] worker ${token} failed to stop while closing ${treeKey}:`,
            result?.status === "rejected" ? result.reason : undefined
          );
          // Leave this claim and its locator exactly as they are — a possibly-still-live pane
          // must never lose its only durable handle.
        }
      }
    }
    if (anyFailed) {
      tree.status = "lingering";
      tree.lingerUntil = new Date(this.deps.now()).toISOString();
      await this.deps.saveState();
      throw new StopFailed(
        treeKey,
        `Legion tree ${treeKey} has a process that could not be stopped`
      );
    }

    tree.status = "closed";
    delete tree.lingerUntil;
    const rootStatus = this.deps.state.issues[treeKey]?.status;
    if (rootStatus !== "done" && rootStatus !== "backlog" && rootStatus !== "icebox") {
      await writeStatus(this.deps.state, this.deps.dispatchClient, treeKey, "done");
    }
    await this.releaseSlot(treeKey);
    // Every stopped claim above (one with a locator) is already deleted; this also clears any
    // remaining claim under the tree that never had a locator to stop in the first place (a
    // claim still queued, never launched, when the tree closed), which the fixed-point loop
    // above never even sees. Each delete runs inside that same token's own critical section
    // (mirroring the fixed-point loop's delete-after-stop): a writer that acquired this exact
    // token's lock just before `tree.status` flipped to "closed" above, and has not yet reached
    // its own post-lock `rejectIfTreeGone` check, is let to finish (and reject itself against
    // the now-closed tree) before this delete runs, rather than racing it.
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && this.rootForIssue(claim.issue) === treeKey) {
        await this.workerAdmission.mutateClaim(token, async () => {
          this.cancelBootWatchdog(token);
          this.revokeRoleClaim(claim);
          delete this.deps.state.roles[token];
        });
      }
    }
    this.clearTreePhases(treeKey);
    // A tree that was closed while it still had queued (never-launched) tokens must never leave
    // them behind for a later, unrelated drain to promote against a tree that no longer exists.
    await this.workerAdmission.pruneQueueForTree(treeKey);
    // A spawn capability minted before shutdown has no live claim left to authorize once this
    // tree's claims are deleted above, but it would otherwise still sit in state indefinitely,
    // revealing a valid (tree, issue, role) triple to whoever holds the token -- deleting every
    // capability recorded against this tree closes that window for good.
    for (const [key, capability] of Object.entries(this.deps.state.spawnCapabilities)) {
      if (capability.tree === treeKey) {
        delete this.deps.state.spawnCapabilities[key];
      }
    }
    await this.deps.saveState();
    // Deleting this tree's claims may have freed running-worker slots other trees' queues are
    // waiting on.
    this.workerAdmission.promoteWorkerQueue();
  }

  /**
   * Kills every `@legion_owner`-marked tmux window this state does not
   * name (via a tree's or the controller's locator) and that has been idle
   * at least `graceMs` — long enough that a window mid-creation (activity
   * not yet recorded) is never mistaken for an orphan. Boot calls this with
   * `graceMs: 0`: it never trusts a window it did not itself record, so
   * there is no such race to protect against there (see `reconcileAdmission`).
   * Also reaps unrecorded worker-shim *panes* split into a window this state
   * DOES still recognize — a window-level orphan check alone can never see
   * these: `launchWorker` persists a locator only after opening the pane
   * (see its own doc comment), so a crash in that exact gap leaves a real,
   * running process inside a window nothing else ever flags as unowned.
   *
   * `tmuxPaneId` is optional on every locator (state predating the field, or any other
   * pane-id-less write) — a window whose owning tree/role/controller locator has a known
   * `tmuxWindowId` but no recorded `tmuxPaneId` is exempted from the pane-level pass entirely:
   * every pane in that window (including the owner's own, since we cannot tell which one it is
   * without the id) would otherwise look exactly like the unrecorded-crash-window-orphan this
   * pass exists to catch, and killing it would kill a live root/controller/worker the daemon
   * itself is still actively running. `probe`/`controllerAlive` backfill `tmuxPaneId` the next
   * time they confirm that locator alive, so this exemption — and the ambiguity it accepts —
   * shrinks to nothing as every surviving locator gets its pane id recorded.
   */
  async reconcileTmuxWindows(graceMs = TMUX_RECONCILIATION_GRACE_MS): Promise<void> {
    const session = `legion-${this.deps.state.project}`;
    const owner = session;
    const locators = [
      ...Object.values(this.deps.state.trees).map((tree) => tree.locator),
      ...Object.values(this.deps.state.roles).map((claim) =>
        "issue" in claim ? claim.locator : undefined
      ),
      this.deps.state.controllerLocator,
    ];
    const knownWindows = new Set(
      locators
        .map((locator) => locator?.tmuxWindowId)
        .filter((windowId): windowId is string => windowId !== undefined)
    );
    const unknownWindows = await tmux.listUnknownOwnedWindows(
      this.deps.run,
      session,
      owner,
      knownWindows
    );
    for (const { windowId, activityAt } of unknownWindows) {
      if (this.deps.now() - activityAt < graceMs) continue;
      await tmux.killWindow(this.deps.run, windowId);
    }

    const knownPanes = new Set(
      locators
        .map((locator) => locator?.tmuxPaneId)
        .filter((paneId): paneId is string => paneId !== undefined)
    );
    const exemptWindows = new Set(
      locators
        .filter(
          (locator) => locator?.tmuxWindowId !== undefined && locator.tmuxPaneId === undefined
        )
        .map((locator) => locator?.tmuxWindowId)
    );
    const unknownPanes = await tmux.listUnknownPanes(this.deps.run, owner, knownPanes);
    for (const { paneId, windowId, activityAt } of unknownPanes) {
      if (exemptWindows.has(windowId)) continue;
      if (this.deps.now() - activityAt < graceMs) continue;
      await tmux.killPane(this.deps.run, paneId);
    }
  }

  async spawnRoot(issue: IssueKey, resume = false, resumeSessionFile?: string): Promise<void> {
    const tree = this.ensureTree(issue);
    const priorGeneration = tree.generation;
    const priorLocator = tree.locator;
    tree.generation += 1;
    try {
      await this.spawnTree(tree, resume, resumeSessionFile);
    } catch (error) {
      tree.generation = priorGeneration;
      if (priorLocator) tree.locator = priorLocator;
      else delete tree.locator;
      tree.launchFailures += 1;
      const activeIndex = this.deps.state.admission.active.indexOf(issue);
      if (activeIndex !== -1) this.deps.state.admission.active.splice(activeIndex, 1);
      if (tree.launchFailures >= MAX_LAUNCH_FAILURES) {
        tree.status = "launch-failed";
        const queueIndex = this.deps.state.admission.queue.indexOf(issue);
        if (queueIndex !== -1) this.deps.state.admission.queue.splice(queueIndex, 1);
        this.publishController({
          type: "launch-failed",
          issue,
          failures: tree.launchFailures,
        });
      } else {
        tree.status = "queued";
        if (!this.deps.state.admission.queue.includes(issue)) {
          this.deps.state.admission.queue.push(issue);
        }
      }
      this.settlePromotionSpawn(issue);
      if (this.promotionSweep) await this.advancePromotionSweep();
      else await this.beginPromotionSweep(issue);
      await this.deps.saveState();
      throw error;
    }

    // The spawn itself succeeded — a real tmux window is running. A save
    // failure past this point is not a launch failure: rolling back
    // generation/status/launchFailures here would make the daemon retry a
    // tree that already has a live window. Kill that window first instead
    // — every spawn is either persisted or reaped, never left running
    // unrecorded — then propagate the failure distinctly (see
    // `SpawnPersistenceFailure`) so this goes fatal like every other
    // durable effect whose post-mutation save fails.
    tree.launchFailures = 0;
    this.settlePromotionSpawn(issue);
    if (this.promotionSweep?.inFlight === 0) this.promotionSweep = undefined;
    try {
      await this.deps.saveState();
    } catch (error) {
      if (tree.locator) {
        await this.deps.run(["tmux", "kill-window", "-t", tree.locator.tmuxWindowId]);
      }
      throw new SpawnPersistenceFailure(error);
    }
  }

  /**
   * A live pane is not proof of a role holder: `controllerAlive()` only confirms the tmux pane
   * itself is running, not that it ever reached `/controller/ready` (a stuck plugin) or that its
   * Envoy role claim survived while the pane did (lost independently of the pane dying). Either
   * way, an alive-but-unclaimed controller would otherwise strand every pending notice forever,
   * since nothing else ever retries a pane this method already considers "there". Arms a bounded
   * wait bound to the exact locator observed -- an already-alive-but-unclaimed one here, or a
   * freshly-spawned one below, since a brand new controller can just as easily never register --
   * `workerBootTimeoutSeconds * workerBootRegistrationDeadlineIntervals` (the same budget a
   * worker's own boot gets), rather than resetting the clock on every call. If the role is still
   * unclaimed once that wait elapses, retires the stuck pane and spawns a fresh one in its place.
   */
  async ensureController(): Promise<void> {
    if (await this.controllerAlive()) {
      const locator = this.deps.state.controllerLocator;
      if (this.deps.state.roles[controllerToken(this.deps.state.project)]) {
        this.cancelControllerRegistrationDeadline();
      } else if (locator) {
        this.armControllerRegistrationDeadline(locator);
      }
      return;
    }
    this.cancelControllerRegistrationDeadline();
    if (!this.controllerSpawn) {
      this.controllerSpawn = (async () => {
        const controllerSecret = await this.deps.mintControllerCapability();
        await this.spawnController(controllerSecret);
        const freshLocator = this.deps.state.controllerLocator;
        if (freshLocator) this.armControllerRegistrationDeadline(freshLocator);
      })().finally(() => {
        this.controllerSpawn = undefined;
      });
    }
    await this.controllerSpawn;
  }

  /** Arms a bounded wait for `locator` to be claimed, unless one is already armed (for this or
   * any other locator) -- at most one wait is ever in flight, and only the wait that observed
   * this exact locator may ever act on it (see `retireAndRespawnStuckController`'s doc
   * comment). */
  private armControllerRegistrationDeadline(
    locator: NonNullable<LegionState["controllerLocator"]>
  ): void {
    if (this.controllerRegistrationWait) return;
    const deadlineMs =
      this.deps.config.workerBootTimeoutSeconds *
      1_000 *
      this.deps.config.workerBootRegistrationDeadlineIntervals;
    const { timedOut, cancel } = this.stopTimeout(deadlineMs);
    this.controllerRegistrationWait = { locator, cancel };
    void timedOut.then(() =>
      this.retireAndRespawnStuckController(locator).catch((error) => {
        console.error("[legion] failed to retire and respawn a stuck controller:", error);
      })
    );
  }

  /** Cancels the armed controller-registration wait, if any -- releases its underlying real
   * timer (a no-op for a test's injected `sleep`; see the field's own doc comment for why that
   * is still safe) and clears the field so a late fire from it is recognized as stale by
   * `retireAndRespawnStuckController`'s own identity check. */
  private cancelControllerRegistrationDeadline(): void {
    this.controllerRegistrationWait?.cancel();
    this.controllerRegistrationWait = undefined;
  }

  /**
   * Runs once the registration deadline armed above elapses for `locator`. A no-op unless this
   * exact wait is still the one currently armed (by reference): an intervening `ensureController`
   * call may have already cancelled it (role claimed) or superseded it (this pane died on its
   * own and a fresh one was spawned, arming its own wait) before this stale timer got a chance
   * to fire. Re-checks the role once before the liveness probe and again immediately after it --
   * a `/controller/ready` landing during that probe's own await must still win over this
   * stale-timeout decision, never be raced by it. A `state.controllerLocator` that no longer
   * matches `locator` by reference is the same "superseded" case caught above, checked again
   * directly against live state for good measure. On a failed stop/kill, logs and leaves the
   * locator exactly as it was -- clearing it and spawning a second controller onto a pane that
   * never actually stopped would orphan that pane with nothing tracking it; a later
   * `ensureController` call re-observes this same stuck locator and re-arms a fresh wait for it
   * instead.
   */
  private async retireAndRespawnStuckController(
    locator: NonNullable<LegionState["controllerLocator"]>
  ): Promise<void> {
    if (this.controllerRegistrationWait?.locator !== locator) return;
    this.controllerRegistrationWait = undefined;
    const token = controllerToken(this.deps.state.project);
    if (this.deps.state.roles[token]) return;
    const stillAlive = await this.controllerAlive();
    if (this.deps.state.roles[token]) return;
    if (!stillAlive) return;
    if (this.deps.state.controllerLocator !== locator) return;
    try {
      await this.stopProcess(token, locator, this.workerStopTimeoutMs);
    } catch (error) {
      console.error(
        "[legion] failed to stop a stuck controller pane; leaving it in place rather than orphaning it:",
        error
      );
      return;
    }
    delete this.deps.state.controllerLocator;
    await this.ensureController();
  }

  /** Probes a tree's recorded locator for liveness. Backfills `locator.tmuxPaneId` once
   * confirmed alive if it was never recorded (state predating the field, or any other
   * pane-id-less write) — see `reconcileTmuxWindows`'s doc comment for why a locator missing
   * its own pane id exempts its whole window from pane-level reaping; this is what shrinks
   * that exemption to nothing over time. */
  async probe(treeKey: IssueKey): Promise<"alive" | "dead"> {
    const tree = this.deps.state.trees[treeKey];
    const locator = tree?.locator;
    if (!locator) return "dead";

    const target = locator.tmuxPaneId ?? locator.tmuxWindowId;
    const pid = await tmux.panePid(this.deps.run, target);
    if (pid === undefined) return "dead";
    if (!(await this.isOmpPane(pid))) return "dead";
    if (locator.tmuxPaneId === undefined) {
      const paneId = await tmux.firstPaneId(this.deps.run, locator.tmuxWindowId);
      if (paneId !== undefined) {
        locator.tmuxPaneId = paneId;
        await this.persist();
      }
    }
    return "alive";
  }

  async controlDirective(
    tree: IssueKey,
    directive: ControlDirective,
    redeliver = true
  ): Promise<boolean> {
    const generation = this.deps.state.trees[tree]?.generation;
    if (generation === undefined) throw new Error(`Unknown Legion tree: ${tree}`);
    const reply = controlReplyType(
      await this.deps.natsRequest(
        `legion.ctl.${sanitizeToken(tree)}.${generation}`,
        JSON.stringify(directive)
      )
    );
    if (reply === "ack") {
      if (redeliver && "redeliver" in directive) {
        this.deps.natsPublish(directive.redeliver.topic, directive.redeliver.payload);
      }
      return true;
    }
    if (directive.type !== "shutdown") {
      this.publishController({
        type: "revive-failed",
        issue: directive.issue,
        role: "architect",
      });
    }
    return false;
  }

  async resurrect(treeKey: IssueKey): Promise<void> {
    const current = this.resurrecting.get(treeKey);
    if (current) return current;

    const resurrection = this.resurrectDeadTree(treeKey).finally(() => {
      this.resurrecting.delete(treeKey);
    });
    this.resurrecting.set(treeKey, resurrection);
    return resurrection;
  }
  /** Connects the root architect's shim socket on `/process/started`, exactly as `workerReady`
   * does for a phase worker, so the shim's pre-connect backlog drains. The daemon holds no
   * events to replay here: a resurrected root's missed wake is recovered by the overseer
   * catch-up snapshot (`onTreeReady`, called alongside this by the same route), not by this
   * method. */
  async markTreeReady(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (!tree.locator?.socketPath) return;
    await this.workerClient(
      roleToken(this.deps.state.project, treeKey, "architect"),
      tree.locator.socketPath
    );
  }

  /**
   * Marks a tree lingering and releases its admission slot, awaiting the
   * full release-promote-spawn cascade this can trigger (see `releaseSlot`,
   * `advancePromotionSweep`, `startRoot`) before returning. This runs inside
   * the durable lane's dispatch-before-save transaction (`applyDurableEvent`
   * via `onLinger`): by the time this resolves, any promoted tree's spawn
   * has already settled (locator recorded, or handled as a launch failure —
   * `startRoot` never rethrows) and every mutation is captured by `persist`,
   * so the transaction's outer save can never observe a promoted tree
   * without a matching spawn attempt. A rejected `persist` (a `saveState`
   * failure) propagates out of this function and becomes fatal, same as
   * any other durable effect.
   */
  async beginLinger(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    tree.status = "lingering";
    tree.lingerUntil = new Date(
      this.deps.now() + this.deps.config.lingerHours * HOUR_MS
    ).toISOString();
    this.clearTreePhases(treeKey);
    await this.releaseSlot(treeKey);
    await this.persist();
  }

  /** `closeTree` gracefully stops every process under the tree itself (root and every worker,
   * each over its own shim socket) before clearing their locators and claims, so an expired
   * linger has nothing left to do beyond that one call. A `StopFailed` from a process that
   * would not stop propagates: this fire-and-forget call's own caller (the linger sweep timer)
   * already logs and moves on, and the tree is left `lingering` with a fresh `lingerUntil` for
   * the next sweep tick to retry. */
  async expireLinger(treeKey: IssueKey): Promise<void> {
    await this.closeTree(treeKey);
  }

  async handleException(exception: ExceptionInfo): Promise<void> {
    const parsed = parseRoleToken(this.deps.state.project, exception.roleToken);
    if (!parsed) return;
    if ("controller" in parsed) {
      await this.ensureController();
      return;
    }

    const root = this.rootForIssue(parsed.issue);
    if (!root) throw new Error(`No Legion tree records issue ${parsed.issue}`);

    if (parsed.role === "architect" && parsed.issue === root) {
      if ((await this.probe(root)) === "alive") {
        await this.controlDirective(root, {
          type: "reclaim-architect",
          issue: parsed.issue,
          redeliver: exception.original,
        });
      } else {
        await this.resurrect(root);
      }
      return;
    }

    await this.resumeWorker(root, parsed.issue, parsed.role);
  }

  /**
   * Recovers a role's missed wake by probing its own worker locator (never the root's) and, if
   * dead, resuming the same agent through the existing `spawnWorker` resume path (`--resume`,
   * never fresh) with a state-derived catch-up as its prompt instead of the raw missed event —
   * shared by a role-lane delivery exception, the durable lane's `onUndeliverable` 404, and a
   * dead-launch retry. A role with no claim, or a claim with neither a locator nor a resumable
   * identity (`resumeSessionFile`, or its locator's own `ompSessionFile`), was never spawned or
   * has nothing left to resume: there is nothing to recover, so this is a no-op (its eventual
   * first spawn's own catch-up recovers anything missed meanwhile). Critically, a claim whose
   * *locator* was already cleared but whose `resumeSessionFile` survives — exactly the shape
   * `markWorkerDeadLocked` leaves behind for a confirmed-dead worker — is NOT that case: this is
   * the one scenario this method exists to recover, and `spawnWorker`'s own resume-session
   * lookup (`claim.locator?.ompSessionFile ?? claim.resumeSessionFile`) already handles it once
   * reached. Publishes `worker-died` to the tree architect only once the resume attempt itself
   * fails at the launch-failure threshold.
   */
  async resumeWorker(root: IssueKey, issue: IssueKey, role: LegionRole): Promise<void> {
    const token = roleToken(this.deps.state.project, issue, role);
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || (!claim.locator && !claim.resumeSessionFile)) {
      console.error(
        `[legion] resumeWorker no-op for ${token}: no claim or resumable identity (never spawned, or already fully retired) - its eventual first spawn's own catch-up recovers anything missed meanwhile`
      );
      return;
    }
    const catchup = await workerCatchup(this.deps.state, issue, role, this.deps.workerCatchup);
    try {
      await this.spawnWorker(root, issue, role, JSON.stringify(catchup));
    } catch (error) {
      console.error(`[legion] failed to resume worker ${issue}/${role}:`, error);
      const failedClaim = this.deps.state.roles[token];
      const failures =
        failedClaim && "issue" in failedClaim ? (failedClaim.launchFailures ?? 0) : 0;
      // `===`, not `>=`: mirrors `launchWorker`'s own threshold publish - fires exactly once, at
      // the tick `failures` first reaches the threshold, never again on a later attempt against
      // the same already-past-threshold claim.
      if (failures === MAX_LAUNCH_FAILURES) {
        this.publishWorkerDied(root, issue, role);
      }
    }
  }

  private startRoot(issue: IssueKey): Promise<void> {
    const spawn = this.spawnRoot(issue)
      .catch((error) => {
        if (error instanceof SpawnPersistenceFailure) throw error;
        console.error(`[legion] failed to spawn ${issue}:`, error);
      })
      .finally(() => {
        this.spawns.delete(spawn);
      });
    this.spawns.add(spawn);
    return spawn;
  }

  /** Awaits every currently in-flight `startRoot` call, including ones added while draining, so a shutdown's final save never races a spawn's own `saveState`. */
  async drainSpawns(): Promise<void> {
    while (this.spawns.size > 0) {
      await Promise.allSettled([...this.spawns]);
    }
  }

  /** Marks a sweep-launched spawn as settled once its success or failure is known. */
  private settlePromotionSpawn(issue: IssueKey): void {
    const sweep = this.promotionSweep;
    if (sweep?.attempted.has(issue) && sweep.inFlight > 0) sweep.inFlight -= 1;
  }

  private async beginPromotionSweep(initialFailure?: IssueKey): Promise<void> {
    if (this.promotionSweep) {
      await this.advancePromotionSweep();
      return;
    }
    this.promotionSweep = {
      attempted: new Set(initialFailure ? [initialFailure] : []),
      inFlight: 0,
    };
    await this.advancePromotionSweep();
  }

  private async advancePromotionSweep(): Promise<void> {
    const sweep = this.promotionSweep;
    if (!sweep) return;
    const admission = this.deps.state.admission;
    if (admission.active.length >= admission.cap) {
      if (sweep.inFlight === 0) this.promotionSweep = undefined;
      await this.persist();
      return;
    }
    const nextIndex = admission.queue.findIndex((candidate) => {
      const tree = this.ensureTree(candidate);
      return (
        !sweep.attempted.has(candidate) &&
        tree.status === "queued" &&
        !admission.active.includes(candidate)
      );
    });
    if (nextIndex === -1) {
      if (sweep.inFlight === 0) this.promotionSweep = undefined;
      await this.persist();
      return;
    }
    const [next] = admission.queue.splice(nextIndex, 1);
    sweep.attempted.add(next);
    sweep.inFlight += 1;
    admission.active.push(next);
    this.ensureTree(next).status = "active";
    await this.persist();
    await this.startRoot(next);
  }

  private ensureTree(issue: IssueKey): TreeState {
    const existing = this.deps.state.trees[issue];
    if (existing) return existing;
    const tree: TreeState = {
      root: issue,
      generation: 0,
      status: "queued",
      launchFailures: 0,
    };
    this.deps.state.trees[issue] = tree;
    return tree;
  }

  private requireTree(treeKey: IssueKey): TreeState {
    const tree = this.deps.state.trees[treeKey];
    if (!tree) throw new Error(`Unknown Legion tree: ${treeKey}`);
    return tree;
  }

  /** True when `launchWorker` (direct or promotion-triggered) must refuse to touch `issue`: its
   * tree has been reparented away from `treeKey` (`rootForIssue` no longer agrees), the tree is
   * closed or was never recorded at all (a plain `.status === "closed"` check on `requireTree`
   * would throw a generic Error for "never recorded", not the `TreeClosingError` every caller
   * here needs), or `closeTreeLocked` has it mid-teardown right now (`closingTrees`). */
  private isTreeGone(treeKey: IssueKey, issue: IssueKey): boolean {
    const tree = this.deps.state.trees[treeKey];
    return (
      this.rootForIssue(issue) !== treeKey ||
      !tree ||
      tree.status === "closed" ||
      this.closingTrees.has(treeKey)
    );
  }

  /** Public wrapper around `isTreeGone` for a live API route writing a role claim outside the
   * normal spawn/launch/close paths (`/worker/started`, `/process/started`) — throws the same
   * `TreeClosingError` (409-mapped) `spawnWorker`/`workerReady` throw, so a boot handshake that
   * raced a concurrent `closeTree` is rejected the same way any other write into a closing/closed
   * tree is, instead of silently completing into a tree that no longer exists. */
  rejectIfTreeGone(treeKey: IssueKey, issue: IssueKey): void {
    if (this.isTreeGone(treeKey, issue)) throw new TreeClosingError(treeKey);
  }

  /** Runs `fn` inside `token`'s per-role critical section — the same one `spawnWorker`/
   * `workerReady`/`closeTreeLocked`'s own stop-then-delete use — rejecting with `TreeClosingError`
   * both before entering it and again immediately before returning control to `fn`, so a live
   * API route's claim write (currently only `/worker/started`) can never land after
   * `closeTreeLocked` has already deleted this same token's claim, and can never race a
   * concurrent close's own delete once both are serialized on the same token. */
  async mutateLiveRoleClaim<T>(
    treeKey: IssueKey,
    issue: IssueKey,
    token: string,
    fn: () => Promise<T>
  ): Promise<T> {
    this.rejectIfTreeGone(treeKey, issue);
    return this.workerAdmission.mutateClaim(token, async () => {
      this.rejectIfTreeGone(treeKey, issue);
      return fn();
    });
  }

  private async computeResumeArgument(
    issue: IssueKey,
    resumeSessionFile: string | undefined,
    logVerb: string
  ): Promise<string> {
    if (!resumeSessionFile) return "";
    try {
      await stat(resumeSessionFile);
    } catch (error) {
      if (
        typeof error !== "object" ||
        error === null ||
        !("code" in error) ||
        error.code !== "ENOENT"
      ) {
        throw error;
      }
      // Same-agent invariant: a recorded session that has gone missing is a launch failure, never
      // a silent fresh start that would lose the original agent's context.
      throw new Error(
        `Refusing to start ${issue} fresh while ${logVerb}: recorded OMP session file is missing: ${resumeSessionFile}`
      );
    }
    console.info(`[legion] ${logVerb} ${issue} by resuming OMP session ${resumeSessionFile}`);
    return ` --resume=${shellPath(resumeSessionFile)}`;
  }
  /** Provisions the jj workspace and credential wiring shared by every issue's process — the
   * root architect and every phase worker alike. */
  private async provisionWorkspace(issue: IssueKey): Promise<WorkspaceSpec> {
    const [owner] = this.deps.config.repo.split("/") as [string, string];
    return provisionIssueWorkspace(issue, {
      repo: this.deps.config.repo,
      extensionPackage: EXTENSION_PACKAGE,
      stateDir: this.deps.config.stateDir,
      provisioningToken: async () => await this.deps.provisioningToken(owner),
      credentialHelper: this.deps.credentialHelper,
      run: async (command, options) => {
        const result = await this.deps.run(command, options);
        return { ...result, stderr: result.stderr ?? "" };
      },
    });
  }

  private async spawnTree(
    tree: TreeState,
    resume: boolean,
    resumeSessionFile?: string
  ): Promise<void> {
    const workspace = await this.provisionWorkspace(tree.root);
    const promptPath = path.join(EXTENSION_PACKAGE, "roles", "architect-root.md");
    const priorSessionFile = resume
      ? (resumeSessionFile ?? tree.locator?.ompSessionFile)
      : undefined;

    const generation = tree.generation;
    const bootToken = await this.deps.mintBootToken(tree.root, generation);
    const env = tmuxEnv({
      LEGION_TREE: tree.root,
      LEGION_ISSUE: tree.root,
      LEGION_ROLE: "architect",
      LEGION_ROOT_WORKSPACE: workspace.workspaceDir,
      LEGION_GENERATION: String(generation),
      LEGION_BOOT_TOKEN: bootToken,
      LEGION_DAEMON_URL: `http://127.0.0.1:${this.deps.config.port}`,
      LEGION_PROJECT: this.deps.state.project,
      ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
      ENVOY_URL: this.deps.config.envoyUrl,
      LEGION_CONTROL_SUBJECT: `legion.ctl.${sanitizeToken(tree.root)}.${generation}`,
      LEGION_MAX_RECURSION_DEPTH: String(this.deps.config.maxRecursionDepth),
      LEGION_STATE_DIR: this.deps.config.stateDir,
      LEGION_CREDENTIAL_HELPER: this.deps.credentialHelper,
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      PATH: this.deps.panePath,
      DISPATCH_URL: this.deps.config.dispatchUrl,
      DISPATCH_TOKEN: this.deps.config.dispatchToken,
    });
    const addressingPrompt = addressingFragment(
      this.deps.state.project,
      tree.root,
      tree.root,
      "architect"
    );
    const locator = await this.launchShimmedProcess(
      tree.root,
      "architect",
      workspace.workspaceDir,
      promptPath,
      addressingPrompt,
      env,
      priorSessionFile,
      "resurrecting"
    );
    if (
      this.deps.state.trees[tree.root] !== tree ||
      tree.status === "lingering" ||
      tree.status === "closed" ||
      !this.deps.state.admission.active.includes(tree.root) ||
      this.deps.state.issues[tree.root]?.status !== "todo"
    ) {
      if (this.deps.state.trees[tree.root] === tree) tree.locator = locator;
      try {
        await this.stopProcessSerialized(
          roleToken(this.deps.state.project, tree.root, "architect"),
          locator,
          this.workerStopTimeoutMs
        );
        if (this.deps.state.trees[tree.root] === tree) delete tree.locator;
      } catch (error) {
        console.error(`[legion] failed to retire stale root pane for ${tree.root}:`, error);
      }
      return;
    }
    tree.locator = locator;
    tree.status = "active";
    await writeStatus(this.deps.state, this.deps.dispatchClient, tree.root, "in_progress");
  }

  private issueDepth(issue: IssueKey): number {
    let depth = 0;
    let current: IssueKey | undefined = issue;
    const seen = new Set<IssueKey>();
    while (current && !seen.has(current)) {
      seen.add(current);
      const parent: IssueKey | undefined = this.deps.state.issues[current]?.parent;
      if (!parent) break;
      depth += 1;
      current = parent;
    }
    return depth;
  }

  private recordedWindowId(issue: IssueKey): string | undefined {
    if (this.rootForIssue(issue) === issue) {
      const windowId = this.deps.state.trees[issue]?.locator?.tmuxWindowId;
      if (windowId) return windowId;
    }
    for (const claim of Object.values(this.deps.state.roles)) {
      if ("issue" in claim && claim.issue === issue && claim.locator) {
        return claim.locator.tmuxWindowId;
      }
    }
    return this.issueWindowIds.get(issue);
  }

  /**
   * After opening a fresh window for `issue` (first spawn, or a fallback from a dead recorded
   * window), every existing claim for that issue — and the tree locator, if `issue` is a root —
   * must point at the new window id in the same place, or `recordedWindowId` keeps handing a
   * later spawn a stale id and each one opens yet another window instead of splitting into it.
   */
  private rewriteIssueWindowId(issue: IssueKey, windowId: string): void {
    this.issueWindowIds.set(issue, windowId);
    const tree = this.deps.state.trees[issue];
    if (tree?.locator) tree.locator.tmuxWindowId = windowId;
    for (const claim of Object.values(this.deps.state.roles)) {
      if ("issue" in claim && claim.issue === issue && claim.locator) {
        claim.locator.tmuxWindowId = windowId;
      }
    }
  }

  /** Trusts no recorded window id until it is confirmed live, so a human-killed window falls back to a fresh one. */
  private async probedWindowId(issue: IssueKey): Promise<string | undefined> {
    const candidate = this.recordedWindowId(issue);
    if (!candidate) return undefined;
    return (await tmux.windowAlive(this.deps.run, candidate)) ? candidate : undefined;
  }

  /**
   * Connects (or reuses a cached connection) to a role's shim socket. `onIdle` is wired
   * unconditionally for every connection (root architect, controller, phase worker, and
   * sub-architect alike) purely as a trigger to re-check the running-worker queue — occupancy
   * itself is always derived fresh by `runningWorkerCount()` from `state.roles` (the root
   * architect and controller are never in `state.roles` at all, tracked separately via
   * `state.trees`/`state.controllerLocator`), not by opting a connection in or out here.
   */
  private async workerClient(token: string, socketPath: string): Promise<WorkerRpcClient> {
    const existing = this.workerClients.get(token);
    if (existing) return existing;
    const inFlight = this.workerConnections.get(token);
    if (inFlight) return inFlight;
    const connecting = (async () => {
      const client = await this.deps.connectWorkerRpc(socketPath);
      try {
        await client.negotiate();
      } catch (error) {
        client.close();
        throw error;
      }
      this.workerClients.set(token, client);
      client.onIdle(() => this.workerAdmission.promoteWorkerQueue());
      // Detached from this connect call on purpose (the caller must not wait on the worker's
      // eventual close) — `.catch` here is not error recovery, it is the only thing standing
      // between an `onWorkerClientClosed` rejection (tmux/saveState failures included) and an
      // unhandled rejection, which is process-fatal under Bun.
      client.closed
        .then(
          () => this.onWorkerClientClosed(token, client),
          () => this.onWorkerClientClosed(token, client)
        )
        .catch((error) => {
          console.error(`[legion] worker client-closed handler failed for ${token}:`, error);
        });
      return client;
    })().finally(() => {
      if (this.workerConnections.get(token) === connecting) this.workerConnections.delete(token);
    });
    this.workerConnections.set(token, connecting);
    return connecting;
  }

  /** Derives `{treeKey, issue, role}` for `retireUnconfirmedBoot`'s retry accounting from a
   * role token alone — `undefined` for a controller token, or a role whose tree cannot be
   * resolved (an issue whose tree no longer exists). */
  private deriveRetryContext(
    token: string,
    issue: IssueKey
  ): { treeKey: IssueKey; issue: IssueKey; role: LegionRole } | undefined {
    const parsed = parseRoleToken(this.deps.state.project, token);
    if (!parsed || "controller" in parsed) return undefined;
    const treeKey = this.rootForIssue(parsed.issue);
    return treeKey ? { treeKey, issue, role: parsed.role } : undefined;
  }

  /**
   * Fires when a cached worker connection's socket closes at runtime (not a boot-time
   * `reconnectWorkers` probe). A replaced/retired client's late close (this cached entry has
   * already moved on to a newer connection) is a stale event and must never touch the current
   * connection's claim — it returns immediately without evicting anything or reconnecting.
   * Otherwise evicts the now-stale cache entry, then tries at most one reconnect per claim
   * generation — the shim may still be listening with a fresh socket, or momentarily
   * restarting — before concluding the worker is truly dead; a second close for the same
   * generation (the reconnect's own connection also died) goes straight to the dead-worker path
   * instead of chaining into another reconnect attempt, so a flapping socket can never loop
   * forever. A reconnect that *fails to connect* means the worker is confirmed dead; one that
   * connects but whose follow-up `get_state` fails only means the shim is busy — never a reason
   * to kill a live worker, so the claim is left as is. An unconfirmed claim (never reached
   * `/worker/started`, `sessionId` still unset) routes its dead verdict through
   * `retireUnconfirmedBoot` — the same retire/count/enqueue-or-give-up accounting the boot
   * watchdog and `reconnectWorkers` use — rather than `markWorkerDead`'s confirmed-worker path,
   * which only clears the locator with no accounting or requeue: without this branch, a socket
   * that dies before confirmation left its claim locator-less and unqueued, with nothing left
   * to ever revisit it. A root architect or controller connection has no matching
   * `WorkerRoleClaim` (they are tracked via `state.trees`/`state.controllerLocator`), so this is
   * a no-op for them past the cache eviction.
   */
  private async onWorkerClientClosed(token: string, client: WorkerRpcClient): Promise<void> {
    if (this.workerClients.get(token) !== client) return;
    this.workerClients.delete(token);
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || claim.locator === undefined) return;
    const locator = claim.locator;
    const generation = claim.generation;
    const confirmed = claim.sessionId !== undefined;
    const retireDead = async (): Promise<void> => {
      if (confirmed) {
        await this.markWorkerDead(token, locator);
      } else {
        await this.retireUnconfirmedBoot(
          token,
          locator,
          generation,
          this.deriveRetryContext(token, claim.issue)
        );
      }
    };
    const attemptKey = `${token}:${claim.generation ?? 0}`;
    if (this.reconnectAttempted.has(attemptKey)) {
      await retireDead();
      return;
    }
    this.reconnectAttempted.add(attemptKey);
    const probe = await probeWorkerSocket(
      (socketPath) => this.workerClient(token, socketPath),
      locator.socketPath
    );
    if (!probe.client) {
      // Reconnect failed to connect at all; the worker is confirmed dead.
      await retireDead();
      return;
    }
    if (!probe.stateAnswered) {
      console.error(
        `[legion] worker ${token} reconnected but get_state failed (treating as busy, not dead):`,
        probe.stateError
      );
    }
  }

  /** The `legion worker-shim --socket <path> -- <inner>` command every Legion OMP process — root,
   * phase worker, and controller alike — runs inside its tmux pane. */
  private shimmedShellCommand(
    workspaceDir: string,
    socketPath: string,
    innerCommand: string
  ): string {
    return `cd ${shellPath(workspaceDir)} && ${shellPath(process.execPath)} ${shellPath(DAEMON_CLI_ENTRYPOINT)} worker-shim --socket ${shellPath(socketPath)} -- ${innerCommand}`;
  }

  private async prepareSocket(name: string): Promise<string> {
    const socketPath = path.join(this.deps.config.stateDir, "workers", `${name}.sock`);
    await mkdir(path.dirname(socketPath), { recursive: true });
    await rm(socketPath, { force: true });
    return socketPath;
  }

  /**
   * Opens (or splits into) the tmux window for `issue`, running `legion worker-shim` around the
   * caller's OMP invocation with its resume argument and system prompt. Shared by the root
   * architect (a worker of its own issue) and every phase worker: the issue's first process
   * opens a fresh window named for the issue; every later process on that issue splits into it.
   */
  private async launchShimmedProcess(
    issue: IssueKey,
    role: LegionRole,
    workspaceDir: string,
    promptPath: string,
    addressingPrompt: string,
    envPairs: string[],
    resumeSessionFile: string | undefined,
    logVerb: string
  ): Promise<WorkerLocator> {
    await (this.deps.statPrompt ?? stat)(promptPath);
    const resumeArgument = await this.computeResumeArgument(issue, resumeSessionFile, logVerb);
    const innerCommand = `${this.deps.ompInvocation}${resumeArgument} --mode rpc --append-system-prompt "$(cat ${shellPath(promptPath)})" --append-system-prompt ${shellPath(addressingPrompt)}`;
    const socketPath = await this.prepareSocket(workerSocketBasename(issue, role));
    const shellCommand = this.shimmedShellCommand(workspaceDir, socketPath, innerCommand);

    const session = `legion-${this.deps.state.project}`;
    const { tmuxWindowId, tmuxPaneId } = await this.serialize(
      this.issueLaunchQueue,
      issue,
      async () => {
        const existingWindowId = await this.probedWindowId(issue);
        if (existingWindowId) {
          const { paneId } = await tmux.splitWindow(this.deps.run, existingWindowId, [
            ...envPairs,
            shellCommand,
          ]);
          return { tmuxWindowId: existingWindowId, tmuxPaneId: paneId };
        }
        const window = await tmux.openWindow(
          this.deps.run,
          session,
          treeName(issue),
          [...envPairs, shellCommand],
          session
        );
        this.rewriteIssueWindowId(issue, window.windowId);
        return { tmuxWindowId: window.windowId, tmuxPaneId: window.paneId };
      }
    );

    return { tmuxSession: session, tmuxWindowId, tmuxPaneId, socketPath };
  }

  private async launchWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    task: string
  ): Promise<void> {
    const token = roleToken(this.deps.state.project, issue, role);
    const generation = (claim?.generation ?? 0) + 1;
    // Checked here, before any I/O (including opening a real pane): a promotion-triggered launch
    // (WorkerAdmission's drain -> the trackLaunch-wrapped dep in the constructor) has no entry
    // fence of its own the way spawnWorker's decision does -- this is that fence for every caller
    // of launchWorker, direct or promoted, so a launch queued behind a since-closing (or already
    // gone) tree never attempts real workspace/tmux I/O for it. The post-open check further down
    // covers the remaining window between this check and the pane actually opening.
    if (this.isTreeGone(treeKey, issue)) {
      throw new TreeClosingError(treeKey);
    }
    try {
      const workspace = await this.provisionWorkspace(issue);
      const promptPath = path.join(EXTENSION_PACKAGE, "roles", `${role}.md`);
      const resumeSessionFile = claim?.locator?.ompSessionFile ?? claim?.resumeSessionFile;

      const bootToken = await this.deps.mintWorkerBootToken(
        treeKey,
        issue,
        role,
        generation,
        claim?.sessionId
      );
      const env = tmuxEnv({
        LEGION_TREE: treeKey,
        LEGION_ISSUE: issue,
        LEGION_ROLE: role,
        LEGION_WORKSPACE: workspace.workspaceDir,
        LEGION_BOOT_TOKEN: bootToken,
        LEGION_GENERATION: String(generation),
        LEGION_DAEMON_URL: `http://127.0.0.1:${this.deps.config.port}`,
        LEGION_PROJECT: this.deps.state.project,
        LEGION_STATE_DIR: this.deps.config.stateDir,
        LEGION_CREDENTIAL_HELPER: this.deps.credentialHelper,
        ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
        ENVOY_URL: this.deps.config.envoyUrl,
        GIT_CONFIG_COUNT: "0",
        GIT_TERMINAL_PROMPT: "0",
        PATH: this.deps.panePath,
        DISPATCH_URL: this.deps.config.dispatchUrl,
        DISPATCH_TOKEN: this.deps.config.dispatchToken,
      });
      const addressingPrompt = addressingFragment(this.deps.state.project, treeKey, issue, role);
      const locator = await this.launchShimmedProcess(
        issue,
        role,
        workspace.workspaceDir,
        promptPath,
        addressingPrompt,
        env,
        resumeSessionFile,
        "respawning"
      );
      // `closeTree` may have started tearing down this tree while this launch's I/O was in
      // flight above -- its fixed-point stop loop can only stop locators it could already see
      // when it ran (mitigated further by `inFlightLaunches`, but the window between opening the
      // pane and this check is still real), so writing a fresh claim now would leave this
      // brand-new pane live and completely untracked otherwise. Report the closing error
      // directly, not via the launch-failure path below: this was never a launch failure, and
      // counting it as one would eventually mark the role `launch-failed` for a tree that is
      // simply gone. `WorkerAdmission`'s own "tree deleted" concern -- `rootForIssue` no longer
      // resolving to `treeKey`, or the tree's own status already `"closed"` -- is checked
      // alongside `closingTrees` below: any one of the three means nothing durable should be
      // written for this launch.
      const freshLocator: WorkerLocator = {
        ...locator,
        ...(resumeSessionFile ? { ompSessionFile: resumeSessionFile } : {}),
      };
      const freshClaim: WorkerRoleClaim = {
        issue,
        role,
        // sessionId deliberately not carried over: it stays unset until /worker/started
        // confirms this generation's boot, so a concurrent spawnWorker call during the boot
        // window sees an unconfirmed claim rather than racing a stale one (both for a fresh
        // spawn and a resume, where OMP reports the same session id it had before).
        ...(claim?.agentId ? { agentId: claim.agentId } : {}),
        // Carried over, never reset by a mere relaunch: a boot the watchdog never confirms
        // must accumulate across repeated retries so the threshold below is ever reachable,
        // even when every retry successfully opens a pane but none ever completes
        // `/worker/started`. `/worker/started`'s own success path clears it — that is the
        // actual recovery signal, not merely reopening a pane.
        launchFailures: claim?.launchFailures ?? 0,
        generation,
        pendingAssignment: task,
        bootTokenHash: secretHash(bootToken).toString("hex"),
        ...(claim?.sessionId ? { expectedSessionId: claim.sessionId } : {}),
        locator: freshLocator,
        // resumeSessionFile deliberately dropped: a fresh locator now carries its own
        // ompSessionFile, so the standalone fallback field is stale.
      };
      if (this.isTreeGone(treeKey, issue)) {
        // Write the locator BEFORE attempting to retire it: a `StopFailed` here must never
        // discard the only durable record of a pane that might still be alive. Only a
        // successful retire clears it; `closeTree`'s own fixed-point loop or the periodic sweep
        // retries the stop from whatever this leaves behind on failure.
        this.deps.state.roles[token] = freshClaim;
        await this.deps.saveState();
        await this.retireWorkerLocator(token, freshLocator);
        delete this.deps.state.roles[token];
        await this.deps.saveState();
        throw new TreeClosingError(treeKey);
      }

      this.deps.state.roles[token] = freshClaim;
      const queueIndex = this.deps.state.workerAdmission.queue.indexOf(token);
      if (queueIndex !== -1) this.deps.state.workerAdmission.queue.splice(queueIndex, 1);
      // Persists the new claim's locator before this call resolves and the caller (`launchOrQueue`/
      // `promoteQueuedWorker`'s own finally) releases the reservation: a crash between opening
      // this pane and this point would otherwise leave a real, running worker-shim process the
      // daemon has completely forgotten about on disk (`reconnectWorkers` only ever reconnects
      // to locators it can read back from state, and `reconcileTmuxWindows` only reaps whole
      // *windows* it doesn't recognize -- a split-pane worker shares its window with the tree's
      // other recognized occupants, so the window itself stays "known" and the orphaned pane
      // inside it is never swept). Holding the reservation through this save is the honest cost
      // of that guarantee: a concurrent admission decision waits slightly longer to see this
      // slot as durably occupied, instead of trusting an in-memory claim that might still
      // vanish on a crash.
      try {
        await this.deps.saveState();
      } catch (saveError) {
        // The pane already exists at this point (opened, locator and `pendingAssignment`
        // already written above) -- a failure here is a durable-state persistence failure, not
        // a launch failure: the launch itself succeeded. Never re-queues or rotates this
        // token (it already has a live pane; re-queuing it would launch a second pane for the
        // same issue/role the next time it is promoted) and never bumps `launchFailures` (this
        // is not what that counter tracks -- see the outer `catch` below, which only ever runs
        // for a failure *before* a pane exists). Retries the persist exactly once more; if
        // that also fails, logs it and leaves the in-memory claim authoritative -- never
        // rethrown, since the launch genuinely succeeded regardless of whether this save did.
        // The pane's own `/worker/started` -> `/worker/ready` handshake calls back into the
        // daemon independent of this save and persists normally on its own next success.
        console.error(
          `[legion] failed to persist ${token}'s locator after a successful launch (pane is live regardless):`,
          saveError
        );
        try {
          await this.deps.saveState();
        } catch (retryError) {
          console.error(
            `[legion] retry-persist for ${token} also failed; in-memory claim remains authoritative:`,
            retryError
          );
        }
      }
      this.bootWatchdog.arm(treeKey, issue, role, token, locator, generation);
    } catch (error) {
      if (error instanceof TreeClosingError) throw error;
      if (error instanceof StopFailed) throw error;
      const failures = (claim?.launchFailures ?? 0) + 1;
      const failingClaim = this.deps.state.roles[token];
      if (failingClaim && "issue" in failingClaim) {
        failingClaim.launchFailures = failures;
      } else {
        this.deps.state.roles[token] = { issue, role, launchFailures: failures };
      }
      // `===`, not `>=`: this fires exactly once, at the tick `failures` first reaches the
      // threshold — not on every later attempt against the same claim (a below-threshold
      // rotation retries the same token repeatedly; `>=` would republish on each one past the
      // crossing).
      if (failures === MAX_LAUNCH_FAILURES) {
        this.deps.natsPublish(
          roleTopic(roleToken(this.deps.state.project, treeKey, "architect")),
          JSON.stringify({ type: "launch-failed", issue, role, failures })
        );
      }
      await this.deps.saveState();
      throw error;
    }
  }

  private async spawnController(controllerSecret: string): Promise<void> {
    const controllerDir = path.join(this.deps.config.stateDir, "controller");
    const promptPath = path.join(EXTENSION_PACKAGE, "roles", "controller-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    await this.writeOmpConfig(controllerDir);
    const socketPath = await this.prepareSocket("controller");
    const innerCommand = `${this.deps.ompInvocation} --mode rpc --append-system-prompt "$(cat ${shellPath(promptPath)})"`;
    const shellCommand = this.shimmedShellCommand(controllerDir, socketPath, innerCommand);
    const session = `legion-${this.deps.state.project}`;
    const env = tmuxEnv({
      LEGION_CONTROLLER: "1",
      LEGION_ROLE: "controller",
      LEGION_CONTROLLER_SECRET: controllerSecret,
      LEGION_DAEMON_URL: `http://127.0.0.1:${this.deps.config.port}`,
      LEGION_PROJECT: this.deps.state.project,
      ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
      ENVOY_URL: this.deps.config.envoyUrl,
      PATH: this.deps.panePath,
      DISPATCH_URL: this.deps.config.dispatchUrl,
      DISPATCH_TOKEN: this.deps.config.dispatchToken,
    });
    const window = await tmux.openWindow(
      this.deps.run,
      session,
      "controller",
      [...env, shellCommand],
      session
    );
    this.deps.state.controllerLocator = {
      tmuxSession: session,
      tmuxWindowId: window.windowId,
      tmuxPaneId: window.paneId,
      socketPath,
    };
    await this.deps.saveState();
  }

  /** Connects the controller's shim socket on `/controller/ready`, exactly as `markTreeReady`
   * does for the root architect, so the shim's pre-connect backlog drains. Best-effort: a shim
   * connect failure (listener race, stale socket, RPC timeout) must never block
   * `/controller/ready` from accepting the role — the daemon holds no events to replay here
   * either; the connection is retried on the next `spawnWorker`/`workerReady`/reconnect attempt
   * that touches this socket.
   */
  async markControllerReady(): Promise<void> {
    const socketPath = this.deps.state.controllerLocator?.socketPath;
    if (!socketPath) return;
    try {
      await this.workerClient(controllerToken(this.deps.state.project), socketPath);
    } catch (error) {
      console.error("[legion] failed to connect controller shim socket on ready:", error);
    }
  }

  private async writeOmpConfig(directory: string): Promise<void> {
    await mkdir(path.join(directory, ".omp"), { recursive: true });
    await writeFile(path.join(directory, ".omp", "config.yml"), "", "utf8");
  }

  /** Revokes a claim's session capability (a no-op if it never had one — never spawned, or
   * already revoked) the instant its process is retired or torn down, so a stale process can
   * never keep minting grants once the daemon has stopped trusting it. The single chokepoint
   * every path that retires or deletes a role's claim goes through: `removeTreeWindow` (the
   * root architect), `retireWorkerLocator` (a worker, via `markWorkerDeadLocked` and every
   * other retirement), and `closeTree` (root and every worker, on tree shutdown). */
  private revokeRoleClaim(claim: WorkerRoleClaim | undefined): void {
    if (claim?.sessionId) this.deps.revokeSessionCapability(claim.sessionId);
  }

  /** Called only once `probe` has already confirmed the recorded pane is dead, so `stopProcess`
   * normally has nothing live to gracefully close and degrades straight to the kill; routed
   * through it anyway for the rare race where the pane outlived that probe. Best-effort: a
   * `StopFailed` here is logged and swallowed rather than blocking `resurrectDeadTree` — the
   * probe already confirmed this pane dead, so a failed kill of an already-dead pane is a stray
   * cleanup problem, never a reason to refuse resurrecting the tree onto a fresh one. */
  private async removeTreeWindow(tree: TreeState): Promise<void> {
    const architectToken = roleToken(this.deps.state.project, tree.root, "architect");
    const architectClaim = this.deps.state.roles[architectToken];
    this.revokeRoleClaim(architectClaim && "issue" in architectClaim ? architectClaim : undefined);
    const locator = tree.locator;
    delete tree.locator;
    if (!locator) return;
    try {
      await this.stopProcessSerialized(
        roleToken(this.deps.state.project, tree.root, "architect"),
        locator,
        this.workerStopTimeoutMs
      );
    } catch (error) {
      console.error(
        `[legion] failed to clean up ${tree.root}'s dead pane before resurrection:`,
        error
      );
    }
  }

  private async isOmpPane(pid: number): Promise<boolean> {
    if ((await this.deps.run(["kill", "-0", String(pid)])).exitCode !== 0) return false;
    try {
      const cmdline = this.deps.readProcessCmdline
        ? await this.deps.readProcessCmdline(pid)
        : await readFile(`/proc/${pid}/cmdline`, "utf8");
      return cmdline.includes("omp");
    } catch {
      return false;
    }
  }

  /** Sends the shim a `{type:"shutdown"}` frame and waits up to `timeoutMs` for its socket to
   * close before falling back to killing the pane; the underlying timer is cancellable so a
   * graceful stop that resolves quickly doesn't leave a stray one running for the rest of
   * `timeoutMs`. A test-injected `deps.sleep` has no real timer behind it to cancel. */
  private stopTimeout(ms: number): { timedOut: Promise<boolean>; cancel: () => void } {
    if (this.deps.sleep) {
      return { timedOut: this.deps.sleep(ms).then(() => true), cancel: () => {} };
    }
    const timer = createCancellableSleep();
    return { timedOut: timer.sleep(ms).then(() => true), cancel: () => timer.cancel() };
  }

  /** Connects to a worker's shim purely to send `{type:"shutdown"}` — reuses an already-cached
   * client if one exists, otherwise connects fresh WITHOUT negotiating: the shim intercepts the
   * shutdown frame itself (`worker-shim.ts`) and never forwards it to OMP, so it needs no RPC
   * round-trip OMP must be free to answer. Negotiating first here would make stopping a busy
   * worker depend on that same busy worker responding to an unrelated RPC command — exactly the
   * grace period this exists to give it. Returns `undefined` only when the socket itself is
   * unreachable (the shim is already gone). */
  private async stopClient(
    token: string,
    socketPath: string
  ): Promise<WorkerRpcClient | undefined> {
    const cached = this.workerClients.get(token);
    if (cached) return cached;
    try {
      return await this.deps.connectWorkerRpc(socketPath);
    } catch {
      return undefined;
    }
  }

  /**
   * The one graceful-shutdown implementation every stop path funnels through. Sends the shim a
   * `{type:"shutdown"}` frame over its socket (connecting first if needed — see `stopClient`) so
   * it closes the wrapped OMP process's stdin, then waits up to `timeoutMs` for the shim's own
   * socket to close (`client.closed`) before falling back to killing the pane. Never SIGTERMs
   * OMP. Only a clean resolve of `client.closed` is a confirmed graceful close and skips the
   * kill; a socket error while waiting is NOT proof the process exited (a reset proves nothing
   * about the pane), so it is treated exactly like a timeout — fall through to `client.close()`
   * and the kill-pane attempt below. A dead/unreachable shim, or `skipGraceful` (the caller
   * already confirmed nothing live is there to ask), also skip straight to the kill. Every real
   * locator carries a pane id (`launchShimmedProcess` always records one); a locator without one
   * is a corrupt or legacy record, not a case to silently degrade for. Throws `StopFailed` for
   * any `kill-pane` failure other than the pane having already been reaped on its own (`"can't
   * find pane"`) — the caller must never treat the process as stopped, or its claim/locator as
   * safe to delete, when it cannot confirm that.
   */
  private async stopProcess(
    token: string,
    locator: { tmuxWindowId: string; tmuxPaneId?: string; socketPath?: string },
    timeoutMs: number,
    options?: { skipGraceful?: boolean }
  ): Promise<void> {
    const client =
      !options?.skipGraceful && locator.socketPath
        ? await this.stopClient(token, locator.socketPath)
        : undefined;
    if (client) {
      client.shutdown();
      const { timedOut: timeoutPromise, cancel } = this.stopTimeout(timeoutMs);
      const unconfirmed = await Promise.race([
        client.closed.then(
          () => false,
          () => true
        ),
        timeoutPromise,
      ]);
      cancel();
      if (this.workerClients.get(token) === client) this.workerClients.delete(token);
      if (!unconfirmed) return;
      client.close();
    }
    if (!locator.tmuxPaneId) {
      throw new Error(`Worker locator for ${token} is missing a pane id`);
    }
    const killed = await tmux.killPane(this.deps.run, locator.tmuxPaneId);
    if (killed.exitCode !== 0 && !/can't find pane/.test(killed.stderr ?? "")) {
      throw new StopFailed(
        token,
        `kill-pane ${locator.tmuxPaneId} exited ${killed.exitCode}${killed.stderr ? `: ${killed.stderr}` : ""}`
      );
    }
  }

  /** `stopProcess`, serialized through `WorkerAdmission.mutateClaim`'s per-role critical section --
   * the same one `spawnWorker`/`markWorkerDead`/`promoteQueuedWorker` acquire -- so a stop and a
   * concurrent (re)spawn for the same role can never interleave. Used by every caller that is
   * NOT already running inside that critical section for this exact token: `retireWorkerLocator`'s
   * call from inside `launchWorker`/`markWorkerDeadLocked` (themselves already running inside it)
   * deliberately calls the raw, unserialized `stopProcess` instead -- re-acquiring the same
   * token's critical section from within a callback already gating it would await a promise
   * that can only settle after that same callback returns, deadlocking forever. */
  private stopProcessSerialized(
    token: string,
    locator: { tmuxWindowId: string; tmuxPaneId?: string; socketPath?: string },
    timeoutMs: number,
    options?: { skipGraceful?: boolean }
  ): Promise<void> {
    return this.workerAdmission.mutateClaim(token, () =>
      this.stopProcess(token, locator, timeoutMs, options)
    );
  }

  /** Before a worker's locator is replaced or cleared during `spawnWorker`'s own launch decision,
   * `markWorkerDeadLocked`'s dead-socket retirement, or a full tree `closeTree` (via
   * `stopProcessSerialized`), retires whatever pane it may still be running -- a shorter timeout
   * than a full tree shutdown when called for a single stale worker, not a whole tree. May throw
   * `StopFailed`; callers never treat a locator as safe to clear or a replacement as safe to
   * launch when it does. */
  private async retireWorkerLocator(token: string, locator: WorkerLocator): Promise<void> {
    this.cancelBootWatchdog(token);
    const claim = this.deps.state.roles[token];
    this.revokeRoleClaim(claim && "issue" in claim ? claim : undefined);
    await this.stopProcess(token, locator, this.workerStopTimeoutMs);
  }

  private get workerStopTimeoutMs(): number {
    return this.deps.config.workerStopTimeoutSeconds * 1000;
  }

  private get treeStopTimeoutMs(): number {
    return this.deps.config.treeStopTimeoutSeconds * 1000;
  }

  /** Probes the controller's recorded locator for liveness. Backfills `tmuxPaneId` once
   * confirmed alive if it was never recorded — see `probe`/`reconcileTmuxWindows`'s doc
   * comments for why. */
  private async controllerAlive(): Promise<boolean> {
    const locator = this.deps.state.controllerLocator;
    if (!locator) return false;
    const target = locator.tmuxPaneId ?? locator.tmuxWindowId;
    const pid = await tmux.panePid(this.deps.run, target);
    if (pid === undefined) {
      delete this.deps.state.controllerLocator;
      return false;
    }
    const alive = await this.isOmpPane(pid);
    if (!alive) {
      delete this.deps.state.controllerLocator;
      return false;
    }
    if (locator.tmuxPaneId === undefined) {
      const paneId = await tmux.firstPaneId(this.deps.run, locator.tmuxWindowId);
      if (paneId !== undefined) {
        locator.tmuxPaneId = paneId;
        await this.persist();
      }
    }
    return true;
  }

  private async resurrectDeadTree(treeKey: IssueKey): Promise<void> {
    if ((await this.probe(treeKey)) === "alive") return;
    const tree = this.requireTree(treeKey);
    const resumeSessionFile = tree.locator?.ompSessionFile;
    await this.removeTreeWindow(tree);
    tree.status = "dead";
    await this.spawnRoot(treeKey, true, resumeSessionFile);
  }

  private rootForIssue(issue: IssueKey): IssueKey | undefined {
    return resolveRootForIssue(this.deps.state, issue);
  }

  private clearTreePhases(treeKey: IssueKey): void {
    for (const issue of Object.keys(this.deps.state.phases) as IssueKey[]) {
      if (this.rootForIssue(issue) === treeKey) delete this.deps.state.phases[issue];
    }
  }

  private publishWorkerDied(root: IssueKey, issue: IssueKey, role: LegionRole): void {
    this.deps.natsPublish(
      roleTopic(roleToken(this.deps.state.project, root, "architect")),
      JSON.stringify({ type: "worker-died", issue, role })
    );
  }

  private publishController(
    payload:
      | { type: "revive-failed"; issue: IssueKey; role: LegionRole }
      | { type: "launch-failed"; issue: IssueKey; failures: number }
  ): void {
    this.deps.natsPublish(
      roleTopic(controllerToken(this.deps.state.project)),
      JSON.stringify(payload)
    );
  }

  private persist(): Promise<void> {
    return this.deps.saveState();
  }
}
