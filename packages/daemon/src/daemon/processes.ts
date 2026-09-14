import { rm } from "node:fs/promises";
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
import type { JjIdentity } from "@legion/workspace";
import {
  issueWorkspaceDir,
  type ProvisionIssueWorkspaceDeps,
  type RemoveIssueWorkspaceResult,
  removeIssueWorkspace,
} from "@legion/workspace";
import type { CommandResult, CommandRunnerOptions } from "../state/fetch";
import { secretHash } from "./api/auth";
import { rootForIssue as resolveRootForIssue } from "./api/context";
import { overseerCatchup, type WorkerCatchupDeps, workerCatchup } from "./catchup";
import type { DaemonConfig } from "./config";
import { type DispatchClient, writeStatus } from "./dispatch-client";
import type { ExceptionInfo } from "./events";
import { gitIdentityEnv } from "./github-app-env";
import { appRoleForLegionRole } from "./github-apps";
import {
  type AdmissionDriftRepair,
  activePhaseLabel,
  describeAdmissionDrift,
  isActivePhase,
  isBystanderCatchup,
  isBystanderRole,
  type LegionState,
  liveAncestorTree,
  owningArchitect,
  type PendingAssignment,
  repairAdmissionDrift,
  samePendingTask,
  staleQueueEntryReason,
  type TreeState,
  type WorkerRoleClaim,
} from "./legion-state";
import {
  PromptNotStarted,
  type PromptNotStartedReason,
  StopFailed,
  TreeClosingError,
} from "./process-errors";
import { MAX_RESENDS, ResendLedger } from "./resend-ledger";
import {
  awaitShutdown,
  boundedWait,
  DAEMON_CLI_ENTRYPOINT,
  type Locator,
  locatorHandles,
  type ProbeResult,
  ProcessStopFailed,
  probeWorker,
  type Runtime,
  sameProcess,
  shellPath,
} from "./runtime";
import {
  DISPATCH_TOKEN_SECRET,
  grantSecretName,
  processSecretNames,
  pruneSecretFiles,
  type SharedSecretName,
  secretFilePath,
} from "./secrets";
import { MAX_LAUNCH_FAILURES, type PromptRetireVerdict, WorkerAdmission } from "./worker-admission";
import { workerBinDir } from "./worker-bin";
import { WorkerBootWatchdog } from "./worker-boot-watchdog";
import type { PromptReceipt, WorkerRpcClient } from "./worker-rpc";

const HOUR_MS = 60 * 60 * 1000;

/**
 * Wraps a `saveState` rejection that occurs after `spawnTree` has already
 * succeeded (a real root process was running when the save was attempted).
 * Distinguishes this from a genuine launch failure so `startRoot` never
 * rolls back generation/status/launchFailures or requeues on it — that
 * spawn is not the problem. The process itself is stopped before this is
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

/** The `ResendLedger` key for one role-lane message: the topic and the verbatim payload every
 * failed copy of that message shares. Never the event id — the listener mints a fresh `event_id`
 * for each publish and the exception reports the failed copy's, so a per-event-id count reaches
 * 1 per chain in production and bounds nothing. Two byte-identical messages to one role whose
 * deliveries both fail within `RESEND_LEDGER_TTL_MS` share one budget; each re-send still
 * carries its own exception's dedupe key, so the plugin's dedupe never conflates them. */
function resendChainKey(original: ExceptionInfo["original"]): string {
  return `${original.topic}\n${original.payload}`;
}

export type ControlDirective =
  | { type: "reclaim-architect"; issue: IssueKey; redeliver: Redelivery }
  | { type: "shutdown" };

/** A child whose stray root tree the boot repair removed, and the parent whose architect now owns
 * it -- what `index.ts` wakes with `child-adopted` once boot admission has settled. */
export interface ChildAdoption {
  child: IssueKey;
  parent: IssueKey;
}

export interface ProcessManagerDeps {
  state: LegionState;
  saveState(): Promise<void>;
  config: DaemonConfig;
  /** The `PATH` every spawned process receives: the daemon's resolved tool environment
   * (`resolveDaemonEnvironment`), `<state_dir>/bin` first and never a `worker-bin` entry —
   * `credentialProcessEnvironment` prepends the pane's own. */
  processPath: string;
  /** The directory holding every role prompt (`ROLE_PROMPT_FILES`, resolved and verified at boot by
   * `resolveDaemonEnvironment`): the checkout's `packages/pi-envoy/roles` on a tmux host,
   * `/opt/legion/roles` in the worker image. Never a path relative to this module — inside the
   * compiled `legion` binary that resolves through Bun's virtual `/$bunfs/root` to `/pi-envoy/roles`,
   * which exists nowhere. */
  rolePromptsDir: string;
  credentialHelper: string;
  /** Publishes one of the daemon's own notices to a role topic — `worker-queued`,
   * `worker-started`, `worker-died`, `launch-failed`, the controller's `revive-failed`, and a
   * redelivered exception payload. Fire-and-forget: the caller never waits on it, and a failed
   * publish is logged by the implementation, never thrown back here (a missed wake is recovered
   * by the role's catch-up, never by a replay). `index.ts` wires it to the Envoy listener's
   * `POST /v1/messages/publish` (`envoyPublish`), which wraps the JSON in an envelope and routes
   * it to the topic's live holder — never to a bare `nats.publish`: the listener validates every
   * role-lane message as an envelope and drops a bare payload as `invalid envelope: event_id is
   * required`, so a raw publish reaches nobody. `dedupeKey` is the listener publish body's
   * `dedupe_key`, set only by `handleException`'s re-send of a failed copy (the triggering
   * exception's own key) so the plugin's dedupe recognises the copy as the message it already
   * saw; every other notice leaves it unset and the listener mints a fresh key. */
  publishRole(topic: string, json: string, dedupeKey?: string): void;
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
  /** The runtime boundary (`runtime.ts`): how a process is started, probed, reached, stopped,
   * and swept. ProcessManager owns the lifecycle around those calls and never reads a
   * runtime-specific locator field itself. */
  runtime: Runtime;
  /** Daemon-host runner used only when the selected Runtime removes workspaces on tree close. */
  run?(
    command: string[],
    options?: CommandRunnerOptions
  ): Promise<{
    stdout: string;
    stderr?: string;
    exitCode: number;
    timedOut?: CommandResult["timedOut"];
    aborted?: CommandResult["aborted"];
  }>;
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

const ORPHAN_RECONCILIATION_GRACE_MS = 120_000;

/** Thrown by `spawnWorker`/`workerReady` when the tree is mid-teardown (tracked only in the
 * in-memory `closingTrees` map `closeTree` populates before awaiting any stop) — a route
 * handler translates this into an HTTP 409, distinct from any other error these methods can
 * throw. */
export { StopFailed, TreeClosingError } from "./process-errors";

/** Builds the addressing fragment every root and phase-worker process gets in its system prompt,
 * so the model can address the architect that owns its issue (and derive a sibling's topic)
 * without hand-encoding a `roleToken` itself — the encoding escapes `_`/`.`/`-` and a hand-built
 * token silently misses. `architectIssue` is the issue whose architect owns the launched process
 * (`owningArchitect`, LEGION-86): the child itself for a worker on a child with a claimed
 * sub-architect, the root otherwise, and the parent for a sub-architect; the root passes its own
 * key. */
export function addressingFragment(
  project: string,
  architectIssue: IssueKey,
  issue: IssueKey,
  role: LegionRole
): string {
  const ownTopic = roleTopic(roleToken(project, issue, role));
  const architectTopic = roleTopic(roleToken(project, architectIssue, "architect"));
  return (
    `Legion addressing: your role topic is \`${ownTopic}\`; the architect that owns your issue is ` +
    `\`${architectTopic}\`; a sibling role on your issue is your topic with the trailing ` +
    "`-<role>` replaced."
  );
}

/** The one sentence a root architect's system prompt carries after the addressing sentence:
 * whether this project arms the design gate (`config.gates.design`). The daemon's reply to
 * `/process/started` carries the same value, but the extension never shows it to the model, so
 * this is the only way the architect learns whether to request spec approval at all. Root
 * architects only — the root approval covers the tree and a child spec is never gated. */
export function designGateFragment(design: "root-issues" | "off"): string {
  return design === "off"
    ? "Design gate policy: `gates.design: off` — this project does not arm the design gate; do not request spec approval, register a gate, or wait for `design-approved`."
    : "Design gate policy: `gates.design: root-issues` — this project arms the root design gate; follow the legion-architect skill's approval sequence before any Legion-role spawn.";
}

/** The git `credential.helper` value written into every provisioned clone: this daemon's own
 * runtime and CLI entrypoint, so `git` inside a pane redeems the pane's grant through `legion
 * credential`. */
export function daemonCredentialHelper(
  execPath = process.execPath,
  entrypoint = DAEMON_CLI_ENTRYPOINT
): string {
  if (!path.isAbsolute(execPath) || !path.isAbsolute(entrypoint)) {
    throw new Error("Legion credential helper requires absolute runtime and CLI paths");
  }
  return `!${shellPath(execPath)} ${shellPath(entrypoint)} credential`;
}

/** The locators state currently records for `issue`, tree locator first (only when `issue` is a
 * tree root) then every worker claim's in `state.roles` order — the order a runtime that groups
 * an issue's processes together (the tmux runtime's shared window) reads them in. */
export function locatorsForIssue(state: LegionState, issue: IssueKey): Locator[] {
  const locators: Locator[] = [];
  const tree = state.trees[issue]?.locator;
  if (tree) locators.push(tree);
  for (const claim of Object.values(state.roles)) {
    if ("issue" in claim && claim.issue === issue && claim.locator) locators.push(claim.locator);
  }
  return locators;
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

/** Starts and supervises the trees and workers whose locators it records in Legion state,
 * through the injected `Runtime`. */
export class ProcessManager {
  private readonly runtime: Runtime;
  private readonly resurrecting = new Map<IssueKey, Promise<void>>();
  private readonly workerClients = new Map<string, WorkerRpcClient>();
  private readonly workerConnections = new Map<string, Promise<WorkerRpcClient>>();
  /** Tracks, per `<token>:<generation>`, whether a worker's socket close has already spent its
   * one reconnect attempt for that generation — `onWorkerClientClosed` consults this so a
   * reconnect's own close goes straight to `markWorkerDead` instead of chaining into another
   * reconnect attempt (no loop across successive closes). Never pruned: grows by one entry per
   * generation a worker's socket ever closes, for the process's lifetime. */
  private readonly reconnectAttempted = new Set<string>();
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
  /** Bounded per-tree wait for a freshly-spawned root to reach `/process/ready`, mirroring
   * `controllerRegistrationWait` above -- see `armRootRegistrationDeadline`'s doc comment for
   * the shared design. Keyed by tree, unlike the singleton controller wait, since multiple
   * trees can each have their own root registration wait in flight at once. Bound to the exact
   * generation armed for: a stale timer that fires after a newer spawn superseded it (a fresh
   * arm replaces the entry) or a confirmed `/process/ready` cancelled it (`confirmRootReady`)
   * is recognized as such by generation mismatch and never acts. */
  private readonly rootRegistrationWaits = new Map<
    IssueKey,
    { generation: number; cancel: () => void }
  >();
  /** Per-role-token idle-retire clock (`armIdleRetire`): armed each time the token's cached client
   * reports an idle transition, replacing any earlier clock for that token, and cleared by
   * `dispose()`. Bound to the exact arm by object identity, mirroring `rootRegistrationWaits`'s
   * generation check: `createCancellableSleep.cancel()` resolves the sleep it cancels, so a
   * superseded or disposed clock still fires — its expiry sees the map no longer holds this entry
   * and does nothing. Under a test-injected `sleep` (no real timer, `cancel` a no-op) the same check
   * is what neutralizes a stale fire. */
  private readonly idleRetireWaits = new Map<string, { cancel: () => void }>();
  /** The bound on `handleException`'s re-send of a failed role-lane message to an alive root
   * architect (`resend-ledger.ts`): at most `MAX_RESENDS` per message, each after its pause.
   * In memory only — a restart forgets every chain and also ends every one. */
  private readonly resendLedger = new ResendLedger(() => this.deps.now());
  /** The re-send pause in flight per chain key (`resendChainKey`), mirroring `idleRetireWaits`:
   * bound to the exact wait by object identity, so a wait `dispose()` cancelled (or a test's
   * injected `sleep` still fires) sees the map no longer holds this entry and sends nothing. */
  private readonly resendWaits = new Map<string, { cancel: () => void }>();
  /** Set once by `dispose()`, never cleared: a `retireUnconfirmedRoot` expiry already in flight
   * (blocked on its own `probe`/`stopProcessSerialized` await) has no map entry left for
   * `dispose()`'s own `cancelAllRootRegistrationDeadlines` to clear, since it never deletes its
   * entry until it is actually ready to act -- this flag is what stops that in-flight expiry
   * from retiring or resurrecting a root after shutdown has begun draining. */
  private disposed = false;
  private promotionSweep?: { attempted: Set<IssueKey>; inFlight: number };
  /** Boot's launch hold. False from construction: no path that opens a tmux pane (`admit`,
   * `advancePromotionSweep`, `resurrect`, `ensureController`, and — through
   * `WorkerAdmission`'s own gate — `spawnWorker`/`resumeWorker`) launches anything until
   * `enableLaunches()` runs, once the daemon's boot probes have passed. A spawn requested while
   * the hold is on queues through the ordinary admission queue and answers `queued`; a
   * resurrection or controller launch is remembered and replayed by `replayHeldRecoveries()`.
   * Paths that only stop or kill panes, or talk to a pane that survived the restart, are not
   * gated: the hold is about opening panes. */
  private launchesEnabled = false;
  private readonly heldResurrects = new Set<IssueKey>();
  private heldControllerRequest = false;
  /** `ensureController`'s one log line for a runtime that does not launch the controller. */
  private loggedControllerNotLaunched = false;
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
   * while one is already running awaits the same in-flight promise instead of racing it. The
   * promise resolves to whether the close released an admission slot -- the slot a mid-close
   * `admit` took -- which `closeTree` hands on to the promotion sweep only after this map has
   * forgotten the tree (see there). */
  private readonly closingTrees = new Map<IssueKey, Promise<boolean>>();
  /** In-memory only: roots the daemon is gracefully stopping to relaunch. The root's own
   * current-generation exit report is not a death while recovery owns the locator and slot. */
  private readonly stoppingForRelaunch = new Set<IssueKey>();
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
  /** `<state_dir>/secrets/dispatch-token`, written by `index.ts` at startup whenever
   * `config.dispatchToken` is set; exported to every process as `DISPATCH_TOKEN_FILE`. */
  private readonly dispatchTokenFile: string | undefined;
  /** Role tokens with a launch in flight, and how many (`holdProcessSecret`): a launch has taken the
   * hold but not yet stored its locator in state. `liveSecretFiles` treats them as live so a
   * persist racing the launch (another role's save, an older generation of the same root
   * settling) cannot reap a file the process is about to read. */
  private readonly launchingSecrets = new Map<string, number>();
  /** Names of every process secret file the runtime has written on this manager's behalf (added
   * immediately before each `runtime.spawn`), plus the survivors of the boot-time listing prune:
   * the population the steady-state prune in `persist()` walks — no directory listing per save. */
  private readonly processSecretFiles = new Set<string>();

  /**
   * The stop hierarchy every graceful-shutdown path funnels through, from lowest level up:
   * `stopProcess` (the one implementation: shim shutdown frame over the cached client, race
   * against a timeout, then `runtime.stop`) is wrapped by `stopProcessSerialized` (acquires the token's `WorkerAdmission`
   * critical section first, for a caller not already running inside it) or called raw by
   * `retireWorkerLocator` (for a caller — `launchWorker`, `markWorkerDeadLocked` — already running
   * inside that same section, where re-acquiring it would deadlock). `markWorkerDeadLocked` is
   * itself reached two ways: directly by `markWorkerDead` (a boot-time reconnect failure or a
   * runtime socket close), or via `WorkerAdmissionDeps.retireDeadClaim` →
   * `retirePromptFailedClaim` (the prompt-failure circuit breaker retiring a persistently-broken
   * but still-queued worker, relaunching it or — at `MAX_PROMPT_RETIRES` — ending the role in
   * `worker-died`).
   */
  constructor(private readonly deps: ProcessManagerDeps) {
    this.runtime = deps.runtime;
    this.dispatchTokenFile =
      deps.config.dispatchToken === undefined
        ? undefined
        : secretFilePath(deps.config.stateDir, DISPATCH_TOKEN_SECRET);
    this.workerAdmission = new WorkerAdmission({
      state: deps.state,
      config: deps.config,
      getWorkerClient: (token) => this.workerClients.get(token),
      persist: () => this.persist(),
      publishArchitect: (payload) => this.publishArchitect(payload),
      // Wrapped in trackLaunch so a promotion-triggered launch (drainWorkerQueue ->
      // promoteQueuedWorker -> here) is just as visible to closeTreeLocked's inFlightLaunches
      // fixed point as a direct spawnWorker launch is -- see launchWorker's own entry check for
      // the other half of this fence (closingTrees, re-checked before this call ever spawns a
      // process).
      launchWorker: (treeKey, issue, role, claim, pending) =>
        this.trackLaunch(treeKey, () => this.launchWorker(treeKey, issue, role, claim, pending)),
      promptExistingWorker: (client, token, issue, role, sessionId, pending) =>
        this.promptExistingWorker(client, token, issue, role, sessionId, pending),
      retireDeadClaim: (token, locator, verdict) =>
        this.retirePromptFailedClaim(token, locator, verdict),
      onAdmissionEvent: deps.onAdmissionEvent,
      rootForIssue: (issue) => this.rootForIssue(issue),
    });
    this.bootWatchdog = new WorkerBootWatchdog({
      workerBootTimeoutSeconds: () => this.deps.config.workerBootTimeoutSeconds,
      registrationDeadlineIntervals: () => this.deps.config.workerBootRegistrationDeadlineIntervals,
      now: () => this.deps.now(),
      probe: (locator) => this.runtime.probe(locator),
      connect: (token, locator) => this.clientFor(token, locator),
      workerRpcTimeoutMs: () => this.workerRpcTimeoutMs,
      sleep: this.deps.sleep,
      yield: this.deps.yield,
      getClaim: (token) => {
        const claim = this.deps.state.roles[token];
        return claim && "issue" in claim
          ? { generation: claim.generation, readyConfirmedAt: claim.readyConfirmedAt }
          : undefined;
      },
      retireUnconfirmedBoot: (token, locator, generation, retry) =>
        this.retireUnconfirmedBoot(token, locator, generation, retry),
    });
  }

  /** Releases boot's launch hold: the only way any pane-opening path (root admission and
   * promotion, worker launch and promotion, resurrection, the controller) is ever allowed to
   * actually open a pane. Call once, after the daemon's boot probes have passed and its HTTP
   * `api` is assigned, before `reconcileAdmission()`/`reconcileWorkerAdmission()`/
   * `replayHeldRecoveries()` -- see `launchesEnabled` and `WorkerAdmission`'s own doc comment. */
  enableLaunches(): void {
    this.launchesEnabled = true;
    this.workerAdmission.enableWorkerPromotion();
  }

  /** Replays every resurrection and controller launch that `resurrect`/`ensureController`
   * held while `launchesEnabled` was false. A held tree is replayed while it is `active` or
   * `dead` — `dead` is the state a resurrection exists to recover from (a root that survived the
   * restart and exited during the hold reports it through `POST /process/exit`, which sets it).
   * One that became `lingering`, `closed`, or `queued` meanwhile (parked by a human, closed, or
   * demoted by boot admission) is skipped: the request was made against a state that no longer
   * holds. Each replay's failure is logged and does not stop the others; call once, right after
   * `enableLaunches()` and the boot reconciles. */
  async replayHeldRecoveries(): Promise<void> {
    const trees = [...this.heldResurrects];
    this.heldResurrects.clear();
    for (const tree of trees) {
      const status = this.deps.state.trees[tree]?.status;
      if (status !== "active" && status !== "dead") {
        console.error(
          `[legion] held resurrection of ${tree} dropped: tree is ${status ?? "gone"}, not active`
        );
        continue;
      }
      try {
        await this.resurrect(tree);
      } catch (error) {
        console.error(`[legion] held resurrection of ${tree} failed:`, error);
      }
    }
    if (!this.heldControllerRequest) return;
    this.heldControllerRequest = false;
    try {
      await this.ensureController();
    } catch (error) {
      console.error("[legion] held controller launch failed:", error);
    }
  }

  /** Cancels the armed boot watchdog for `token`, if any — a no-op if none is armed, or if
   * `generation` is given and does not match the armed watchdog's (a stale caller from an
   * earlier attempt must never cancel a newer one's watch). */
  cancelBootWatchdog(token: string, generation?: number): void {
    this.bootWatchdog.cancel(token, generation);
  }

  /** Cancels every armed boot watchdog, idle-retire clock, re-send pause, and any pending
   * controller/root registration wait, and forgets the re-send ledger. Daemon shutdown calls
   * this once before drain and again after — an in-flight handler during drain (a launch's own
   * success path, `reconnectWorkers`) can still arm a watchdog after the first call, and this is
   * the only guaranteed-safe way to catch that: no background timer may outlive the
   * ProcessManager. Idempotent. */
  dispose(): void {
    this.disposed = true;
    this.bootWatchdog.cancelAll();
    this.cancelControllerRegistrationDeadline();
    this.cancelAllRootRegistrationDeadlines();
    this.cancelAllIdleRetireClocks();
    for (const { cancel } of this.resendWaits.values()) cancel();
    this.resendWaits.clear();
    this.resendLedger.clear();
  }

  admit(issue: IssueKey): "spawned" | "queued" {
    const tree = this.ensureTree(issue);
    const admission = this.deps.state.admission;
    admission.cap = this.deps.config.admissionCap;
    if (admission.active.includes(issue)) {
      if (tree.status === "dead") {
        void this.resurrect(issue).catch((error) => {
          console.error(
            `[legion] failed to resume ${issue} from its reserved admission slot:`,
            error
          );
        });
      }
      return "spawned";
    }

    const queuedIndex = admission.queue.indexOf(issue);
    if (queuedIndex !== -1 || tree.status === "launch-failed") {
      if (queuedIndex !== -1) admission.queue.splice(queuedIndex, 1);
      if (tree.status === "launch-failed") tree.launchFailures = 0;
      tree.status = "queued";
      admission.queue.push(issue);
      void this.beginPromotionSweep();
      return "queued";
    }

    // While boot's launch hold is on, the cap is treated as full: the tree queues exactly as it
    // would behind a full cap, and `reconcileAdmission()` promotes it once the hold releases.
    if (admission.active.length >= admission.cap || !this.launchesEnabled) {
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

  /** LEGION-57 boot repair, idempotent: a pre-LEGION-57 daemon admitted every child released to
   * `todo` as a root tree of its own. Removes each child tree that holds no process -- one of
   * three shapes: `queued`; `launch-failed`; or `active` with no recorded locator (a promotion
   * persisted before `spawnRoot` ever recorded a locator, so the spawn never completed before the
   * daemon stopped) -- whose issue has a live ancestor tree (`liveAncestorTree`, the reducer's own
   * ownership predicate) from `trees`, `admission.queue`, and `admission.active`, and with it the
   * child's stale root-architect claim (`roles[roleToken(child, "architect")]`, revoked through
   * `revokeRoleClaim` exactly as `closeTree` does): a `launch-failed` or active-without-locator
   * tree has had `/process/started` write that claim with a `sessionId` and nothing but
   * `closeTree` ever deletes it, so left in place its `sessionId` would become the
   * `expectedSessionId` of the parent's first sub-architect spawn, whose fresh session would 409
   * at `/worker/started` and burn a boot-timeout cycle before the retry launched clean -- and the
   * architect skill's "no architect claim" gate would see it and skip the spawn. The claim holds
   * no locator (a root architect's never does), so there is nothing to stop. Never the child's
   * Dispatch status, whatever it is; one log line each. Returns the (child, parent) pairs so
   * `index.ts` can wake each parent's architect with the reducer's own `child-adopted` payload once
   * boot admission has settled. Left alone: a `dead` child tree (a root mid-resurrection) and an
   * `active` one with a locator (a live root architect holding capabilities and workers); each
   * lingers and releases its slot when its issue closes (`reduceIssueClosed`). Runs before
   * `enableLaunches()` with the launch hold on, and so before `reconcileAdmission` -- which would
   * otherwise promote a queued child, or demote an active-no-locator one to queued and relaunch it
   * as a root in the same boot -- and a `launch-failed` child left in place would refuse the
   * parent's `spawn_worker` (`rootForIssue` resolves to the child itself) forever now that the
   * controller's `todo` no longer re-admits it.
   *
   * One exposure, logged rather than repaired: a `launch-failed` tree is reached from `dead` after
   * `MAX_LAUNCH_FAILURES` resurrections, and neither that path nor `recordRootExit` stops the phase
   * workers a previously *confirmed* generation of that root spawned. Such a worker keeps its
   * locator and `LEGION_TREE=<child>`; once the tree is gone here, its tree-scoped credential
   * routes (`/grants`, `/git-credential`, `/gh-token`) answer 404 `Unknown tree`, and the new
   * sub-architect's `spawn_worker` for that role finds the live claim and resumes it rather than
   * replacing it. The removal still happens (the spec's Errors row asks for it, and the parent's
   * `spawn_worker` is otherwise refused forever); the surviving claims are named in one log line
   * so the operator can retire them by hand. Synchronous, no persist of its own:
   * `reconcileAdmission`'s closing `persist()` saves the result. */
  adoptOwnerlessChildTrees(): ChildAdoption[] {
    const state = this.deps.state;
    const adoptions: ChildAdoption[] = [];
    for (const [key, tree] of Object.entries(state.trees)) {
      const holdsNoProcess =
        tree.status === "queued" ||
        tree.status === "launch-failed" ||
        (tree.status === "active" && tree.locator === undefined);
      if (!holdsNoProcess) continue;
      const parent = state.issues[key]?.parent;
      if (!parent) continue;
      const owner = liveAncestorTree(state, key);
      if (!owner) continue;
      // Resolved while the tree still exists: `rootForIssue` reaches `key` for the child and its
      // descendants only through this tree.
      const survivingWorkers = Object.entries(state.roles)
        .filter(
          ([, claim]) =>
            "issue" in claim &&
            claim.locator !== undefined &&
            this.rootForIssue(claim.issue) === key
        )
        .map(([token]) => token);
      delete state.trees[key];
      const queuedIndex = state.admission.queue.indexOf(key);
      if (queuedIndex !== -1) state.admission.queue.splice(queuedIndex, 1);
      const activeIndex = state.admission.active.indexOf(key);
      if (activeIndex !== -1) state.admission.active.splice(activeIndex, 1);
      const architectToken = roleToken(state.project, key, "architect");
      const architectClaim = state.roles[architectToken];
      if (architectClaim && "issue" in architectClaim) {
        this.revokeRoleClaim(architectClaim);
        delete state.roles[architectToken];
      }
      console.error(
        `[legion] removed the ${tree.status} root tree for ${key} at boot: it is a child of ${parent} (${owner.root} is ${owner.status}) and is owned by that tree's architect, never admitted as a root (LEGION-57); its Dispatch status is left as is`
      );
      if (survivingWorkers.length > 0) {
        console.error(
          `[legion] ${key}'s removed root tree still has worker claims with recorded panes (${survivingWorkers.join(", ")}): spawned by an earlier confirmed generation of that root, they carry LEGION_TREE=${key}, so their tree-scoped credential routes answer 404 until they are retired; ${parent}'s sub-architect for ${key} would resume, not replace, them`
        );
      }
      adoptions.push({ child: key, parent });
    }
    return adoptions;
  }

  /**
   * Converges stored admission with the configured cap, promoting queued trees
   * until active slots fill or no eligible queued tree remains. Runs at boot
   * because a cap raised between restarts opens slots no release event fills.
   */
  async reconcileAdmission(): Promise<void> {
    // Boot never trusts a process it did not record: reap every
    // runtime-owned process no tree, claim, or the controller currently
    // names, with no grace period, before the demote/promote below decide
    // what to run. An orphan-active-no-locator tree demoted below (its
    // prior process, if one exists, was never recorded in its locator) is
    // caught by this same criterion, so it is reaped here rather than left
    // running alongside the fresh spawn the promotion loop later gives the
    // requeued tree.
    await this.reconcileOrphans(0);

    const admission = this.deps.state.admission;
    admission.cap = this.deps.config.admissionCap;
    // Drop every waiting entry whose issue has already left the line -- a `done` issue queued by
    // a daemon that predates the `dequeue` effect (LEGION-56), or one whose status event this
    // daemon missed -- before anything below is promoted: a stale head of the queue must never
    // spawn a root for an issue with nothing to do, and the valid entry behind it must be the one
    // promoted. The same rule as the resync sweep (`staleQueueEntryReason`, legion-state.ts);
    // `reconcileAdmission`'s closing persist saves the result.
    for (const issue of [...admission.queue]) {
      const reason = staleQueueEntryReason(this.deps.state, issue);
      if (reason === undefined) continue;
      this.removeQueued(issue);
      console.error(`[legion] dropped ${issue} from the admission queue at boot: ${reason}`);
    }
    // An "active" tree with no recorded locator never finished spawning before the daemon last
    // stopped: advancePromotionSweep persists the promotion before startRoot/spawnRoot ever records
    // a locator, so a crash in that exact window leaves this on disk. Demote it back to queued so
    // the promotion loop below re-spawns it, instead of leaving it silently consuming a slot with
    // nothing running forever. Judged over every tree, not only the entries `admission.active`
    // holds: an active-no-locator tree that also dropped out of `admission.active` is requeued
    // here, never re-admitted by the drift repair below as a slot with nothing running.
    for (const tree of Object.values(this.deps.state.trees)) {
      if (tree.status !== "active" || tree.locator) continue;
      const activeIndex = admission.active.indexOf(tree.root);
      if (activeIndex !== -1) admission.active.splice(activeIndex, 1);
      tree.status = "queued";
      if (!admission.queue.includes(tree.root)) admission.queue.push(tree.root);
      console.error(
        `[legion] demoted ${tree.root} from active to queued at boot: no recorded locator (a prior spawn never completed before the daemon stopped)`
      );
    }

    // LEGION-83: every active tree holds a slot, every slot names an active (or mid-resurrection
    // dead) tree, and occupancy above the cap is visible -- repaired here and again by every
    // resync (`runResync`), never by stopping a root. Runs before the promotion loop so an
    // outsider takes its slot back before a queued issue could be promoted into it.
    const repair = repairAdmissionDrift(this.deps.state);
    for (const line of describeAdmissionDrift(repair, "at boot")) console.error(line);
    await this.promoteQueuedRoots();
    await this.persist();
  }

  /** Repairs root admission through the process owner. A dead root with a reserved slot but no
   * in-flight recovery is durable evidence of an interrupted resurrection, so it resumes before
   * ordinary drift reconciliation. */
  async reconcileAdmissionDrift(): Promise<AdmissionDriftRepair> {
    for (const tree of Object.values(this.deps.state.trees)) {
      if (
        tree.status !== "dead" ||
        !this.deps.state.admission.active.includes(tree.root) ||
        this.resurrecting.has(tree.root)
      ) {
        continue;
      }
      try {
        await this.resurrect(tree.root);
      } catch (error) {
        console.error(`[legion] failed to resume persisted recovery for ${tree.root}:`, error);
      }
    }
    const repair = repairAdmissionDrift(this.deps.state);
    for (const line of describeAdmissionDrift(repair, "by resync")) console.error(line);
    if (repair.removed.length > 0) await this.promoteQueuedRoots();
    if (repair.added.length > 0 || repair.removed.length > 0) await this.persist();
    return repair;
  }

  /** Starts eligible queued roots until admission reaches its cap or the queue makes no progress.
   * Both boot reconciliation and resync admission repair use this one owner of promotion. */
  private async promoteQueuedRoots(): Promise<void> {
    const admission = this.deps.state.admission;
    let queued = admission.queue.length;
    while (admission.active.length < admission.cap && queued > 0) {
      await this.beginPromotionSweep();
      // No progress means every remaining queued issue is ineligible.
      if (admission.queue.length === queued) break;
      queued = admission.queue.length;
    }
  }

  /** Removes `issue`'s `admission.queue` entry and, when its tree record is `queued`, that record
   * -- the whole footprint of a root waiting for a slot, so a later `todo` admits it again exactly
   * like a never-seen issue. Any other tree status (active, lingering, dead, launch-failed,
   * closed) is left in place: the linger, close, and launch-failure paths own those records.
   * Returns whether anything changed. Mutation only; the caller persists. */
  private removeQueued(issue: IssueKey): boolean {
    const state = this.deps.state;
    const queuedIndex = state.admission.queue.indexOf(issue);
    if (queuedIndex !== -1) state.admission.queue.splice(queuedIndex, 1);
    const queuedTree = state.trees[issue]?.status === "queued";
    if (queuedTree) delete state.trees[issue];
    return queuedIndex !== -1 || queuedTree;
  }

  /** The `dequeue` effect's executor (`events.ts`'s `onDequeue`): a waiting issue that Dispatch
   * moved to `done`, `backlog`, `icebox`, or `triage` leaves the queue and loses its `queued` tree
   * record, persisted inside the durable lane's dispatch-before-save transaction -- so the queue
   * change lands in the same durable step as the status change, and a persist failure propagates
   * and goes fatal like every other effect's. A silent no-op when the issue holds neither (a
   * redelivered event finds nothing to do). */
  async dequeue(issue: IssueKey): Promise<void> {
    if (!this.removeQueued(issue)) return;
    await this.persist();
  }

  async releaseSlot(issue: IssueKey): Promise<void> {
    const admission = this.deps.state.admission;
    const activeIndex = admission.active.indexOf(issue);
    if (activeIndex === -1) return;

    admission.active.splice(activeIndex, 1);
    await this.beginPromotionSweep();
  }

  /** Delivers an architect's `spawn_worker` task to `role` on `issue`. The architect's task is
   * the only source of an `assignment` -- the one kind of pending prompt whose delivery makes
   * its role the issue's active phase (`state.phases[issue]`, written by `promptExistingWorker`);
   * the daemon's own recovery prompt goes through `deliverToWorker` as a `catchup` instead. */
  async spawnWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    task: string
  ): Promise<SpawnWorkerResponse> {
    return this.deliverToWorker(treeKey, issue, role, {
      kind: "assignment",
      task,
      queuedAt: new Date(this.deps.now()).toISOString(),
    });
  }

  /** Resumes, prompts, launches, or queues the worker for `role` so that `pending` reaches it:
   * prompted straight into a live idle worker, queued on a booting or at-cap claim for
   * `/worker/ready` or queue promotion to deliver, or launched fresh (`--resume` when the role
   * has a recorded session). A `catchup` never displaces a queued `assignment`: the check runs
   * inside the role's lock because `resumeWorker` computes its catch-up outside it (a GitHub
   * fetch), and an architect's `spawn_worker` can land in that window. */
  private async deliverToWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    pending: PendingAssignment
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
        // rechecks stop the decision itself from spawning a fresh process after the tree has
        // already started tearing down.
        if (this.closingTrees.has(treeKey)) {
          throw new TreeClosingError(treeKey);
        }
        const existing = this.deps.state.roles[token];
        const claim = existing && "issue" in existing ? existing : undefined;
        if (pending.kind === "catchup" && claim?.pendingAssignment?.kind === "assignment") {
          console.info(
            `[legion] dropping catch-up for ${token}: the architect's assignment is queued and reaches the worker first`
          );
          return { status: "resumed", roleToken: token };
        }

        if (claim?.locator) {
          if (!claim.sessionId || claim.readyConfirmedAt === undefined) {
            // Booting or started-but-unconfirmed: launchWorker spawned the process, but either
            // /worker/started has not yet registered this generation's session, or it has and
            // /worker/ready has not yet durably confirmed the boot. Never launch a second
            // process or probe/prompt the socket while a boot's readiness is still unconfirmed —
            // queue the pending prompt and let /worker/ready deliver it once the worker's boot
            // is confirmed. An identical re-send keeps the first queuedAt and saves nothing.
            if (!samePendingTask(claim.pendingAssignment, pending)) {
              claim.pendingAssignment = pending;
              await this.persist();
            }
            return { status: "resumed", roleToken: token };
          }
          const locator = claim.locator;
          const probe = await probeWorker(
            () => this.clientFor(token, locator),
            this.workerRpcTimeoutMs
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
              issue,
              role,
              claim,
              claim.sessionId,
              pending,
              probe.client
            );
            if (decision.kind === "queued") {
              return { status: "queued", roleToken: token };
            }
            return { status: "resumed", roleToken: token };
          }
          // May throw StopFailed (a real runtime stop failure, not the routine dead-socket
          // case): never launch a replacement over a process that might still be alive.
          await this.retireWorkerLocator(token, claim.locator);
          if (this.closingTrees.has(treeKey)) {
            throw new TreeClosingError(treeKey);
          }
          return this.workerAdmission.launchOrQueue(token, treeKey, issue, role, claim, pending);
        }

        return this.workerAdmission.launchOrQueue(token, treeKey, issue, role, claim, pending);
      })
    );
  }

  /** Registers `fn`'s own promise in `inFlightLaunches` for `treeKey` for its whole duration,
   * removed on settle either way -- see that field's comment for why `closeTreeLocked`'s
   * fixed-point loop needs to await this set before every re-snapshot. Wraps the ENTIRE
   * post-fence spawn decision, not merely the spawn step inside it: a stale-claim
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

  /** Prompts an already-connected, already-live worker client with `pending.task` and commits the
   * delivery only once the worker's turn is observed to start — the shared implementation behind
   * every "resume an existing worker" path (`WorkerAdmission`'s `resumeOrQueueExisting` admitted
   * branch, its `"prompt"` queue-promotion decision, and `/worker/ready`), as opposed to
   * `launchWorker`, which spawns a fresh process. Delivery means a started turn, never the shim's
   * `{success:true}` acknowledgement: OMP answers that before the turn begins and can accept a
   * message that starts none (see `PromptReceipt`), which is how LEGION-10's queued task vanished
   * with the phase written and the worker idle. After `client.prompt()` resolves, the receipt's
   * `turnStarted` is raced against `workerRpcTimeoutMs` on the injectable clock
   * (`awaitTurnStart`); a started turn runs `commitPromptDelivery`; no turn within the bound
   * throws `PromptNotStarted` having committed nothing — every caller treats it exactly like a
   * refused prompt (count, retry on the next drain, retire at the threshold) — and leaves a
   * continuation on the same receipt (`commitLateStart`) so a turn that starts after the bound is
   * committed as this same delivery and the task is never prompted a second time. A refused
   * prompt (`client.prompt()` itself rejecting) propagates exactly as before, likewise having
   * committed nothing. Does not touch the running-worker admission count itself — the caller
   * owns reserving and releasing that slot (mirroring `launchWorker`, which is likewise unaware
   * of admission bookkeeping) since only the caller knows whether this prompt represents a new
   * admission (`resumeOrQueueExisting`'s below-cap idle-resume, or a queue promotion) or none at
   * all (`/worker/ready` resuming a worker whose slot was already counted via its locator from
   * the moment `launchWorker` wrote it, so nothing here needs releasing or re-checking).
   * For an `assignment`, the Runtime first adopts the issue's working copy for the role, before
   * the prompt frame and any state write. A rejected adoption leaves the worker unprompted and the
   * claim untouched, propagating like a refused prompt. */
  private async promptExistingWorker(
    client: WorkerRpcClient,
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    pending: PendingAssignment,
    afterPrompt?: () => void
  ): Promise<void> {
    if (pending.kind === "assignment") {
      await this.runtime.adoptWorkingCopy(
        issue,
        role,
        await this.workerJjIdentity(role),
        this.deps.config.slowCommandTimeoutSeconds * 1000
      );
    }
    const receipt = await client.prompt(pending.task);
    const outcome = await this.awaitTurnStart(client, receipt);
    if (!outcome.started) {
      // A closed socket's receipt can never settle — nothing more arrives on that connection —
      // so the late-start continuation is left only for a socket that is still up.
      if (outcome.reason === "no-turn") {
        receipt.turnStarted
          .then(() => this.commitLateStart(token, issue, role, sessionId, pending))
          .catch((error) => {
            console.error(`[legion] late-start commit for ${token} failed:`, error);
          });
      }
      throw new PromptNotStarted(
        token,
        this.workerRpcTimeoutMs,
        outcome.reason,
        outcome.observation
      );
    }
    await this.commitPromptDelivery(token, issue, role, sessionId, pending, afterPrompt);
  }

  /** Waits, bounded by `workerRpcTimeoutMs` on the injectable clock, for the turn `receipt`
   * describes to start. A start already observed (an `agent_start` that preceded the
   * acknowledgement) is answered without building a timer. Otherwise the receipt is raced against
   * the socket closing and the bound. A closed socket is answered as `socket-closed` — a dead
   * worker, not a slow one; no `get_state` is attempted, and `onWorkerClientClosed` owns what
   * happens to the claim. At the bound the receipt is abandoned FIRST — a silent restore of the
   * client's pre-prompt `runState`, so the answer that follows is never an idle *transition* on a
   * worker that was idle before the prompt (a transition-fired restore would re-enter promotion
   * synchronously mid-failure; on the ready path the fresh client sits at `"unknown"`, so there
   * the confirming answer IS its first idle transition and fires `promoteWorkerQueue` — safe
   * because that drain peeks the queue head before `queueUnstartedPrompt` has enqueued this
   * token) — THEN the spec's second signal is asked for once: a `get_state` answer reporting a
   * stream in progress is a started turn (the client settles the receipt from it). The receipt
   * is consulted again however that call ended — an `agent_start` that lands while `get_state`
   * is in flight or failing is a started turn — and a socket that closed meanwhile is
   * `socket-closed` whatever `get_state` said; only then is `isStreaming: false` or a failed call
   * `no-turn`. That `get_state` is also, for `/worker/ready` (whose fresh client sits at
   * `"unknown"` — `clientFor` never asks `get_state`), the one thing that seeds the client idle so
   * the queued retry is promotable at all. */
  private async awaitTurnStart(
    client: WorkerRpcClient,
    receipt: PromptReceipt
  ): Promise<
    { started: true } | { started: false; reason: PromptNotStartedReason; observation: string }
  > {
    if (receipt.hasStarted) return { started: true };
    // The close handler settles `client.closed` synchronously when the socket goes; this flag is
    // set one microtask later, well before a `get_state` rejection that same close caused can
    // propagate back here (it crosses the request's own `finally`/`then` chain first).
    let socketClosed = false;
    const closed = client.closed.then(
      () => {
        socketClosed = true;
      },
      () => {
        socketClosed = true;
      }
    );
    const { timedOut, cancel } = boundedWait(this.workerRpcTimeoutMs, this.deps.sleep);
    const outcome = await Promise.race([
      receipt.turnStarted.then(() => "started" as const),
      closed.then(() => "closed" as const),
      timedOut.then(() => "timeout" as const),
    ]);
    cancel();
    if (outcome === "started") return { started: true };
    if (outcome === "closed") {
      return { started: false, reason: "socket-closed", observation: "socket closed" };
    }
    receipt.abandonWait();
    let stateFailure: string | undefined;
    try {
      await client.getState(this.workerRpcTimeoutMs);
    } catch (error) {
      stateFailure = error instanceof Error ? error.message : String(error);
    }
    if (receipt.hasStarted) return { started: true };
    if (socketClosed) {
      return {
        started: false,
        reason: "socket-closed",
        observation:
          stateFailure === undefined
            ? "socket closed"
            : `socket closed (get_state failed: ${stateFailure})`,
      };
    }
    if (stateFailure !== undefined) {
      return {
        started: false,
        reason: "no-turn",
        observation: `get_state failed: ${stateFailure}`,
      };
    }
    return { started: false, reason: "no-turn", observation: "get_state: isStreaming=false" };
  }

  /** The one commit block a delivered prompt runs — shared by the in-bound path and the late
   * start (`commitLateStart`), so the two can never drift. This is the one place a new active
   * phase is written (`state.phases[issue]`, which `phase/complete` and `routeActive` read;
   * `phase/complete` itself only deletes, restores, or marks that record completed), and it is
   * written only for an architect `assignment` -- stamped with `assignedAt`, the ISO time of this
   * delivery, so the completion route's refusal log can say when the record it refused against
   * was created -- a `catchup` prompt is recovery plumbing and leaves the phase exactly as it
   * was, so a relaunched worker whose phase already finished never becomes the active phase again. Clears the claim's `pendingAssignment`, resets
   * `promptFailures` and deletes `promptRetires` (a started turn confirms this worker is
   * responsive again — neither the prompt count nor the relaunch-cycle count from a prior failure
   * may carry into a future one; this is the only place `promptRetires` is ever cleared), runs the
   * caller's `afterPrompt`, removes the token's queue entry (a no-op when it was never queued), and
   * persists. A `persist` failure here is a durable-state persistence issue, not a prompt
   * failure: the worker is already working, mirroring `launchWorker`'s post-locator-write save
   * handling. Retries the persist once; if that also fails, logs it and returns normally — never
   * rethrown, since the caller (and, transitively, the architect) would otherwise see a failure
   * for a worker that is actually already running the task. */
  private async commitPromptDelivery(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    pending: PendingAssignment,
    afterPrompt?: () => void
  ): Promise<void> {
    if (pending.kind === "assignment") {
      this.deps.state.phases[issue] = {
        phase: role,
        sessionId,
        assignedAt: new Date(this.deps.now()).toISOString(),
      };
    }
    const claim = this.deps.state.roles[token];
    if (claim && "issue" in claim) {
      delete claim.pendingAssignment;
      claim.promptFailures = 0;
      delete claim.promptRetires;
    }
    afterPrompt?.();
    await this.workerAdmission.removeFromQueue(token);
    try {
      await this.persist();
    } catch (persistError) {
      console.error(
        `[legion] failed to persist ${token}'s phase/pendingAssignment after its turn started (worker is already working regardless):`,
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

  /** The continuation `promptExistingWorker` leaves on a receipt whose bound expired: the turn
   * started after all, so this is the same delivery, committed now — never a second prompt.
   * Inside the token's critical section the claim is re-read and the commit runs only while it
   * still describes the prompt this receipt belongs to: the same session, a locator still
   * recorded, the same task still pending (compared by value — `kind` and `task` — since identity
   * is not reliable across the three prompt sites and a re-sent identical task is the same
   * task), and a tree that is not gone. Anything else (the task replaced or already delivered by
   * a retry, the worker retired, the tree closed) commits nothing, silently: the newer task stays
   * queued and is delivered once the worker is idle again. A commit publishes `worker-started`
   * (the architect was told `worker-queued` when the retry was queued) and, after the lock,
   * re-checks the queue — the head moved, so the tokens behind it get their turn. */
  private async commitLateStart(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    pending: PendingAssignment
  ): Promise<void> {
    const committed = await this.workerAdmission.mutateClaim(token, async () => {
      const claim = this.deps.state.roles[token];
      const treeKey = this.rootForIssue(issue);
      if (
        !claim ||
        !("issue" in claim) ||
        claim.sessionId !== sessionId ||
        claim.locator === undefined ||
        !samePendingTask(claim.pendingAssignment, pending) ||
        treeKey === undefined ||
        this.isTreeGone(treeKey, issue)
      ) {
        return false;
      }
      console.info(
        `[legion] ${token} started its turn after the prompt wait expired; delivering the queued task now`
      );
      await this.commitPromptDelivery(token, issue, role, sessionId, pending);
      this.publishArchitect({ type: "worker-started", issue, role });
      return true;
    });
    if (committed) this.workerAdmission.promoteWorkerQueue();
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

  /** The topic of the architect that owns `issue` for a wake about `role`'s lifecycle. */
  private owningArchitectTopic(issue: IssueKey, role: LegionRole): string {
    return roleTopic(
      roleToken(this.deps.state.project, owningArchitect(this.deps.state, issue, role), "architect")
    );
  }

  private publishArchitect(
    payload:
      | { type: "worker-queued"; issue: IssueKey; role: LegionRole }
      | { type: "worker-started"; issue: IssueKey; role: LegionRole }
      | { type: "worker-died"; issue: IssueKey; role: LegionRole }
      | { type: "launch-failed"; issue: IssueKey; role: LegionRole; failures: number }
  ): void {
    this.deps.publishRole(
      this.owningArchitectTopic(payload.issue, payload.role),
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
    let retiredBystander = false;
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
      if (!claim.locator) return;
      if (claim.readyConfirmedAt !== undefined) {
        this.cancelBootWatchdog(token, generation);
        return;
      }
      const pending = claim.pendingAssignment;
      if (isBystanderCatchup(this.deps.state, issue, role, pending)) {
        // The phase moved on while this boot was in flight: the catch-up queued for it is a
        // bystander's now, and this relaunched pane has nothing else to do. It cannot simply be
        // left alive with the prompt dropped: a fresh `omp --mode rpc` that is never prompted
        // emits no `agent_end`, and `clientFor` never asks `get_state`, so its client would sit at
        // `runState: "unknown"` -- counted against `workerCap` by `runningWorkerCount` and never
        // reached by `armIdleRetire` (which fires only from an idle transition) -- until some
        // unrelated probe happened to seed it. So it is retired right here, exactly as
        // `retireIdleWorker` retires a finished worker: graceful shutdown frame (kill-pane
        // fallback), locator cleared, `ompSessionFile` carried into `resumeSessionFile` so the
        // architect's next spawn_worker resumes the same agent with `--resume`, `launchFailures`
        // reset because the boot itself did succeed. The slot is free the moment this persists;
        // the drain that hands it to whatever is queued runs after this critical section (below).
        console.info(
          `[legion] dropping queued catch-up for ${token} at ready and retiring the relaunched pane: ${issue}'s active phase is ${activePhaseLabel(this.deps.state, issue)}; only spawn_worker resumes a finished worker`
        );
        // Confirmed in memory BEFORE the stop, never after it: `retireWorkerLocator` may throw
        // `StopFailed` (a real kill-pane failure on a pane that may still be alive), and it has
        // already cancelled this token's boot watchdog by then. Confirmed first, that failure
        // leaves exactly `retireIdleWorker`'s own StopFailed shape -- locator intact, boot
        // confirmed, catch-up gone -- which the next `spawn_worker` recovers through the live-claim
        // probe path. Left unconfirmed, the claim would look like a boot still in flight with no
        // watchdog left to judge it, and every later `spawn_worker` for the role would queue its
        // task on the booting branch for a `/worker/ready` that never comes again. A restart
        // re-derives "unconfirmed" from disk (nothing is persisted until after the stop) and
        // `reconnectWorkers` re-arms the watchdog, which recovers that case too.
        delete claim.pendingAssignment;
        claim.readyConfirmedAt = this.deps.now();
        delete claim.launchFailures;
        const locator = claim.locator;
        await this.retireWorkerLocator(token, locator);
        const resumeSessionFile = locator.ompSessionFile ?? claim.resumeSessionFile;
        delete claim.locator;
        if (resumeSessionFile) claim.resumeSessionFile = resumeSessionFile;
        await this.persist();
        // `retireWorkerLocator` already cancelled the watchdog for every generation of this token.
        retiredBystander = true;
        return;
      }
      const client = await this.clientFor(token, claim.locator);
      const confirmBoot = (): void => {
        claim.readyConfirmedAt = this.deps.now();
        // A durably confirmed boot is the one moment this counter resets -- never a mere
        // `/worker/started` registration, which a worker that keeps registering but never
        // reaching this point could otherwise reset every generation, masking a persistent
        // post-registration failure from ever escalating to `worker-died` (see
        // `retireUnconfirmedBoot`'s own increment).
        delete claim.launchFailures;
      };
      if (pending) {
        try {
          await this.promptExistingWorker(
            client,
            token,
            issue,
            role,
            sessionId,
            pending,
            confirmBoot
          );
        } catch (error) {
          if (!(error instanceof PromptNotStarted) || root === undefined) throw error;
          if (error.reason === "socket-closed") {
            // The boot did NOT succeed: the shim's socket closed before any turn began, so this
            // is a worker that died right after saying "got it", not one that swallowed the
            // prompt. Nothing is confirmed, cancelled, or queued here. The claim is left exactly
            // as the socket-close handler expects an unconfirmed boot — locator recorded,
            // `pendingAssignment` on the claim, watchdog armed — and that handler
            // (`onWorkerClientClosed` -> `retireUnconfirmedBoot`, queued behind this critical
            // section) is the one place that retires it: locator cleared and its slot released,
            // `launchFailures` counted, the task re-queued and relaunched cold with `--resume`
            // (`worker-started` to the architect), or `worker-died` at the threshold. Confirming
            // here instead would route that handler to `retireUnconfirmedBoot`'s confirmed-claim
            // early return and strand a dead locator holding a `worker_cap` slot with the task
            // still on it. Should the handler's one reconnect succeed instead (the shim was only
            // restarting), the claim stays an unconfirmed boot under its watchdog — the same
            // shape a prompt the close made *reject* has always left.
            console.error(
              `[legion] ${token}: ${error.message}; boot left unconfirmed for the socket-close handler to retire`
            );
            return;
          }
          // The boot DID succeed -- registered, ready, socket answering -- so it is confirmed
          // exactly as a delivered prompt would have confirmed it. Left unconfirmed, the claim
          // would be stranded: `promoteQueuedWorker` stops on `readyConfirmedAt === undefined`
          // and every later `spawn_worker` would queue behind a boot that never confirms again.
          // `queueUnstartedPrompt` persists, so the confirmation lands in the same save. The
          // task stays on the claim and the role joins the promotion queue, so the same retry,
          // count, and retire accounting the queued idle-resume path has applies here; the retry
          // is the next drain (an idle/dead event, the 60 s sweep, or the late start), never an
          // immediate re-prompt of a worker that may merely be slow to start its turn. The
          // confirming `get_state` inside `awaitTurnStart` has already seeded this fresh client
          // from `"unknown"` to `"idle"`, so that drain finds it promotable.
          confirmBoot();
          this.cancelBootWatchdog(token, generation);
          console.error(
            `[legion] ${token}: ${error.message}; boot confirmed, task queued for promotion`
          );
          await this.workerAdmission.queueUnstartedPrompt(
            token,
            issue,
            role,
            claim,
            pending,
            error.reason
          );
          return;
        }
      } else {
        confirmBoot();
        await this.persist();
      }
      this.cancelBootWatchdog(token, generation);
    });
    // A pane retired above frees a `workerCap` slot in state but nothing else drains the queue
    // for it: `retireIdleWorker`'s retirement gets its drain from the retired client's socket
    // close (`onWorkerClientClosed` -> `markWorkerDead` -> `promoteWorkerQueue`), but this pane
    // was never connected through `clientFor`, so no cached client exists to close. Without this
    // trigger a spawn already queued behind the cap would wait for the next linger sweep's
    // `reconcileWorkerAdmission`. Outside the critical section, exactly as `markWorkerDead` does.
    if (retiredBystander) this.workerAdmission.promoteWorkerQueue();
  }

  /** The inner logic behind a worker's socket being confirmed dead -- a boot-time reconnect
   * probe failed, or a live connection's own `client.closed` handler tried and failed its one
   * reconnect attempt, or the prompt-failure circuit breaker retired a persistently-broken
   * worker (`retirePromptFailedClaim`, behind `WorkerAdmissionDeps.retireDeadClaim`) -- so its
   * locator is retired and cleared:
   * retires whatever process it may still be running, then clears the locator -- moving its
   * `ompSessionFile` to `resumeSessionFile` so the eventual respawn/promotion still resumes the
   * same agent -- leaving `pendingAssignment` so it still delivers the queued task. Assumes the
   * caller already holds this token's `roleLaunchQueue` critical section -- the same one
   * `launchWorker`/`promoteQueuedWorker` use -- and re-checks that the claim's current locator
   * still matches the one it was handed before touching anything: by the time this call gets
   * its turn, a newer launch for the same token may already have replaced it (or the claim may
   * be gone entirely, e.g. `closeTree`), and this must never delete a newer launch's locator or
   * retire a process that isn't the one it was told to. */
  private async markWorkerDeadLocked(token: string, locator: Locator): Promise<void> {
    const current = this.deps.state.roles[token];
    if (!current || !("issue" in current) || !sameProcess(current.locator, locator)) {
      return;
    }
    await this.retireWorkerLocator(token, locator);
    const resumeSessionFile = current.locator?.ompSessionFile ?? current.resumeSessionFile;
    delete current.locator;
    if (resumeSessionFile) current.resumeSessionFile = resumeSessionFile;
    await this.persist();
  }

  /** `WorkerAdmissionDeps.retireDeadClaim`: the prompt-failure circuit breaker's retirement, inside
   * the caller's role lock. `"relaunch"` is exactly `markWorkerDeadLocked` (graceful shutdown with
   * kill-pane fallback, locator cleared, `ompSessionFile` carried into `resumeSessionFile`,
   * `pendingAssignment` kept, persisted): the task stays queued and the next drain relaunches it
   * cold with `--resume`. `"died"` is the same retirement and then the terminal shape: the role's
   * promotion-queue entry removed (after the stop, so a `StopFailed` never strands a live pane with
   * no queue entry to retry it) and `worker-died {issue, role}` published once to the tree's
   * architect — unless the tree is gone or closing (`isTreeGone`, mirroring
   * `retireUnconfirmedBoot`'s guard: the close path owns that claim). `pendingAssignment` stays on
   * the claim, `promptRetires` stays at the bound, so the architect's next `spawn_worker` is one cold
   * launch, `MAX_LAUNCH_FAILURES` prompts, and `worker-died` again. Counters are the caller's. */
  private async retirePromptFailedClaim(
    token: string,
    locator: Locator,
    verdict: PromptRetireVerdict
  ): Promise<void> {
    const claim = this.deps.state.roles[token];
    const retry =
      claim && "issue" in claim ? this.deriveRetryContext(token, claim.issue) : undefined;
    await this.markWorkerDeadLocked(token, locator);
    if (verdict !== "died") return;
    await this.workerAdmission.removeFromQueue(token);
    if (retry && !this.isTreeGone(retry.treeKey, retry.issue)) {
      this.publishArchitect({ type: "worker-died", issue: retry.issue, role: retry.role });
    }
  }

  /** Acquires this token's `roleLaunchQueue` critical section (see `markWorkerDeadLocked` for
   * the actual logic) then re-checks the running-worker queue, since clearing the locator may
   * have freed the slot this worker was occupying. */
  private async markWorkerDead(token: string, locator: Locator): Promise<void> {
    await this.workerAdmission.mutateClaim(token, () => this.markWorkerDeadLocked(token, locator));
    this.workerAdmission.promoteWorkerQueue();
  }

  /** Reconnects to every live worker's shim socket after a daemon restart, probing liveness. A
   * connect failure against a ready-confirmed claim means the worker is confirmed dead; the same
   * failure against a boot whose ready path is still incomplete is routed through
   * `retireUnconfirmedBoot` instead, so it retries or gives up exactly like the boot watchdog
   * would rather than merely clearing the locator and stranding the claim with nothing left to
   * ever retry it. A connect that succeeds but whose follow-up `get_state` fails (times out,
   * say) means only that the shim is busy answering this one request in time — never a reason
   * to kill a live worker — so the claim is left exactly as is, with its conservative
   * "unknown"-counts-as-running `runState`. A boot that survives this reconnect without its
   * ready path confirmed gets its watchdog re-armed: the in-memory `WorkerBootWatchdog` registry
   * does not survive a restart, so without this it would never be probed again.
   *
   * Every claim is reconciled in isolation: a throw while handling one (a capability revoke
   * failing, a tmux call rejecting) is logged as `failed to reconcile worker <token>` and leaves
   * that claim exactly where the throw found it — its locator still recorded, so it still counts
   * as running and a later probe can retry it — while every other claim's reconcile still runs
   * and is awaited before boot continues. */
  async reconnectWorkers(): Promise<void> {
    const claims = Object.entries(this.deps.state.roles).filter(
      (entry): entry is [string, WorkerRoleClaim & { locator: Locator }] =>
        "issue" in entry[1] && entry[1].locator !== undefined
    );
    await Promise.all(
      claims.map(async ([token, claim]) => {
        try {
          await this.reconnectWorker(token, claim);
        } catch (error) {
          console.error(`[legion] failed to reconcile worker ${token}:`, error);
        }
      })
    );
  }

  private async reconnectWorker(
    token: string,
    claim: WorkerRoleClaim & { locator: Locator }
  ): Promise<void> {
    // Captured once, immutably, before any await: a concurrent respawn replacing this
    // claim's locator mid-probe (e.g. a dead-socket `spawnWorker` decision finishing while
    // this exact connect is still in flight) must never be mistaken for the locator this
    // call is actually probing — `retireUnconfirmedBoot`'s/`markWorkerDeadLocked`'s own
    // locator identity check only protects against retiring the WRONG locator if this one
    // is passed correctly.
    const probedLocator = claim.locator;
    const parsed = parseRoleToken(this.deps.state.project, token);
    let treeKey: IssueKey | undefined;
    let role: LegionRole | undefined;
    if (parsed && !("controller" in parsed)) {
      role = parsed.role;
      treeKey = this.rootForIssue(parsed.issue);
    }
    const probe = await probeWorker(
      () => this.clientFor(token, probedLocator),
      this.workerRpcTimeoutMs
    );
    if (!probe.client) {
      console.error(`[legion] failed to reconnect worker ${token}:`, probe.connectError);
      if (claim.readyConfirmedAt !== undefined) {
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
    // `probeWorker`'s `get_state` call seeds `runState` from the response's
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
    if (claim.readyConfirmedAt === undefined && treeKey && role) {
      this.bootWatchdog.arm(
        treeKey,
        claim.issue,
        role,
        token,
        claim.locator,
        claim.generation ?? 1
      );
    }
  }

  /** Re-arms the root registration deadline for every active tree with a recorded locator that
   * never reached `/process/ready` before a restart (`readyConfirmedAt` unset) -- the
   * `rootRegistrationWaits` map is purely in-memory, so a daemon restart between `spawnTree`
   * recording a locator and a confirmed ready discards whatever deadline was armed for it,
   * exactly like `reconnectWorkers` re-arms the boot watchdog for a worker whose
   * `/worker/started` never confirmed. A dead tree still holding an admission slot is a persisted
   * mid-resurrection state, not a live root: hold it for `replayHeldRecoveries` so it resumes
   * with `resumeSessionFile` after boot retains the reserved slot. An already-confirmed active
   * tree (or one with no locator at all -- nothing was ever launched, or `reconcileAdmission`'s
   * own orphan check already demoted it) is left untouched. Each tree is armed in isolation,
   * exactly like `reconnectWorkers`: a throw while arming one is logged with its key and the rest
   * are still armed. */
  reconnectRoots(): void {
    for (const tree of Object.values(this.deps.state.trees)) {
      if (tree.status === "dead" && this.deps.state.admission.active.includes(tree.root)) {
        this.heldResurrects.add(tree.root);
        continue;
      }
      if (tree.status !== "active" || !tree.locator || tree.readyConfirmedAt !== undefined) {
        continue;
      }
      try {
        this.armRootRegistrationDeadline(tree.root, tree.generation);
      } catch (error) {
        console.error(`[legion] failed to reconcile root ${tree.root}:`, error);
      }
    }
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
      // A root that exited on its own is relaunchable: keep its session file so the resurrection
      // (or the promotion, if its slot is gone by then) resumes the same agent (LEGION-83). A
      // teardown (`reportRootExit`, no `status`) keeps nothing -- a re-admitted closed issue
      // starts fresh, as before.
      if (status === "dead" && tree.locator?.ompSessionFile !== undefined) {
        tree.resumeSessionFile = tree.locator.ompSessionFile;
      }
      delete tree.locator;
    }
    if (status) tree.status = status;
    await this.releaseSlot(treeKey);
    await this.persist();
  }

  /**
   * Handles a boot confirmed dead before its ready path completed, with its process/socket both
   * gone: retires whatever is left of the process, clears the locator (stashing its
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
   * over-admission, or a second process racing a concurrent same-role spawn — and would also open
   * a process during boot's launch hold when this runs from `reconnectWorkers` (before
   * `enableLaunches()`). The whole decision runs under `mutateClaim(token)`, serialized
   * against every other admission/retirement decision for this token (`spawnWorker`,
   * `handleWorkerStarted` via `mutateLiveRoleClaim`) — and re-validates the claim it was handed
   * against the current one before touching anything, since the caller may have captured it,
   * or decided this boot was dead, some time before this actually runs: a claim that has since
   * completed its ready path (`readyConfirmedAt` now set), superseded by a newer launch (a
   * different generation or process), or deleted entirely (`closeTree`) means this retirement is
   * stale and must never touch what replaced it. Also never fights a `closeTree` already tearing
   * this claim's tree down: `closeTreeLocked`'s own fixed-point loop already owns stopping (and
   * deleting) every worker under a closing/closed tree through its own `stopProcessSerialized`
   * call — retiring this same token again here would either find nothing left to stop or, worse,
   * stop a respawned generation `closeTree` never asked for, so a tree that `isTreeGone` reports
   * gone makes this a no-op instead. Shared by the boot watchdog's own dead verdict,
   * `reconnectWorkers`' restart-time probe, and a pre-confirmation socket close
   * (`onWorkerClientClosed`).
   */
  private async retireUnconfirmedBoot(
    token: string,
    locator: Locator,
    generation: number | undefined,
    retry?: { treeKey: IssueKey; issue: IssueKey; role: LegionRole }
  ): Promise<void> {
    await this.workerAdmission.mutateClaim(token, async () => {
      const claim = this.deps.state.roles[token];
      if (
        !claim ||
        !("issue" in claim) ||
        claim.readyConfirmedAt !== undefined ||
        claim.generation !== generation ||
        !sameProcess(claim.locator, locator)
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
        await this.persist();
        return;
      }
      const { issue, role } = retry;
      const failures = (claim.launchFailures ?? 0) + 1;
      claim.launchFailures = failures;
      // `===`, not `>=`: fires exactly once, at the tick `failures` first reaches the threshold —
      // see `launchWorker`'s own publish for why.
      if (failures === MAX_LAUNCH_FAILURES) {
        this.publishArchitect({ type: "worker-died", issue, role });
        await this.persist();
        return;
      }
      // Pushed onto the queue *before* this save (not after, and not via `enqueueForRetry`'s
      // own separate persist) so the locator-clear above and this queue-push land in the same
      // durable transition: two separate saves would let a crash, or a persist failure,
      // between them strand this claim — locator-less, unqueued, and invisible to
      // `reconnectWorkers`' locator-only filter, with nothing left to ever retry it.
      await this.workerAdmission.enqueueForRetryPending(token);
      await this.persist();
      this.workerAdmission.promoteWorkerQueue();
    });
  }

  /** The root architect's own `/process/exit` self-report on an OPEN issue (see `recordRootExit`).
   * Ignored, beyond one log line, while the daemon is itself stopping this root to relaunch it
   * (`stoppingForRelaunch`): that recovery owns the locator, the status, and the admission slot,
   * and treating its own graceful stop as a death is exactly what released the slot (LEGION-83). */
  async markProcessDead(treeKey: IssueKey, generation?: number): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.generation !== (generation ?? tree.generation)) return;
    if (this.stoppingForRelaunch.has(treeKey)) {
      console.error(
        `[legion] ignoring ${treeKey}'s exit self-report: the daemon is stopping this root itself to relaunch it, and that recovery owns its locator, status, and admission slot`
      );
      return;
    }
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
   * configured tree stop timeout before `stopProcess` has the runtime kill it directly. Idempotent: a
   * second call for a tree already closing awaits the same in-flight close instead of starting a
   * new one (`closingTrees`, in-memory only — see its field comment for why this is never
   * persisted). `stopRoot: false` names the one caller for whom the root's OWN shutdown must be
   * skipped entirely — `reportRootExit`, the root architect reporting its own exit on a closed
   * issue (see that method's doc comment for why gracefully stopping that same process would
   * deadlock); its own locator is still cleared, since the caller already confirmed it is gone.
   * Every other (unilateral) caller — `expireLinger` — stops the root too, but only if `probe`
   * confirms it is still alive; a dead root has nothing to gracefully close, so `stopProcess`
   * skips straight to reaping whatever process is left rather than burning the full stop timeout
   * finding that out again. Its first durable act (before any stop) is marking the tree
   * `lingering` with a fresh `lingerUntil` if it is not already — so a crash anywhere during the
   * close leaves a retryable `lingering` tree the periodic sweep re-closes, never a durably
   * `active` record with an already-half-stopped process underneath it. Runs to a fixed point:
   * `inFlightLaunches` (see its field comment) is awaited before every re-snapshot of
   * `state.roles`, so a `spawnWorker` call that crossed the closing check just before
   * `closingTrees` was populated always gets to finish (and be seen) before a snapshot can
   * report the tree empty — every claim present at any point during this call gets exactly one
   * stop attempt. A claim whose stop fails (`StopFailed` — a real runtime stop failure, not the
   * routine "process already gone" case) is never deleted and its locator never cleared: the
   * tree is left `lingering` with a fresh `lingerUntil` so the periodic sweep retries the close,
   * and this call throws `StopFailed` rather than reporting a false success. The admission slot
   * a mid-close `admit` took is released by `closeTreeLocked` but handed on only here, once
   * `closingTrees` no longer names the tree: the promotion sweep launches roots
   * (`advancePromotionSweep` → `startRoot`), and a launched root first awaits any in-flight close
   * of its own tree or an ancestor's (`awaitClosingTrees`) — so a queued child of this tree,
   * promoted from inside the frame `closingTrees` still named, would wait on the very close that
   * was promoting it and neither would ever settle (found in LEGION-104's review). Nothing the
   * fence protects needs it any longer by then: every claim is deleted and the record reads
   * `closed`, which `isTreeGone` refuses launches for on its own.
   */
  async closeTree(treeKey: IssueKey, options?: { stopRoot?: boolean }): Promise<void> {
    const inFlight = this.closingTrees.get(treeKey);
    if (inFlight) {
      await inFlight;
      return;
    }
    const closing = this.closeTreeLocked(treeKey, options).finally(() => {
      this.closingTrees.delete(treeKey);
    });
    this.closingTrees.set(treeKey, closing);
    const releasedSlot = await closing;
    if (releasedSlot) await this.beginPromotionSweep();
  }

  /** Returns whether the close released an admission slot: only a tree `admit` re-activated
   * mid-close holds one by the time it closes (`beginLinger` and `recordRootExit` released it
   * before the close began). The slot is spliced here but never handed on from inside this frame
   * — `closeTree` starts the promotion sweep once `closingTrees` has forgotten the tree. */
  private async closeTreeLocked(
    treeKey: IssueKey,
    options?: { stopRoot?: boolean }
  ): Promise<boolean> {
    const tree = this.requireTree(treeKey);
    this.cancelRootRegistrationDeadline(treeKey);
    if (tree.status !== "lingering") {
      tree.status = "lingering";
      tree.lingerUntil = new Date(this.deps.now()).toISOString();
      await this.persist();
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
        // The graceful shutdown is skipped only when the process is proven gone. Any other dead
        // verdict -- a legacy identity-less locator, a reissued pane id -- still asks the root to
        // exit over its own role-scoped socket: that path reaches exactly the process this tree
        // recorded if it is alive at all, and the runtime's destroy step is refused for a
        // process that is not the recorded one. `probeTree` logs the decision itself. A probe
        // the runtime could not complete is a failure of this leg like a failed stop: the tree
        // stays lingering for the sweep to retry, its locator untouched.
        try {
          const verdict = await this.probeTree(treeKey);
          await this.stopProcessSerialized(architectToken, rootLocator, this.treeStopTimeoutMs, {
            skipGraceful: verdict.status === "dead" && verdict.reason === "gone",
            refuseKill: verdict.status === "dead" && verdict.reason === "not-recorded-process",
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
        (entry): entry is [string, WorkerRoleClaim & { locator: Locator }] =>
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
      const stoppedLocators = new Map(batch.map(([token, claim]) => [token, { ...claim.locator }]));
      const settled = await Promise.allSettled(
        batch.map(([token, claim]) =>
          this.stopProcessSerialized(token, claim.locator, this.treeStopTimeoutMs)
        )
      );
      for (const [index, [token]] of batch.entries()) {
        const result = settled[index];
        if (result?.status === "fulfilled") {
          // Re-acquires the token's own critical section for the delete itself, re-checking the
          // claim's identity (`sameProcess`) against the locator that was actually stopped — mirrors
          // `retireUnconfirmedBoot`'s stale-identity guard. `stopProcessSerialized` above already
          // ran under this same lock, but only for the stop call; without re-acquiring it here,
          // a live writer (e.g. `/worker/started`) that raced in in-between (took the lock after
          // the stop released it but before this delete runs) and wrote a fresh locator for a
          // respawned generation would have that fresh claim silently deleted by a stop that
          // targeted the OLD, now-irrelevant process.
          await this.workerAdmission.mutateClaim(token, async () => {
            const current = this.deps.state.roles[token];
            if (
              current &&
              "issue" in current &&
              sameProcess(current.locator, stoppedLocators.get(token))
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
          // Leave this claim and its locator exactly as they are — a possibly-still-live process
          // must never lose its only durable handle.
        }
      }
    }
    if (anyFailed) {
      tree.status = "lingering";
      tree.lingerUntil = new Date(this.deps.now()).toISOString();
      await this.persist();
      throw new StopFailed(
        treeKey,
        `Legion tree ${treeKey} has a process that could not be stopped`
      );
    }

    // Every process under the tree is stopped (or this call has thrown above): the workspaces of
    // the finished issues go now, before the record turns `closed` — see `removeTreeWorkspaces`.
    await this.removeTreeWorkspaces(treeKey, tree);

    tree.status = "closed";
    delete tree.lingerUntil;
    const rootStatus = this.deps.state.issues[treeKey]?.status;
    if (rootStatus !== "done" && rootStatus !== "backlog" && rootStatus !== "icebox") {
      await writeStatus(this.deps.state, this.deps.dispatchClient, treeKey, "done");
    }
    const admission = this.deps.state.admission;
    const slot = admission.active.indexOf(treeKey);
    const releasedSlot = slot !== -1;
    if (releasedSlot) admission.active.splice(slot, 1);
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
    await this.persist();
    // Deleting this tree's claims may have freed running-worker slots other trees' queues are
    // waiting on.
    this.workerAdmission.promoteWorkerQueue();
    return releasedSlot;
  }

  /**
   * Has the runtime reap every process it owns that this state does not name (via a tree's, a
   * worker claim's, or the controller's locator) and that has been idle at least `graceMs` —
   * long enough that a process mid-creation (its locator not yet recorded) is never mistaken for
   * an orphan. Boot calls this with `graceMs: 0`: it never trusts a process it did not itself
   * record, so there is no such race to protect against there (see `reconcileAdmission`). What
   * "known" means per runtime is `locatorHandles`' business (runtime.ts); the runtime decodes the
   * handles it produced.
   */
  async reconcileOrphans(graceMs = ORPHAN_RECONCILIATION_GRACE_MS): Promise<void> {
    const known = new Set<string>();
    for (const locator of this.recordedLocators()) {
      for (const handle of locatorHandles(locator)) known.add(handle);
    }
    await this.runtime.reconcileOrphans(known, graceMs);
  }

  /** Every locator state currently records: each tree's, each worker claim's, the controller's. */
  private recordedLocators(): Locator[] {
    const locators = [
      ...Object.values(this.deps.state.trees).map((tree) => tree.locator),
      ...Object.values(this.deps.state.roles).map((claim) =>
        "issue" in claim ? claim.locator : undefined
      ),
      this.deps.state.controllerLocator,
    ];
    return locators.filter((locator): locator is Locator => locator !== undefined);
  }

  /** Awaits every in-flight `closeTree` for `issue` or an ancestor of it before a root launch
   * provisions its workspace: that close may be removing exactly this directory
   * (`removeTreeWorkspaces`). A root re-admitted while its previous tree still closes (a human
   * moves a finished issue back to `todo` within the close's stop window) and a child re-admitted
   * as its own root while its parent's tree closes both wait here; the close itself removes
   * nothing for a record that is no longer `lingering`. A failed close is its own caller's to log
   * — the launch proceeds either way, exactly as it does today. Phase-worker launches need no
   * wait: `launchWorker` refuses a closing tree outright (`TreeClosingError`). */
  private async awaitClosingTrees(issue: IssueKey): Promise<void> {
    const seen = new Set<IssueKey>();
    for (
      let current: IssueKey | undefined = issue;
      current !== undefined && !seen.has(current);
      current = this.deps.state.issues[current]?.parent
    ) {
      seen.add(current);
      const closing = this.closingTrees.get(current);
      if (!closing) continue;
      console.error(`[legion] launch of ${issue} waits for the close of tree ${current} to finish`);
      await closing.catch(() => {});
    }
  }

  async spawnRoot(issue: IssueKey, resume = false, resumeSessionFile?: string): Promise<void> {
    await this.awaitClosingTrees(issue);
    const tree = this.ensureTree(issue);
    const priorGeneration = tree.generation;
    const priorLocator = tree.locator;
    const priorReadyConfirmedAt = tree.readyConfirmedAt;
    const priorResumeSessionFile = tree.resumeSessionFile;
    // `resumeSessionFile` is an explicit resurrection argument; `priorResumeSessionFile` is the
    // durable handoff from a prior dead/queued transition. A below-threshold failed launch must
    // preserve whichever one supplied this attempt so its queued retry never starts a new agent.
    const retryResumeSessionFile = resumeSessionFile ?? priorResumeSessionFile;
    // A tree that kept its session file across a cleared pane (`recordRootExit`, the at-cap branch
    // of `resurrectDeadTree`) is always resumed, whatever the caller asked: `admit` and the
    // promotion sweep ask for a fresh launch because they cannot tell a never-started tree from a
    // requeued one (LEGION-83).
    const resuming = resume || tree.resumeSessionFile !== undefined;
    tree.generation += 1;
    // Held for the whole launch, released immediately before each persist below once the
    // outcome is in state — see `holdProcessSecret`. Two generations of one root can be in flight
    // at once; each holds its own count.
    const releaseSecret = this.holdProcessSecret(
      roleToken(this.deps.state.project, issue, "architect")
    );
    try {
      await this.spawnTree(tree, resuming, resumeSessionFile);
    } catch (error) {
      // `spawnTree` clears `readyConfirmedAt` before the process ever starts (see its doc comment)
      // and only arms the registration deadline once a locator actually exists -- a throw here
      // can land either before that clear (nothing to cancel) or after a locator was recorded
      // and armed but a later step in the happy path failed (e.g. `writeStatus`), so cancel
      // unconditionally; canceling an unarmed generation is a no-op. Restore both fields the
      // clear may have touched, mirroring the locator rollback below, so a rolled-back prior
      // generation that was already confirmed does not look unconfirmed again.
      this.cancelRootRegistrationDeadline(issue, tree.generation);
      tree.generation = priorGeneration;
      if (priorLocator) tree.locator = priorLocator;
      else delete tree.locator;
      if (priorReadyConfirmedAt !== undefined) tree.readyConfirmedAt = priorReadyConfirmedAt;
      else delete tree.readyConfirmedAt;
      if (priorResumeSessionFile !== undefined) tree.resumeSessionFile = priorResumeSessionFile;
      else delete tree.resumeSessionFile;
      // Released as soon as the rollback is in state and before anything below can throw
      // (`publishController`, the promotion-sweep awaits): a hold that outlives this catch would
      // exempt the architect file for the daemon's lifetime. This generation stored no locator,
      // so the persist below reaps the file — unless the rollback restored a prior generation's
      // locator, which keeps it live.
      releaseSecret();
      tree.launchFailures += 1;
      const activeIndex = this.deps.state.admission.active.indexOf(issue);
      if (activeIndex !== -1) this.deps.state.admission.active.splice(activeIndex, 1);
      if (tree.launchFailures >= MAX_LAUNCH_FAILURES) {
        tree.status = "launch-failed";
        // A controller re-admit of a launch-failed tree starts fresh, as it did before a tree
        // remembered a session file: a file that is what keeps failing (gone from disk, see
        // `computeResumeArgument`) would otherwise fail every re-admit the same way.
        delete tree.resumeSessionFile;
        const queueIndex = this.deps.state.admission.queue.indexOf(issue);
        if (queueIndex !== -1) this.deps.state.admission.queue.splice(queueIndex, 1);
        this.publishController({
          type: "launch-failed",
          issue,
          failures: tree.launchFailures,
        });
      } else {
        tree.status = "queued";
        if (retryResumeSessionFile !== undefined) {
          tree.resumeSessionFile = retryResumeSessionFile;
        }
        if (!this.deps.state.admission.queue.includes(issue)) {
          this.deps.state.admission.queue.push(issue);
        }
      }
      this.settlePromotionSpawn(issue);
      if (this.promotionSweep) await this.advancePromotionSweep();
      else await this.beginPromotionSweep(issue);
      await this.persist();
      throw error;
    }

    // The spawn itself succeeded — a real root process is running. A save
    // failure past this point is not a launch failure: rolling back
    // generation/status here would make the daemon retry a tree that
    // already has a live process. Stop that process first instead — every
    // spawn is either persisted or reaped, never left running unrecorded —
    // then propagate the failure distinctly (see `SpawnPersistenceFailure`)
    // so this goes fatal like every other durable effect whose
    // post-mutation save fails. `launchFailures` is deliberately left
    // untouched here rather than reset: only a confirmed `/process/ready`
    // resets it (`confirmRootReady`), so a process that starts but never gets
    // there keeps its accumulated count across the retry
    // `retireUnconfirmedRoot` drives instead of resetting to 0 on every
    // successful spawn, letting repeated never-confirmed cycles still
    // escalate to `MAX_LAUNCH_FAILURES`.
    this.settlePromotionSpawn(issue);
    if (this.promotionSweep?.inFlight === 0) this.promotionSweep = undefined;
    // `spawnTree` has stored this generation's locator (or, for a superseded generation, left
    // the newer one's in place — whose own spawnRoot still holds its count), so this persist's
    // prune sees the file referenced.
    releaseSecret();
    try {
      await this.persist();
    } catch (error) {
      if (tree.locator) {
        // Reaps exactly the process this spawn opened — never a sibling worker sharing whatever
        // the runtime grouped it with.
        try {
          await this.stopProcess(
            roleToken(this.deps.state.project, issue, "architect"),
            tree.locator,
            this.workerStopTimeoutMs,
            { skipGraceful: true }
          );
        } catch (stopError) {
          console.error(`[legion] failed to reap ${issue}'s unpersisted root process:`, stopError);
        }
      }
      throw new SpawnPersistenceFailure(error);
    }
  }

  /**
   * A live process is not proof of a role holder: `controllerAlive()` only confirms the
   * controller's process itself is running, not that it ever reached `/controller/ready` (a
   * stuck plugin) or that its Envoy role claim survived while the process did (lost independently
   * of the process dying). Either way, an alive-but-unclaimed controller would otherwise strand
   * every pending notice forever, since nothing else ever retries a process this method already
   * considers "there". Arms a bounded
   * wait bound to the exact locator observed -- an already-alive-but-unclaimed one here, or a
   * freshly-spawned one below, since a brand new controller can just as easily never register --
   * `workerBootTimeoutSeconds * workerBootRegistrationDeadlineIntervals` (the same budget a
   * worker's own boot gets), rather than resetting the clock on every call. If the role is still
   * unclaimed once that wait elapses, retires the stuck process and spawns a fresh one in its place.
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
    if (!this.runtime.launchesController) {
      // Nothing to mint and nothing to spawn: a Kubernetes daemon's controller is not this
      // runtime's to launch (LEGION-25). Logged once, not per controller-bound event.
      if (!this.loggedControllerNotLaunched) {
        this.loggedControllerNotLaunched = true;
        console.error(
          "[legion] the controller is not launched by this runtime (LEGION-25); controller-bound events wait for one started elsewhere"
        );
      }
      return;
    }
    if (!this.launchesEnabled) {
      // Spawning the controller opens a pane. Held for `replayHeldRecoveries()`.
      this.heldControllerRequest = true;
      console.error("[legion] controller launch held until the OMP probe passes");
      return;
    }
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
    const { timedOut, cancel } = boundedWait(deadlineMs, this.deps.sleep);
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

  /** Arms a bounded wait for `treeKey`'s just-recorded `generation` to reach `/process/ready` --
   * `spawnTree`'s own happy path is the only caller, right after recording a fresh locator, so
   * only a generation that actually got a live process ever gets one. Uses the same budget the
   * controller's own registration wait and a worker's boot watchdog use
   * (`workerBootTimeoutSeconds * workerBootRegistrationDeadlineIntervals`). Unlike the
   * controller's single in-flight wait, this is tracked per tree -- replaces whatever was
   * already armed for this exact tree unconditionally, since the only way to reach this twice
   * for the same tree is a newer generation superseding an older, still-unconfirmed one. */
  private armRootRegistrationDeadline(treeKey: IssueKey, generation: number): void {
    this.rootRegistrationWaits.get(treeKey)?.cancel();
    const deadlineMs =
      this.deps.config.workerBootTimeoutSeconds *
      1_000 *
      this.deps.config.workerBootRegistrationDeadlineIntervals;
    const { timedOut, cancel } = boundedWait(deadlineMs, this.deps.sleep);
    this.rootRegistrationWaits.set(treeKey, { generation, cancel });
    void timedOut.then(() =>
      this.retireUnconfirmedRoot(treeKey, generation).catch((error) => {
        console.error(
          `[legion] failed to retire and resurrect an unconfirmed root for ${treeKey}:`,
          error
        );
      })
    );
  }

  /** Cancels the armed root-registration wait for `treeKey`, if any -- a no-op if none is
   * armed, or if `generation` is given and does not match the armed wait's (a stale caller must
   * never cancel a newer wait it was never meant to touch). */
  private cancelRootRegistrationDeadline(treeKey: IssueKey, generation?: number): void {
    const current = this.rootRegistrationWaits.get(treeKey);
    if (!current) return;
    if (generation !== undefined && current.generation !== generation) return;
    current.cancel();
    this.rootRegistrationWaits.delete(treeKey);
  }

  /** Cancels every armed root-registration wait, for every tree. Called alongside `dispose()`'s
   * other watchdog teardown so no background timer outlives the ProcessManager. */
  private cancelAllRootRegistrationDeadlines(): void {
    for (const { cancel } of this.rootRegistrationWaits.values()) cancel();
    this.rootRegistrationWaits.clear();
  }

  /** Arms (or re-arms) `token`'s idle-retire clock for `workerIdleRetireSeconds` -- a no-op when that
   * is 0 (the timer is disabled) or after `dispose()`. Uses the same injectable timer surface as the
   * boot watchdog and the registration deadlines (`boundedWait` over `deps.sleep`), and judges every
   * condition at expiry, never here: a worker re-prompted inside the window fails the idle check
   * then, and its next idle report arms a fresh clock. Two arm sites: the client's own idle
   * transition (`clientFor`), and `retireIdleWorker`'s expiry when it declines for a reason that can
   * change while the worker stays idle -- that worker will never report idle again, so the expiry is
   * the only thing left that can arm its next clock. Replaces whatever clock was already armed for
   * this token (the entry-identity check in the expiry callback is what makes the replaced one
   * inert). */
  private armIdleRetire(token: string, client: WorkerRpcClient): void {
    const retireMs = this.deps.config.workerIdleRetireSeconds * 1_000;
    if (retireMs === 0 || this.disposed) return;
    this.idleRetireWaits.get(token)?.cancel();
    const { timedOut, cancel } = boundedWait(retireMs, this.deps.sleep);
    const wait = { cancel };
    this.idleRetireWaits.set(token, wait);
    void timedOut.then(() => {
      // A cancelled or superseded clock still resolves (see `idleRetireWaits`); only the current arm acts.
      if (this.idleRetireWaits.get(token) !== wait) return;
      this.idleRetireWaits.delete(token);
      return this.retireIdleWorker(token, client).catch((error) => {
        console.error(`[legion] failed to retire idle worker ${token}:`, error);
      });
    });
  }

  /** Cancels every armed idle-retire clock. Called from `dispose()` so no background timer outlives
   * the ProcessManager. */
  private cancelAllIdleRetireClocks(): void {
    for (const { cancel } of this.idleRetireWaits.values()) cancel();
    this.idleRetireWaits.clear();
  }

  /**
   * The idle-retire clock's expiry (see `armIdleRetire`). Inside `token`'s `mutateClaim` critical
   * section -- serialized against `spawnWorker`, `markWorkerDead`, `closeTree`'s stops, and queue
   * promotion for this role, every one of which is the only way a prompt or a stop reaches this
   * worker -- re-reads the live state and retires the worker only if all of these still hold: the
   * cached client is still `client` and still reports `"idle"` (a prompt that landed inside the
   * window flipped it to `"running"` synchronously); the claim is a ready-confirmed worker claim with
   * a locator (a boot still in flight is never retired); the role is not `architect` (an architect
   * has no phase of its own -- it is never `phases[issue].phase`, so the not-active-phase test below
   * would pass on every idle -- and it parks by design between wakes for the life of its subtree, so
   * each wake to a retired one would relaunch it through the no-holder recovery: one relaunch per
   * wake costs more than one idle process per child issue); the tree is neither closing nor closed
   * (`closeTree` owns stopping every worker under it); no `pendingAssignment` is queued (a queued
   * task prompts it in place when a slot frees); and `phases[claim.issue]` is absent, `completed`,
   * or names a different role -- phase completion is judged per role, not per issue, so an idle
   * implementer retires while the tester runs on the same issue. Then performs exactly
   * `markWorkerDeadLocked`'s retirement -- `retireWorkerLocator` (graceful `shutdown` frame,
   * kill-pane fallback), clear the locator, carry `ompSessionFile` into `resumeSessionFile`, persist
   * -- and never touches `launchFailures`/`promptFailures`: this worker is healthy, the daemon chose
   * to stop it. The socket close this causes reaches `onWorkerClientClosed`, whose one reconnect
   * probe fails against the exited shim and routes to `markWorkerDead`; queued behind this same
   * critical section, its `markWorkerDeadLocked` finds the locator already cleared and returns -- no
   * launch failure counted, no `worker-died` published. The next `spawn_worker` for the role finds a
   * locator-less claim with `resumeSessionFile` and launches with `--resume`, exactly the dead-pane
   * recovery shape. No `promoteWorkerQueue()` afterwards: an idle client was never counted by
   * `runningWorkerCount`, so nothing was freed.
   *
   * A decline on exactly the last two conditions -- the role is the issue's active phase, or a
   * `pendingAssignment` is queued -- re-arms the clock. Both change without this worker ever
   * transitioning to idle again (`promptExistingWorker` delivering an architect assignment to
   * another role re-writes `phases[issue]`; a promotion drains the queued task), and an already-idle
   * worker's `onIdle` never fires again, so without the re-arm a worker that finished its turn while
   * still the recorded phase would stay resident for the life of the tree once the phase moves on.
   * Re-armed inside this same critical section, so it cannot interleave with a prompt; the
   * identity/idle/disposed checks at the top still hold -- nothing has been awaited since. No other
   * decline re-arms: a running worker's own next idle report arms; an architect is never retired, so
   * a re-arm would only spin; a closing or closed tree is `closeTree`'s to stop; a missing claim,
   * locator, or confirmation, or a replaced client, has no worker of this clock's left to judge.
   */
  private async retireIdleWorker(token: string, client: WorkerRpcClient): Promise<void> {
    await this.workerAdmission.mutateClaim(token, async () => {
      if (this.disposed) return;
      if (this.workerClients.get(token) !== client || client.runState !== "idle") return;
      const claim = this.deps.state.roles[token];
      if (!claim || !("issue" in claim) || !claim.locator || claim.readyConfirmedAt === undefined) {
        return;
      }
      if (claim.role === "architect") return;
      const treeKey = this.rootForIssue(claim.issue);
      if (treeKey === undefined || this.isTreeGone(treeKey, claim.issue)) return;
      if (
        claim.pendingAssignment !== undefined ||
        isActivePhase(this.deps.state, claim.issue, claim.role)
      ) {
        this.armIdleRetire(token, client);
        return;
      }
      const locator = claim.locator;
      console.info(
        `[legion] retiring idle worker ${token}: idle for ${this.deps.config.workerIdleRetireSeconds}s with no active phase; it resumes from its OMP session on its next assignment`
      );
      await this.retireWorkerLocator(token, locator);
      const resumeSessionFile = locator.ompSessionFile ?? claim.resumeSessionFile;
      delete claim.locator;
      if (resumeSessionFile) claim.resumeSessionFile = resumeSessionFile;
      await this.persist();
    });
  }

  /** Confirms `treeKey` reached `/process/ready` for `generation`: cancels its root-registration
   * deadline (a no-op if this generation's wait was never armed or was already superseded),
   * persists `readyConfirmedAt` -- the durable marker `retireUnconfirmedRoot`'s own race-safe
   * re-checks and `reconnectRoots`'s boot-time re-arm decision both read, so a restart or an
   * in-flight expiry can never observe a stale "unconfirmed" the way the in-memory wait map
   * alone could -- and resets `launchFailures`. Ready, not `/process/started`, is the
   * confirmation this counter waits for -- `spawnRoot`'s own success path no longer resets it on
   * a mere spawn, so a root that keeps spawning but never reaching `ready` still
   * escalates to `MAX_LAUNCH_FAILURES` instead of looping forever. */
  confirmRootReady(treeKey: IssueKey, generation: number): void {
    this.cancelRootRegistrationDeadline(treeKey, generation);
    const tree = this.deps.state.trees[treeKey];
    if (!tree || tree.generation !== generation) return;
    tree.readyConfirmedAt = this.deps.now();
    tree.launchFailures = 0;
  }

  /** Shared unconfirmed-root failure accounting for `retireUnconfirmedRoot`'s two branches (a
   * dead process, or an alive-but-unconfirmed process already retired) -- cancels whatever remains of
   * this generation's registration wait (a fresh spawn arms its own if `onRetry` succeeds; a
   * `launch-failed` tree needs none left dangling), increments `launchFailures`, and at
   * `MAX_LAUNCH_FAILURES` marks the tree `launch-failed`, revokes its architect capability
   * (nothing is left to trust once this daemon gives up on it), and releases its admission slot
   * through `releaseSlot` -- not a direct `admission.active` splice -- so a queued root waiting
   * behind this failed one is promoted immediately instead of stranded until some unrelated
   * `admit()`/`reconcileAdmission()` call happens to notice the freed slot; `releaseSlot`'s own
   * promotion sweep persists the full state, this tree's own mutations included, so no separate
   * persist is needed on that branch. Below the threshold, persists the incremented count and
   * re-checks the daemon is not disposed and the tree is still this same, still-`"active"`
   * object before running `onRetry` (the resurrect attempt) -- a `dispose()`, `closeTree`, or
   * `beginLinger` landing during that persist's own await must still win over this retry
   * decision, never be raced into resurrecting a root after shutdown or into a closed/lingering
   * tree. Either branch counts toward the same threshold, dead or merely unconfirmed, so
   * repeated never-confirmed cycles always escalate instead of looping forever. */
  /** True if `tree` -- assumed freshly re-read from `deps.state.trees`, never a reference held
   * across an await -- is still exactly as an in-flight unconfirmed-root recovery attempt found
   * it: not disposed, still on `generation`, still `"active"`, and still missing
   * `readyConfirmedAt`. Shared by `retireUnconfirmedRoot`'s own per-await re-checks and
   * `escalateOrRetryUnconfirmedRoot`'s post-persist re-check below, so every one of them applies
   * the identical predicate: a `/process/ready` landing, a `dispose()`, or a newer generation's
   * own spawn arriving during any of those awaits always wins over the stale recovery decision. */
  private treeStillUnconfirmed(tree: TreeState | undefined, generation: number): tree is TreeState {
    return (
      !this.disposed &&
      tree !== undefined &&
      tree.generation === generation &&
      tree.status === "active" &&
      tree.readyConfirmedAt === undefined
    );
  }

  private async escalateOrRetryUnconfirmedRoot(
    treeKey: IssueKey,
    tree: TreeState,
    onRetry: () => Promise<void>
  ): Promise<void> {
    const generation = tree.generation;
    this.cancelRootRegistrationDeadline(treeKey, generation);
    tree.launchFailures += 1;
    if (tree.launchFailures >= MAX_LAUNCH_FAILURES) {
      tree.status = "launch-failed";
      delete tree.locator;
      // Same as spawnRoot's launch-failed branch: a re-admit starts fresh.
      delete tree.resumeSessionFile;
      const architectToken = roleToken(this.deps.state.project, treeKey, "architect");
      const architectClaim = this.deps.state.roles[architectToken];
      this.revokeRoleClaim(
        architectClaim && "issue" in architectClaim ? architectClaim : undefined
      );
      const queueIndex = this.deps.state.admission.queue.indexOf(treeKey);
      if (queueIndex !== -1) this.deps.state.admission.queue.splice(queueIndex, 1);
      this.publishController({
        type: "launch-failed",
        issue: treeKey,
        failures: tree.launchFailures,
      });
      await this.releaseSlot(treeKey);
      return;
    }
    await this.persist();
    // Re-check after the persist's own await, via the same predicate `retireUnconfirmedRoot`'s
    // own probe path uses (see `treeStillUnconfirmed`'s doc comment): a ready confirmation or a
    // newer generation's spawn landing during this persist must still win over the retry, not
    // just a dispose/closeTree/beginLinger status change.
    if (!this.treeStillUnconfirmed(this.deps.state.trees[treeKey], generation)) return;
    try {
      await onRetry();
    } catch (error) {
      // The retry is `resurrect`. Two things in it can throw: its own re-probe, exactly as the
      // probe that brought us here can (a `list-panes` that proves nothing about the pane -- the
      // runtime refused to fake a verdict, so nothing was cleared), and the `spawnRoot` that
      // follows (its own accounting has already queued or launch-failed the tree). Its stop
      // cannot: `removeTreeProcess` logs a stop failure and clears the locator in `finally`.
      // Either way the deadline was cancelled above. For the first case the root would sit
      // active and unconfirmed with its locator intact until a restart -- the resync backstop
      // probes only confirmed roots -- so re-arm this same generation's deadline, as
      // `retireUnconfirmedRoot`'s stop-failure branch does; otherwise `treeStillUnconfirmed`
      // declines -- the tree is no longer this generation's active, unconfirmed one -- and it is
      // already where its own path put it. Logged here, once, with the tree as observed; the
      // deadline's own catch never sees it. (`treeStillUnconfirmed` is a type guard that narrows
      // the declined tree to `never` for the rest of this method, whichever expression it is read
      // through; `describeTreeForLog` reads it from its own scope.)
      if (this.treeStillUnconfirmed(this.deps.state.trees[treeKey], generation)) {
        console.error(
          `[legion] failed to resurrect an unconfirmed root for ${treeKey}; its locator is untouched and its registration deadline is re-armed:`,
          error
        );
        this.armRootRegistrationDeadline(treeKey, generation);
      } else {
        const observed = this.describeTreeForLog(treeKey);
        console.error(
          `[legion] failed to resurrect an unconfirmed root for ${treeKey} (generation ${generation}); the tree is no longer that generation's active, unconfirmed root (${observed}${this.disposed ? "; daemon disposing" : ""}), so no deadline is re-armed:`,
          error
        );
      }
    }
  }

  /** `status`/`generation`/confirmation of `treeKey`'s tree as recorded right now, for a log
   * line explaining why a recovery declined -- reads the tree outside `treeStillUnconfirmed`'s
   * type guard, which narrows a declined tree to `never`. */
  private describeTreeForLog(treeKey: IssueKey): string {
    const current = this.deps.state.trees[treeKey];
    if (!current) return "no tree recorded";
    const confirmed = current.readyConfirmedAt === undefined ? "" : ", ready-confirmed";
    return `status ${current.status}, generation ${current.generation}${confirmed}`;
  }

  /**
   * Runs once the registration deadline `armRootRegistrationDeadline` set for `treeKey`'s
   * `generation` elapses. `stillUnconfirmed` is re-checked after every await -- never trusted
   * only once at entry -- so a `/process/ready` landing, a `dispose()`, or a newer generation's
   * own spawn arriving mid-probe or mid-stop always wins over this stale-timeout decision: it
   * checks the daemon is not disposed, the wait entry armed for `treeKey` is still this exact
   * `generation` (a fresh spawn replaces it outright; `confirmRootReady` cancels it), and the
   * tree itself is still on `generation`, still `"active"`, and still missing
   * `readyConfirmedAt` (the durable marker `confirmRootReady` sets -- checked here, not just the
   * in-memory wait, so this decision never depends on the wait map surviving a restart the way
   * `reconnectRoots`'s own re-arm does not need to either). Otherwise re-probes the process fresh --
   * never trusts anything observed before the deadline elapsed: a dead process resurrects directly,
   * exactly like any other exception-driven recovery. A process that is still alive but never
   * reached `/process/ready` is retired first -- `stopProcessSerialized` on the recorded
   * locator, mirroring `spawnTree`'s own stale-generation retire path, without clearing
   * `tree.locator` here so the `resurrectDeadTree` call that follows still captures
   * `resumeSessionFile` from it -- so its own liveness probe finds the process dead and proceeds. A
   * stop failure re-arms the same generation's deadline (provided the tree is still exactly as
   * this attempt found it) rather than stranding an unconfirmed root with no timer left to retry
   * it. Either a dead process or a retired alive-but-unconfirmed one counts toward `launchFailures`
   * via the shared `escalateOrRetryUnconfirmedRoot` helper, so repeated never-confirmed cycles
   * still escalate to `MAX_LAUNCH_FAILURES` instead of looping forever, exactly like
   * `spawnRoot`'s own throw-driven escalation.
   */
  private async retireUnconfirmedRoot(treeKey: IssueKey, generation: number): Promise<void> {
    const stillUnconfirmed = (): TreeState | undefined => {
      const wait = this.rootRegistrationWaits.get(treeKey);
      if (!wait || wait.generation !== generation) return undefined;
      const tree = this.deps.state.trees[treeKey];
      return this.treeStillUnconfirmed(tree, generation) ? tree : undefined;
    };

    if (!stillUnconfirmed()) return;
    let alive: boolean;
    try {
      alive = (await this.probe(treeKey)) === "alive";
    } catch (error) {
      console.error(
        `[legion] failed to probe an unconfirmed root for ${treeKey}; re-arming its registration deadline rather than deciding on a probe that did not complete:`,
        error
      );
      if (stillUnconfirmed()) this.armRootRegistrationDeadline(treeKey, generation);
      return;
    }
    // Re-check after the probe's own await: see this method's doc comment.
    let tree = stillUnconfirmed();
    if (!tree) return;

    if (!alive) {
      await this.escalateOrRetryUnconfirmedRoot(treeKey, tree, () => this.resurrect(treeKey));
      return;
    }

    const locator = tree.locator;
    if (!locator) return;
    const token = roleToken(this.deps.state.project, treeKey, "architect");
    this.stoppingForRelaunch.add(treeKey);
    try {
      await this.stopProcessSerialized(token, locator, this.workerStopTimeoutMs);
    } catch (error) {
      console.error(
        `[legion] failed to retire an alive-but-unconfirmed root process for ${treeKey}; leaving it in place rather than orphaning it:`,
        error
      );
      // Re-arm the same generation's deadline so a transient stop failure is retried later,
      // provided the tree is still exactly as this attempt found it.
      if (stillUnconfirmed()) this.armRootRegistrationDeadline(treeKey, generation);
      return;
    } finally {
      this.stoppingForRelaunch.delete(treeKey);
    }
    // Re-check again after the stop's own await, for the same reason as above.
    tree = stillUnconfirmed();
    if (!tree) return;
    await this.escalateOrRetryUnconfirmedRoot(treeKey, tree, () => this.resurrect(treeKey));
  }

  /**
   * Runs once the registration deadline armed above elapses for `locator`. A no-op unless this
   * exact wait is still the one currently armed (by reference): an intervening `ensureController`
   * call may have already cancelled it (role claimed) or superseded it (this process died on its
   * own and a fresh one was spawned, arming its own wait) before this stale timer got a chance
   * to fire. Re-checks the role once before the liveness probe and again immediately after it --
   * a `/controller/ready` landing during that probe's own await must still win over this
   * stale-timeout decision, never be raced by it. A `state.controllerLocator` that no longer
   * matches `locator` by reference is the same "superseded" case caught above, checked again
   * directly against live state for good measure. On a failed stop/kill, logs and leaves the
   * locator exactly as it was -- clearing it and spawning a second controller over a process that
   * never actually stopped would orphan that process with nothing tracking it; a later
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
        "[legion] failed to stop a stuck controller process; leaving it in place rather than orphaning it:",
        error
      );
      return;
    }
    delete this.deps.state.controllerLocator;
    await this.ensureController();
  }

  /** `probeTree`'s verdict collapsed to alive/dead for callers that only branch on it. */
  async probe(treeKey: IssueKey): Promise<"alive" | "dead"> {
    return (await this.probeTree(treeKey)).status;
  }

  /** Probes a tree's recorded locator for liveness through the runtime. A tree with no locator
   * is dead (`gone`). A process that is present but not the recorded one -- the reissued-pane
   * case the identity check exists for, or a legacy locator with no identity -- is logged here,
   * once, with both identities, and reported dead so the ordinary path resumes the root onto a
   * fresh, fully-recorded process. This is the decision point and the only place that logs it:
   * the resurrection that follows (`resurrectDeadTree`) re-probes through the silent
   * `probeLocator` -- so a root that came back alive between the two is never replaced -- and
   * hands that fresh verdict to its stop, which decides nothing again. A probe the runtime
   * could not complete (a failed `list-panes`) throws through, exactly as `probeLocator`
   * documents. */
  private async probeTree(treeKey: IssueKey): Promise<Exclude<ProbeResult, { status: "unknown" }>> {
    const tree = this.deps.state.trees[treeKey];
    const locator = tree?.locator;
    if (!locator) return { status: "dead", reason: "gone" };
    const result = await this.probeLocator(locator, treeKey);
    if (result.status === "dead" && result.reason === "not-recorded-process") {
      console.error(`[legion] treating ${treeKey}'s root as dead: ${result.detail}`);
    }
    return result;
  }

  /** The one `Runtime.probe` call every liveness decision in this manager goes through, silent:
   * the caller logs what it decides. The tmux runtime never reports `unknown`; when a runtime
   * that can (LEGION-24) lands, the lifecycle policy for it lands here with it — until then it is
   * loud, never a default. A runtime that cannot complete the probe at all (tmux: `list-panes`
   * failed for a reason that does not prove the pane gone) throws instead of returning either
   * verdict, and every caller's own failure handling logs and retries later without clearing
   * anything: the resync `onProbe` on its next tick, a registration deadline by re-arming
   * itself, the boot watchdog by re-arming its interval, the linger sweep on its next pass.
   * `handleException` only logs -- the core-NATS exception lane has no redelivery -- and leaves
   * the retry to those. */
  private async probeLocator(
    locator: Locator,
    subject: string
  ): Promise<Exclude<ProbeResult, { status: "unknown" }>> {
    const result = await this.runtime.probe(locator);
    if (result.status === "unknown") {
      throw new Error(
        `Runtime probe reported an unknown status for ${subject}; ProcessManager has no unknown-status policy`
      );
    }
    return result;
  }

  /** Sends `directive` to the tree's root architect pane over `legion.ctl.<tree>.<generation>`
   * and, on `ack`, re-publishes a `reclaim-architect` directive's `redeliver` message to its role
   * topic unless `redeliver` is `false`. `redeliver.dedupeKey` is the triggering exception's
   * dedupe key, handed to `publishRole` (the listener publish body's `dedupe_key`) and never
   * placed in the directive JSON — the directive shape is part of the unbumped daemon/plugin
   * contract. A nack on any directive but `shutdown` is reported to the controller as
   * `revive-failed`. */
  async controlDirective(
    tree: IssueKey,
    directive: ControlDirective,
    redeliver: { dedupeKey?: string } | false = {}
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
      if (redeliver !== false && "redeliver" in directive) {
        this.deps.publishRole(
          directive.redeliver.topic,
          directive.redeliver.payload,
          redeliver.dedupeKey
        );
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
    if (!this.launchesEnabled) {
      // A resurrection opens a pane. Held for `replayHeldRecoveries()` once boot's probes pass.
      this.heldResurrects.add(treeKey);
      console.error(`[legion] resurrection of ${treeKey} held until the OMP probe passes`);
      return;
    }
    const current = this.resurrecting.get(treeKey);
    if (current) return current;

    const resurrection = this.resurrectDeadTree(treeKey).finally(() => {
      this.resurrecting.delete(treeKey);
    });
    this.resurrecting.set(treeKey, resurrection);
    return resurrection;
  }
  /** Exposes only whether this manager currently owns a root resurrection for resync ownership
   * classification; the promise stays private so no caller can await or replace recovery work. */
  isResurrecting(treeKey: IssueKey): boolean {
    return this.resurrecting.has(treeKey);
  }

  /** Connects the root architect's shim socket on `/process/started`, exactly as `workerReady`
   * does for a phase worker, so the shim's pre-connect backlog drains. The daemon holds no
   * events to replay here: a resurrected root's missed wake is recovered by the overseer
   * catch-up snapshot (`onTreeReady`, called alongside this by the same route), not by this
   * method. */
  async markTreeReady(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (!tree.locator) return;
    await this.clientFor(roleToken(this.deps.state.project, treeKey, "architect"), tree.locator);
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
    this.cancelRootRegistrationDeadline(treeKey);
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

  /**
   * Recovers the role a core-NATS delivery exception names: the controller through
   * `ensureController` for every reason; a tree's root architect and any other role by reason.
   * `receipt_timeout` (the holder is live but its receipt missed the listener's window): a
   * process that probes alive — the root's pane; a worker's cached shim client or its pane
   * (`workerAlive`) — is logged once, naming the role token and event id, and nothing else
   * happens: the holder is slow, not gone, and re-sending would only hand a busy model a second
   * copy. A dead one takes the existing path for every reason: the root is resurrected, any other
   * role goes through `resumeWorker`. `delivery_failed` and `no_holder` on the root architect keep
   * the `reclaim-architect` directive and re-send, but bounded (`resendToRootArchitect`); on any
   * other role, `resumeWorker` as before. This is LEGION-101's contract with the listener
   * (LEGION-108) and the plugin (LEGION-109), and it supersedes LEGION-103's daemon stopgap, which
   * read every `delivery_failed` on an alive holder as a late receipt and dropped it: once the
   * listener reports a late receipt as `receipt_timeout`, a `delivery_failed` is a forward that
   * never happened and deserves the bounded re-send; before that, the pause before the first copy
   * and the cap bound a slow holder's copies the same way. The exception lane has no redelivery
   * -- core NATS has no nak, and the event pump only records a rejected handler in memory,
   * surfaced at shutdown -- so every failure past parsing the token (a liveness probe the runtime
   * could not complete, a recovery that failed past it, a token naming an issue no tree records)
   * is logged here, naming the role token, and nothing more is done with it: the retry is the
   * resync backstop (a confirmed root), the registration deadline (an unconfirmed one), or the
   * next controller-bound effect or exception. What each failing step left behind is that step's
   * own business (a probe
   * throw clears nothing; a failed `spawnRoot` has already done its own rollback).
   */
  async handleException(exception: ExceptionInfo): Promise<void> {
    const parsed = parseRoleToken(this.deps.state.project, exception.roleToken);
    if (!parsed) return;
    try {
      if ("controller" in parsed) {
        await this.ensureController();
        return;
      }
      const root = this.rootForIssue(parsed.issue);
      if (!root) throw new Error(`No Legion tree records issue ${parsed.issue}`);
      const isRoot = parsed.role === "architect" && parsed.issue === root;
      if (exception.reason === "receipt_timeout") {
        const alive = isRoot
          ? (await this.probe(root)) === "alive"
          : await this.workerAlive(exception.roleToken);
        if (alive) {
          console.error(
            `[legion] ${exception.roleToken}: receipt_timeout for event ${exception.original.eventId} on a live process; the holder is slow, not gone — nothing re-sent`
          );
          return;
        }
        if (isRoot) await this.resurrect(root);
        else await this.resumeWorker(root, parsed.issue, parsed.role);
        return;
      }
      if (isRoot) {
        await this.resendToRootArchitect(root, exception);
        return;
      }
      await this.resumeWorker(root, parsed.issue, parsed.role);
    } catch (error) {
      console.error(
        `[legion] failed to recover ${exception.roleToken} after a delivery exception; this lane has no redelivery -- the resync backstop, the registration deadline, or the next exception retries:`,
        error
      );
    }
  }

  /** Whether a phase worker or sub-architect counts as alive for a `receipt_timeout`: its claim
   * has a locator and either a cached shim client (a closed one is evicted by
   * `onWorkerClientClosed`) or a pane that probes alive. The pane probe is a `list-panes` plus
   * `/proc` start-ticks check with no side effects; dialing the socket instead would cache a
   * client as a side effect of a log-only decision. A probe the runtime cannot complete throws
   * through to `handleException`'s catch (logged once, nothing cleared). */
  private async workerAlive(token: string): Promise<boolean> {
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || !claim.locator) return false;
    if (this.workerClients.has(token)) return true;
    return (await this.probeLocator(claim.locator, token)).status === "alive";
  }

  /**
   * The `delivery_failed`/`no_holder` recovery for a root architect: a dead root is resurrected at
   * once; an alive one is told to reclaim its role and redelivered the failed message — but
   * bounded by `resendLedger` (LEGION-101: before this, every exception re-sent at once and
   * without limit, so a holder busy for a minute turned one message into dozens of paid turns).
   * The chain is the message itself (`resendChainKey`); every re-send, the first included, waits
   * its pause (`RESEND_PAUSES_MS`) so a busy holder's turn can end, and the root is probed again
   * after it — a root that died meanwhile is resurrected, never sent a directive it cannot
   * acknowledge (a nack would publish a misleading `revive-failed`). An exception for a chain
   * whose pause is still running is a duplicate of the failure that started it, not the failure of
   * the pending copy, and is dropped uncounted; the cap drops the entry with one line and sends
   * nothing — the resync backstop and the role's next catch-up recover the holder. The re-sent
   * copy carries the triggering exception's dedupe key to `publishRole` (the listener publish
   * body's `dedupe_key`) so a LEGION-108 listener stamps it on the envelope and the plugin's
   * dedupe drops the copy as already seen; the key never enters the directive JSON, whose shape
   * is unchanged. The pause is cancelled by `dispose()` (`resendWaits`), after which the wait's
   * expiry sends nothing. Every line here names the role token, the event id, and the dedupe key
   * — never the payload.
   */
  private async resendToRootArchitect(root: IssueKey, exception: ExceptionInfo): Promise<void> {
    const token = exception.roleToken;
    // Destructured, never assigned whole: `dedupeKey` must not leak into the directive JSON.
    const { topic, payload, eventId, dedupeKey } = exception.original;
    if ((await this.probe(root)) !== "alive") {
      await this.resurrect(root);
      return;
    }
    const key = resendChainKey(exception.original);
    const decision = this.resendLedger.claim(key);
    const keyLabel = dedupeKey ?? "none";
    if (decision.kind === "capped") {
      console.error(
        `[legion] ${token}: re-send cap reached for event ${eventId} (dedupe key ${keyLabel}) after ${decision.attempts} re-sends; dropping it — the resync backstop and the role's next catch-up recover the holder`
      );
      return;
    }
    if (decision.kind === "in-flight") {
      console.error(
        `[legion] ${token}: event ${eventId} (dedupe key ${keyLabel}) arrived while its re-send is pending; dropped without counting`
      );
      return;
    }
    console.error(
      `[legion] ${token}: re-sending event ${eventId} (dedupe key ${keyLabel}) — attempt ${decision.attempt} of ${MAX_RESENDS} after ${decision.pauseMs / 1000}s`
    );
    const { timedOut, cancel } = boundedWait(decision.pauseMs, this.deps.sleep);
    const wait = { cancel };
    this.resendWaits.set(key, wait);
    await timedOut;
    // A cancelled wait still resolves (see `resendWaits`); only the current arm acts.
    if (this.disposed || this.resendWaits.get(key) !== wait) return;
    this.resendWaits.delete(key);
    this.resendLedger.settle(key);
    if ((await this.probe(root)) !== "alive") {
      await this.resurrect(root);
      return;
    }
    await this.controlDirective(
      root,
      { type: "reclaim-architect", issue: root, redeliver: { topic, payload, eventId } },
      { dedupeKey }
    );
  }

  /** Recovers a no-holder role with its state-derived catch-up. Root architects are deliberately
   * left to the resync probe; `resumeWorker` records that decision. */
  async recoverRole(token: string): Promise<void> {
    const parsed = parseRoleToken(this.deps.state.project, token);
    if (!parsed) return;
    if ("controller" in parsed) {
      await this.ensureController();
      return;
    }
    const root = this.rootForIssue(parsed.issue);
    if (!root) return;
    await this.resumeWorker(root, parsed.issue, parsed.role);
  }

  /**
   * Recovers a role's missed wake by probing its own worker locator (never the root's) and, if
   * dead, resuming the same agent through the existing `deliverToWorker` resume path (`--resume`,
   * never fresh) with a state-derived catch-up as its prompt instead of the raw missed event —
   * shared by a role-lane delivery exception, the durable lane's `onUndeliverable` 404, and a
   * dead-launch retry. A role with no claim, or a claim with neither a locator nor a resumable
   * identity (`resumeSessionFile`, or its locator's own `ompSessionFile`), was never spawned or
   * has nothing left to resume: there is nothing to recover, so this is a no-op (its eventual
   * first spawn's own catch-up recovers anything missed meanwhile). Critically, a claim whose
   * *locator* was already cleared but whose `resumeSessionFile` survives — exactly the shape
   * `markWorkerDeadLocked` leaves behind for a confirmed-dead worker — is NOT that case: this is
   * the one scenario this method exists to recover, and `deliverToWorker`'s own resume-session
   * lookup (`claim.locator?.ompSessionFile ?? claim.resumeSessionFile`) already handles it once
   * reached. Publishes `worker-died` to the architect that owns the role's issue only once the
   * resume attempt itself fails at the launch-failure threshold. A sub-architect's catch-up is the
   * `catchup-overseer` snapshot of its own subtree (`overseerCatchup`), a phase worker's the
   * `catchup-worker` (LEGION-86; see the branch below).
   *
   * The catch-up is a `catchup` pending prompt, never an `assignment`: it does not write
   * `phases[issue]`, and it is not sent at all to a phase-worker role that is neither the issue's
   * active phase nor holding a pending prompt -- a finished worker whose wake was misrouted is a
   * bystander until the architect's next `spawn_worker`, so resuming it would only relaunch a
   * process with nothing to do. The same judgement is made again at delivery time
   * (`isBystanderCatchup` in `promoteQueuedWorker` and `workerReady`): a catch-up queued here
   * behind the cap or a boot is dropped if the phase has moved on by the time it would be
   * prompted or relaunched. A sub-architect (`role === "architect"` on a child issue) is exempt,
   * exactly as in `retireIdleWorker`: an architect has no phase of its own -- it is never
   * `phases[issue].phase` once it has spawned a planner -- and it parks by design between wakes
   * for the life of its subtree, so this is its only recovery path. `recoverRole` reaches this
   * method for a root architect too, but the root-role guard returns without a worker resume:
   * its tree's resync probe owns root recovery. A catch-up never replaces a queued architect
   * assignment either: checked here to skip the fetch, and again inside the role's lock in
   * `deliverToWorker` for a `spawn_worker` that lands while the catch-up is being computed. A
   * catch-up queued behind the cap, a busy client, or a boot never publishes `worker-queued`
   * (`WorkerAdmission.publishQueued`): the architect did not ask for it and cannot act on it,
   * and before LEGION-107 every role-lane exception for one busy worker told the architect
   * `worker-queued` again (LEGION-60's eight notices for one queued catch-up).
   */
  async resumeWorker(root: IssueKey, issue: IssueKey, role: LegionRole): Promise<void> {
    const token = roleToken(this.deps.state.project, issue, role);
    if (role === "architect" && issue === root) {
      console.info(
        `[legion] root architect ${token} has no worker resume path; the resync probe owns root recovery`
      );
      return;
    }
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || (!claim.locator && !claim.resumeSessionFile)) {
      console.error(
        `[legion] resumeWorker no-op for ${token}: no claim or resumable identity (never spawned, or already fully retired) - its eventual first spawn's own catch-up recovers anything missed meanwhile`
      );
      return;
    }
    if (claim.pendingAssignment?.kind === "assignment") {
      console.info(
        `[legion] dropping catch-up for ${token}: the architect's assignment is queued and reaches the worker first`
      );
      return;
    }
    if (isBystanderRole(this.deps.state, issue, role) && claim.pendingAssignment === undefined) {
      console.info(
        `[legion] no catch-up for ${token}: ${issue}'s active phase is ${activePhaseLabel(this.deps.state, issue)} and nothing is queued for this role; only spawn_worker resumes a finished worker`
      );
      return;
    }
    // A sub-architect parks between wakes for the life of its subtree and, since LEGION-86, owns
    // its child's phase completions: its revival wake is the same snapshot a root gets
    // (`catchup-overseer`, scoped to its own subtree, carrying the completions recorded for it),
    // not the phase-worker catch-up -- which carries no completions and would lose them.
    const catchup =
      role === "architect"
        ? await overseerCatchup(this.deps.state, issue)
        : await workerCatchup(this.deps.state, issue, role, this.deps.workerCatchup);
    try {
      await this.deliverToWorker(root, issue, role, {
        kind: "catchup",
        task: JSON.stringify(catchup),
        queuedAt: new Date(this.deps.now()).toISOString(),
      });
    } catch (error) {
      console.error(`[legion] failed to resume worker ${issue}/${role}:`, error);
      const failedClaim = this.deps.state.roles[token];
      const failures =
        failedClaim && "issue" in failedClaim ? (failedClaim.launchFailures ?? 0) : 0;
      // `===`, not `>=`: mirrors `launchWorker`'s own threshold publish - fires exactly once, at
      // the tick `failures` first reaches the threshold, never again on a later attempt against
      // the same already-past-threshold claim.
      if (failures === MAX_LAUNCH_FAILURES) {
        this.publishArchitect({ type: "worker-died", issue, role });
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

  /** Awaits every currently in-flight `startRoot` call and every in-flight resurrection
   * (`resurrect`, fired unawaited by a registration-deadline expiry or a resync probe), including
   * ones added while draining, so a shutdown's final save never races a spawn's own `saveState`. */
  async drainSpawns(): Promise<void> {
    while (this.spawns.size > 0 || this.resurrecting.size > 0) {
      await Promise.allSettled([...this.spawns, ...this.resurrecting.values()]);
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
    // Boot's launch hold counts as a full cap: queued trees stay queued until `enableLaunches()`
    // and the `reconcileAdmission()` that follows it.
    if (admission.active.length >= admission.cap || !this.launchesEnabled) {
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

  /** The `run` every workspace-package command goes through: the daemon's runner, with `stderr`
   * normalised to a string the package's `commandFailure` can print. Typed as the package's own
   * `run` so both `provisionIssueWorkspace` and `removeIssueWorkspace` accept it unchanged. */
  private readonly workspaceCommandRunner: ProvisionIssueWorkspaceDeps["run"] = async (
    command,
    options
  ) => {
    const run = this.deps.run;
    if (!run)
      throw new Error("runtime owns workspace cleanup but ProcessManager has no command runner");
    const result = await run(command, options);
    return { ...result, stderr: result.stderr ?? "" };
  };

  /** Removes `issue`'s jj workspace from the shared clone (`removeIssueWorkspace`), under the same
   * runner and slow-command budget as `provisionWorkspace`. Only `removeTreeWorkspaces` calls it. */
  private async removeWorkspace(issue: IssueKey): Promise<RemoveIssueWorkspaceResult> {
    return removeIssueWorkspace(issue, {
      repo: this.deps.config.repo,
      stateDir: this.deps.config.stateDir,
      commandTimeoutMs: this.deps.config.slowCommandTimeoutSeconds * 1000,
      run: this.workspaceCommandRunner,
    });
  }

  /** The workspaces a closing tree leaves behind (LEGION-104): once every process under the tree
   * is stopped, the root's workspace goes unless the root is parked (`backlog`/`icebox`), and each
   * descendant's goes when that issue is `done`; everything else is kept and named. Runs only
   * while the tree record is still `lingering` and the tree holds no admission slot: `admit`
   * reuses the record mid-close for a root a human moved back to `todo` (it sets `active`, takes a
   * slot, and `startRoot`s it — `spawnRoot` then waits on this close, `awaitClosingTrees`), and
   * deleting the directory that root is about to provision into would be the one thing worse than
   * leaving it. The status alone is not proof: a close whose stop failed rewrites the record
   * `lingering` for the sweep's retry over that `active`, while the slot `admit` took stays held
   * — so the retry keys on the slot. Each issue is re-checked at its turn: a child that got its
   * own tree record meanwhile (re-admitted as a root while this tree closed) belongs to that tree
   * now. A removal that fails is logged once with the issue, the directory, and the error, and
   * the close goes on — the leftover is today's state, and the next provisioning of that issue
   * repairs whichever half state it finds (`createWorkspace`). Nothing retries. Sits before
   * `tree.status = "closed"` so a crash mid-removal leaves the tree `lingering` for the sweep to
   * re-run the close and the idempotent removal. */
  private async removeTreeWorkspaces(treeKey: IssueKey, tree: TreeState): Promise<void> {
    if (this.runtime.removesWorkspacesOnTreeClose === false) return;
    const keptEvery =
      tree.status !== "lingering"
        ? `the tree record is "${tree.status}", not lingering`
        : this.deps.state.admission.active.includes(treeKey)
          ? "the tree holds an admission slot"
          : undefined;
    if (keptEvery) {
      console.error(`[legion] kept every workspace of tree ${treeKey} at its close: ${keptEvery}`);
      return;
    }
    const issues = [
      treeKey,
      ...(Object.keys(this.deps.state.issues) as IssueKey[]).filter(
        (issue) => issue !== treeKey && this.rootForIssue(issue) === treeKey
      ),
    ];
    for (const issue of issues) {
      const dir = issueWorkspaceDir(this.deps.config.stateDir, this.deps.config.repo, issue);
      const owner = this.rootForIssue(issue);
      if (issue !== treeKey && owner !== treeKey) {
        console.error(
          `[legion] kept the workspace of ${issue} (${dir}) at the close of tree ${treeKey}: it now belongs to tree ${owner}`
        );
        continue;
      }
      const status = this.deps.state.issues[issue]?.status;
      const kept =
        issue === treeKey ? status === "backlog" || status === "icebox" : status !== "done";
      if (kept) {
        console.error(
          `[legion] kept the workspace of ${issue} (${dir}) at the close of tree ${treeKey}: Dispatch status "${status}"`
        );
        continue;
      }
      try {
        const result = await this.removeWorkspace(issue);
        if (result.removed) {
          console.error(
            `[legion] removed the workspace of ${issue} (${dir}) at the close of tree ${treeKey}: abandoned ${result.abandoned.length} commit(s) nothing else reached`
          );
        }
      } catch (error) {
        console.error(
          `[legion] failed to remove the workspace of ${issue} (${dir}) at the close of tree ${treeKey}:`,
          error
        );
      }
    }
  }

  private async spawnTree(
    tree: TreeState,
    resume: boolean,
    resumeSessionFile?: string
  ): Promise<void> {
    const promptPath = path.join(this.deps.rolePromptsDir, "architect-root.md");
    const priorSessionFile = resume
      ? (resumeSessionFile ?? tree.locator?.ompSessionFile ?? tree.resumeSessionFile)
      : undefined;

    const generation = tree.generation;
    const bootToken = await this.deps.mintBootToken(tree.root, generation);
    const architectToken = roleToken(this.deps.state.project, tree.root, "architect");
    const env = {
      LEGION_TREE: tree.root,
      LEGION_ISSUE: tree.root,
      LEGION_ROLE: "architect",
      LEGION_GENERATION: String(generation),
      LEGION_DAEMON_URL: this.deps.config.daemonUrl,
      LEGION_PROJECT: this.deps.state.project,
      ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
      ENVOY_URL: this.deps.config.envoyUrl,
      LEGION_CONTROL_SUBJECT: `legion.ctl.${sanitizeToken(tree.root)}.${generation}`,
      LEGION_MAX_RECURSION_DEPTH: String(this.deps.config.maxRecursionDepth),
      LEGION_STATE_DIR: this.deps.config.stateDir,
      LEGION_CREDENTIAL_HELPER: this.deps.credentialHelper,
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      ...this.credentialProcessEnvironment(architectToken),
      DISPATCH_URL: this.deps.config.dispatchUrl,
      DISPATCH_TOKEN_FILE: this.dispatchTokenFile,
    };
    const addressingPrompt = `${addressingFragment(
      this.deps.state.project,
      tree.root,
      tree.root,
      "architect"
    )} ${designGateFragment(this.deps.config.gates.design)}`;
    // Cleared before the process starts, not after `runtime.spawn` resolves: the root is a real
    // OMP process outside this event loop, so a fast root's own `/process/started` +
    // `/process/ready` can land before this continuation even runs again (interleaved with the
    // daemon's own HTTP handling while this async chain merely awaits the runtime). Clearing
    // here, before any confirmation for this generation could possibly land, means a
    // confirmation that beats this code back is never silently wiped by a `delete` that runs
    // after it — the deadline itself still arms below only once the launch actually produces a
    // locator (arming this early would let it fire and probe a tree with no process yet). A
    // launch failure below (this call throwing) restores whatever this tree read before this
    // attempt, via `spawnRoot`'s own catch.
    delete tree.readyConfirmedAt;
    // Tracked before the runtime writes it: a name whose write then fails is a harmless no-op
    // `rm --force` at the next prune. The caller (`spawnRoot`) holds the file exempt from pruning
    // for the whole launch.
    this.trackProcessSecrets(architectToken);
    const locator = await this.runtime.spawn("root", {
      issue: tree.root,
      tree: tree.root,
      generation,
      role: "architect",
      env,
      launch: { promptPath, addressingPrompt, resumeSessionFile: priorSessionFile },
      secrets: { LEGION_BOOT_TOKEN: bootToken, ...this.sharedProcessSecrets() },
    });
    // A newer `spawnRoot` (generation bump) may already have run and finished for this exact
    // tree while this launch was still blocked in `runtime.spawn` -- a park releasing the
    // admission slot followed by an immediate re-admission starts a fresh generation without
    // waiting for the older one to finish. Retire only this stale process and touch nothing else:
    // the newer generation already owns `tree.locator`/`tree.status` and any status write, and
    // writing over them here would silently replace a live root with this older one and
    // re-issue a stale `in_progress` PATCH.
    const treeReplaced = this.deps.state.trees[tree.root] !== tree;
    if (!treeReplaced && tree.generation !== generation) {
      try {
        await this.stopProcessSerialized(
          roleToken(this.deps.state.project, tree.root, "architect"),
          locator,
          this.workerStopTimeoutMs
        );
      } catch (error) {
        console.error(
          `[legion] failed to retire a stale-generation root process for ${tree.root}:`,
          locator,
          error
        );
      }
      return;
    }
    // Abort only for a genuine human park/close, never for this daemon's own `in_progress` echo
    // (including a delayed one landing mid-launch) or a resurrect in flight -- both of those
    // MUST proceed to a running root. `tree.status` "lingering"/"closed" already reflects a park
    // or close the reducer's own linger effect applied while this launch was in flight (that
    // effect only fires when `tree.status` already read "active" at the time it ran); the issue
    // going `backlog`/`icebox`/`done` covers the window where it can't have fired yet (a
    // resurrect's tree still reads "dead" here, so the reducer saw no "active" tree to linger).
    const issueStatus = this.deps.state.issues[tree.root]?.status;
    const humanParked =
      issueStatus === "backlog" || issueStatus === "icebox" || issueStatus === "done";
    if (treeReplaced || tree.status === "lingering" || tree.status === "closed" || humanParked) {
      if (!treeReplaced) {
        tree.locator = locator;
        if (humanParked && tree.status !== "lingering" && tree.status !== "closed") {
          // The reducer's own park/close effect never ran for this tree (see above), so nothing
          // has released this admission slot or lingered it yet -- replicate `beginLinger`'s own
          // rollback here rather than leave an admitted slot with a doomed process and no locator.
          tree.status = "lingering";
          tree.lingerUntil = new Date(
            this.deps.now() + this.deps.config.lingerHours * HOUR_MS
          ).toISOString();
          this.clearTreePhases(tree.root);
          await this.releaseSlot(tree.root);
        }
      }
      try {
        await this.stopProcessSerialized(
          roleToken(this.deps.state.project, tree.root, "architect"),
          locator,
          this.workerStopTimeoutMs
        );
        if (!treeReplaced) delete tree.locator;
      } catch (error) {
        console.error(
          `[legion] failed to retire stale root process for ${tree.root}:`,
          locator,
          error
        );
        // Mirrors closeTreeLocked's own stop-failure convention: never let a process that might
        // still be alive settle into a terminal tree state (e.g. "closed") nothing ever revisits
        // again. An immediately-expired lingerUntil makes the next periodic sweep retry the stop
        // through the ordinary closeTree path; the locator stays in place (set above) so that
        // retry has something to target.
        if (!treeReplaced) {
          tree.status = "lingering";
          tree.lingerUntil = new Date(this.deps.now()).toISOString();
        }
      }
      return;
    }
    tree.locator = locator;
    // The fresh locator's `ompSessionFile` (written at `/process/started`) is the resume source from
    // here; the copy kept across the cleared pane has done its job.
    delete tree.resumeSessionFile;
    tree.status = "active";
    // Armed only once a real locator exists to probe against -- `readyConfirmedAt` was already
    // cleared before the process started (see above), so a confirmation that raced ahead of this
    // continuation is preserved and this call is a harmless no-op wait: `stillUnconfirmed`
    // checks `readyConfirmedAt` itself before ever probing.
    this.armRootRegistrationDeadline(tree.root, generation);
    // Only a first admission moves the issue to `in_progress`. A resurrection (`resume`) relaunches
    // the same recorded session onto a fresh pane after the lifecycle may already have moved the
    // issue on (`testing`, `needs_review`, `retro`, or a human's `todo`); it writes no status, so
    // later phase completions keep acting on the true one.
    if (!resume) {
      await writeStatus(this.deps.state, this.deps.dispatchClient, tree.root, "in_progress");
    }
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

  /**
   * Connects (or reuses a cached connection) to a role's shim socket. `onIdle` is wired
   * unconditionally for every connection (root architect, controller, phase worker, and
   * sub-architect alike) as a trigger that (a) arms the connection's idle-retire clock
   * (`armIdleRetire`, which judges at expiry whether this token is a retirable phase worker at
   * all) and (b) re-checks the running-worker queue — occupancy itself is always derived fresh
   * by `runningWorkerCount()` from `state.roles` (the root architect and controller are never in
   * `state.roles` at all, tracked separately via `state.trees`/`state.controllerLocator`), not by
   * opting a connection in or out here. The dial itself is the runtime's (`Runtime.connect`, raw
   * and uncached); the negotiate, the cache, and the `closed` wiring below are this manager's,
   * since `onWorkerClientClosed`'s stale-close guard depends on the cache it evicts from.
   */
  private async clientFor(token: string, locator: Locator): Promise<WorkerRpcClient> {
    const existing = this.workerClients.get(token);
    if (existing) return existing;
    const inFlight = this.workerConnections.get(token);
    if (inFlight) return inFlight;
    const connecting = (async () => {
      const client = await this.runtime.connect(locator, this.workerRpcTimeoutMs);
      try {
        await client.negotiate();
      } catch (error) {
        client.close();
        throw error;
      }
      this.workerClients.set(token, client);
      client.onIdle(() => {
        this.armIdleRetire(token, client);
        this.workerAdmission.promoteWorkerQueue();
      });
      // Detached from this connect call on purpose (the caller must not wait on the worker's
      // eventual close) — `.catch` here is not error recovery, it is the only thing standing
      // between an `onWorkerClientClosed` rejection (runtime/saveState failures included) and an
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
   * to kill a live worker, so the claim is left as is. A claim whose ready path never completed
   * (`readyConfirmedAt` still unset) routes its dead verdict through `retireUnconfirmedBoot` —
   * the same retire/count/enqueue-or-give-up accounting the boot watchdog and `reconnectWorkers`
   * use — rather than `markWorkerDead`'s confirmed-worker path, which only clears the locator
   * with no accounting or requeue: without this branch, a socket that dies before ready
   * confirmation left its claim locator-less and unqueued, with nothing left to ever revisit it.
   * A root architect or controller connection has no matching `WorkerRoleClaim` (they are tracked
   * via `state.trees`/`state.controllerLocator`), so this is a no-op for them past cache eviction.
   */
  private async onWorkerClientClosed(token: string, client: WorkerRpcClient): Promise<void> {
    if (this.workerClients.get(token) !== client) return;
    this.workerClients.delete(token);
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || claim.locator === undefined) return;
    const locator = claim.locator;
    const generation = claim.generation;
    const confirmed = claim.readyConfirmedAt !== undefined;
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
    const probe = await probeWorker(() => this.clientFor(token, locator), this.workerRpcTimeoutMs);
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

  private async launchWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    pending: PendingAssignment
  ): Promise<void> {
    const token = roleToken(this.deps.state.project, issue, role);
    const generation = (claim?.generation ?? 0) + 1;
    // Checked here, before any I/O (including spawning a real process): a promotion-triggered
    // launch (WorkerAdmission's drain -> the trackLaunch-wrapped dep in the constructor) has no
    // entry fence of its own the way spawnWorker's decision does -- this is that fence for every
    // caller of launchWorker, direct or promoted, so a launch queued behind a since-closing (or
    // already gone) tree never attempts real workspace/runtime I/O for it. The post-spawn check
    // further down covers the remaining window between this check and the process actually
    // starting.
    if (this.isTreeGone(treeKey, issue)) {
      throw new TreeClosingError(treeKey);
    }
    // Held for the whole launch, released once the fresh claim (and its locator) is in state —
    // or the launch has given up — immediately before the persist that follows. See
    // `holdProcessSecret`.
    const releaseSecret = this.holdProcessSecret(token);
    try {
      const identity = await this.workerIdentityEnv(role);
      const promptPath = path.join(this.deps.rolePromptsDir, `${role}.md`);
      const resumeSessionFile = claim?.locator?.ompSessionFile ?? claim?.resumeSessionFile;

      const bootToken = await this.deps.mintWorkerBootToken(
        treeKey,
        issue,
        role,
        generation,
        claim?.sessionId
      );
      const env = {
        LEGION_TREE: treeKey,
        LEGION_ISSUE: issue,
        LEGION_ROLE: role,
        LEGION_GENERATION: String(generation),
        LEGION_DAEMON_URL: this.deps.config.daemonUrl,
        LEGION_PROJECT: this.deps.state.project,
        LEGION_STATE_DIR: this.deps.config.stateDir,
        LEGION_CREDENTIAL_HELPER: this.deps.credentialHelper,
        ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
        ENVOY_URL: this.deps.config.envoyUrl,
        GIT_CONFIG_COUNT: "0",
        GIT_TERMINAL_PROMPT: "0",
        ...identity,
        ...this.credentialProcessEnvironment(token),
        DISPATCH_URL: this.deps.config.dispatchUrl,
        DISPATCH_TOKEN_FILE: this.dispatchTokenFile,
      };
      const architectIssue = owningArchitect(this.deps.state, issue, role);
      const addressingPrompt = addressingFragment(
        this.deps.state.project,
        architectIssue,
        issue,
        role
      );
      // Tracked before the runtime writes it — see `spawnTree`. The hold above keeps it exempt
      // from pruning for the whole launch.
      this.trackProcessSecrets(token);
      const locator = await this.runtime.spawn("worker", {
        issue,
        tree: treeKey,
        generation,
        role,
        env,
        launch: { promptPath, addressingPrompt, resumeSessionFile },
        secrets: { LEGION_BOOT_TOKEN: bootToken, ...this.sharedProcessSecrets() },
      });
      // `closeTree` may have started tearing down this tree while this launch's I/O was in
      // flight above -- its fixed-point stop loop can only stop locators it could already see
      // when it ran (mitigated further by `inFlightLaunches`, but the window between spawning
      // the process and this check is still real), so writing a fresh claim now would leave this
      // brand-new process live and completely untracked otherwise. Report the closing error
      // directly, not via the launch-failure path below: this was never a launch failure, and
      // counting it as one would eventually mark the role `launch-failed` for a tree that is
      // simply gone. `WorkerAdmission`'s own "tree deleted" concern -- `rootForIssue` no longer
      // resolving to `treeKey`, or the tree's own status already `"closed"` -- is checked
      // alongside `closingTrees` below: any one of the three means nothing durable should be
      // written for this launch.
      const freshLocator: Locator = {
        ...locator,
        ...(resumeSessionFile ? { ompSessionFile: resumeSessionFile } : {}),
      };
      const freshClaim: WorkerRoleClaim = {
        issue,
        role,
        // sessionId deliberately not carried over: it stays unset until /worker/started
        // registers this generation's session, so a concurrent spawnWorker call during the boot
        // window sees an unregistered claim rather than racing a stale one (both for a fresh
        // spawn and a resume, where OMP reports the same session id it had before).
        ...(claim?.agentId ? { agentId: claim.agentId } : {}),
        // Carried over, never reset by a mere relaunch: a boot the watchdog never sees complete
        // its ready path must accumulate across repeated retries so the threshold below is ever
        // reachable, even when every retry successfully spawns a process but none ever completes
        // `/worker/ready`. `/worker/started`'s own registration success clears it — that is the
        // recovery signal for launch accounting, not merely respawning.
        launchFailures: claim?.launchFailures ?? 0,
        // Carried over like `launchFailures`: this relaunch IS a prompt-retire cycle; dropping it
        // here would make `MAX_PROMPT_RETIRES` unreachable (LEGION-93). Deleted only by a started
        // turn (`commitPromptDelivery`).
        ...(claim?.promptRetires ? { promptRetires: claim.promptRetires } : {}),
        generation,
        pendingAssignment: pending,
        bootTokenHash: secretHash(bootToken).toString("hex"),
        ...(claim?.sessionId ? { expectedSessionId: claim.sessionId } : {}),
        locator: freshLocator,
        // resumeSessionFile deliberately dropped: a fresh locator now carries its own
        // ompSessionFile, so the standalone fallback field is stale.
      };
      if (this.isTreeGone(treeKey, issue)) {
        // Write the locator BEFORE attempting to retire it: a `StopFailed` here must never
        // discard the only durable record of a process that might still be alive. Only a
        // successful retire clears it; `closeTree`'s own fixed-point loop or the periodic sweep
        // retries the stop from whatever this leaves behind on failure.
        this.deps.state.roles[token] = freshClaim;
        releaseSecret();
        await this.persist();
        await this.retireWorkerLocator(token, freshLocator);
        delete this.deps.state.roles[token];
        await this.persist();
        throw new TreeClosingError(treeKey);
      }

      this.deps.state.roles[token] = freshClaim;
      releaseSecret();
      const queueIndex = this.deps.state.workerAdmission.queue.indexOf(token);
      if (queueIndex !== -1) this.deps.state.workerAdmission.queue.splice(queueIndex, 1);
      // Persists the new claim's locator before this call resolves and the caller (`launchOrQueue`/
      // `promoteQueuedWorker`'s own finally) releases the reservation: a crash between spawning
      // this process and this point would otherwise leave a real, running worker-shim process the
      // daemon has completely forgotten about on disk (`reconnectWorkers` only ever reconnects
      // to locators it can read back from state, and the runtime's orphan sweep only reaps what
      // it can tell apart from a recorded process's own grouping -- under tmux a split-pane
      // worker shares its window with the tree's other recognized occupants, so the window itself
      // stays "known" and the orphaned pane inside it is never swept). Holding the reservation
      // through this save is the honest cost
      // of that guarantee: a concurrent admission decision waits slightly longer to see this
      // slot as durably occupied, instead of trusting an in-memory claim that might still
      // vanish on a crash.
      try {
        await this.persist();
      } catch (saveError) {
        // The process already exists at this point (spawned, locator and `pendingAssignment`
        // already written above) -- a failure here is a durable-state persistence failure, not
        // a launch failure: the launch itself succeeded. Never re-queues or rotates this
        // token (it already has a live process; re-queuing it would launch a second process for
        // the same issue/role the next time it is promoted) and never bumps `launchFailures`
        // (this is not what that counter tracks -- see the outer `catch` below, which only ever
        // runs for a failure *before* a process exists). Retries the persist exactly once more;
        // if that also fails, logs it and leaves the in-memory claim authoritative -- never
        // rethrown, since the launch genuinely succeeded regardless of whether this save did.
        // The process's own `/worker/started` -> `/worker/ready` handshake calls back into the
        // daemon independent of this save and persists normally on its own next success.
        console.error(
          `[legion] failed to persist ${token}'s locator after a successful launch (process is live regardless):`,
          saveError
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
      this.bootWatchdog.arm(treeKey, issue, role, token, locator, generation);
    } catch (error) {
      // Nothing durable references the file (the locator was never stored, or was stored and
      // already retired above): this path's persist reaps it.
      releaseSecret();
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
        this.publishArchitect({ type: "launch-failed", issue, role, failures });
      }
      await this.persist();
      throw error;
    }
  }

  private async spawnController(controllerSecret: string): Promise<void> {
    const promptPath = path.join(this.deps.rolePromptsDir, "controller-root.md");
    const token = controllerToken(this.deps.state.project);
    // Held until the locator is in state (or the launch failed) — see `holdProcessSecret`.
    const releaseSecret = this.holdProcessSecret(token);
    try {
      // Tracked before the runtime writes it — see `spawnTree`.
      this.trackProcessSecrets(token);
      const env = {
        LEGION_CONTROLLER: "1",
        LEGION_ROLE: "controller",
        LEGION_DAEMON_URL: this.deps.config.daemonUrl,
        LEGION_PROJECT: this.deps.state.project,
        ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
        ENVOY_URL: this.deps.config.envoyUrl,
        ...this.credentialProcessEnvironment(token),
        DISPATCH_URL: this.deps.config.dispatchUrl,
        DISPATCH_TOKEN_FILE: this.dispatchTokenFile,
      };
      this.deps.state.controllerLocator = await this.runtime.spawn("controller", {
        role: "controller",
        env,
        launch: { promptPath },
        secrets: { LEGION_CONTROLLER_SECRET: controllerSecret, ...this.sharedProcessSecrets() },
      });
    } finally {
      releaseSecret();
    }
    await this.persist();
  }

  /** Connects the controller's shim socket on `/controller/ready`, exactly as `markTreeReady`
   * does for the root architect, so the shim's pre-connect backlog drains. Best-effort: a shim
   * connect failure (listener race, stale socket, RPC timeout) must never block
   * `/controller/ready` from accepting the role — the daemon holds no events to replay here
   * either; the connection is retried on the next `spawnWorker`/`workerReady`/reconnect attempt
   * that touches this socket.
   */
  async markControllerReady(): Promise<void> {
    const locator = this.deps.state.controllerLocator;
    if (!locator) return;
    try {
      await this.clientFor(controllerToken(this.deps.state.project), locator);
    } catch (error) {
      console.error("[legion] failed to connect controller shim socket on ready:", error);
    }
  }

  /** Revokes a claim's session capability (a no-op if it never had one — never spawned, or
   * already revoked) the instant its process is retired or torn down, so a stale process can
   * never keep minting grants once the daemon has stopped trusting it. The single chokepoint
   * every path that retires or deletes a role's claim goes through: `removeTreeProcess` (the
   * root architect), `retireWorkerLocator` (a worker, via `markWorkerDeadLocked` and every
   * other retirement), and `closeTree` (root and every worker, on tree shutdown). */
  private revokeRoleClaim(claim: WorkerRoleClaim | undefined): void {
    if (claim?.sessionId) this.deps.revokeSessionCapability(claim.sessionId);
  }

  /** Called only once the recorded process has probed dead (`verdict`): gone, or present but
   * not the recorded process -- a reissued pane id, or a legacy locator with no identity. Routed
   * through `stopProcess` anyway: the graceful shutdown goes over the root's own role-scoped
   * socket, so a still-live legacy root exits cleanly before its session is resumed elsewhere,
   * while the runtime's destroy step is refused (`refuseKill`) for a process that is not the
   * recorded one -- the caller's verdict already decided and logged that, and a stranger's
   * process is never killed. The locator is cleared only after the stop settles, exactly like
   * `closeTreeLocked` and `controllerAlive`: while the graceful ask waits, the root's window must
   * stay in `recordedLocators()`, or a linger-sweep `reconcileOrphans` tick could reap an idle
   * legacy root mid-shutdown instead of letting it exit cleanly. Best-effort: a `StopFailed` here
   * is logged and swallowed rather than blocking `resurrectDeadTree` — the probe already decided
   * this locator is dead, so a stray cleanup failure is never a reason to refuse resurrecting the
   * tree onto a fresh process. */
  private async removeTreeProcess(
    tree: TreeState,
    verdict: Extract<ProbeResult, { status: "dead" }>
  ): Promise<void> {
    const architectToken = roleToken(this.deps.state.project, tree.root, "architect");
    const architectClaim = this.deps.state.roles[architectToken];
    this.revokeRoleClaim(architectClaim && "issue" in architectClaim ? architectClaim : undefined);
    const locator = tree.locator;
    if (!locator) return;
    this.stoppingForRelaunch.add(tree.root);
    try {
      await this.stopProcessSerialized(architectToken, locator, this.workerStopTimeoutMs, {
        skipGraceful: verdict.reason === "gone",
        refuseKill: verdict.reason === "not-recorded-process",
      });
    } catch (error) {
      console.error(
        `[legion] failed to clean up ${tree.root}'s dead process before resurrection:`,
        error
      );
    } finally {
      this.stoppingForRelaunch.delete(tree.root);
      if (tree.locator === locator) delete tree.locator;
    }
  }

  /**
   * The one graceful-shutdown implementation every stop path funnels through. When this manager
   * already holds a cached client for `token`, the shim is asked to shut down over it (a
   * `{type:"shutdown"}` frame, then up to `timeoutMs` for the shim's own socket to close — see
   * `awaitShutdown`) and the cache entry is evicted; only a clean close is a confirmed stop. That
   * eviction happens here, between the race and `close()`, because it is what makes the cached
   * client's own `closed` reaction (registered at connect time in `clientFor`) a no-op for a
   * client this manager is deliberately closing. Everything past that — the runtime's own
   * graceful attempt when nothing was cached, and the kill itself — is `Runtime.stop`'s;
   * `skipGraceful` (the caller already confirmed nothing live is there to ask) skips straight to
   * it. A `ProcessStopFailed` from the runtime (it could not confirm the process stopped) is
   * rethrown as `StopFailed` for this token; every other error passes through unchanged. The
   * caller must never treat the process as stopped, or its claim/locator as safe to delete, when
   * it cannot confirm that.
   */
  private async stopProcess(
    token: string,
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void> {
    const cached = options?.skipGraceful ? undefined : this.workerClients.get(token);
    if (cached) {
      const confirmed = await awaitShutdown(cached, timeoutMs, this.deps.sleep);
      if (this.workerClients.get(token) === cached) this.workerClients.delete(token);
      if (confirmed) return;
      cached.close();
      options = { ...options, skipGraceful: true };
    }
    try {
      await this.runtime.stop(locator, timeoutMs, options);
    } catch (error) {
      if (error instanceof ProcessStopFailed) throw new StopFailed(token, error.message);
      throw error;
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
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void> {
    return this.workerAdmission.mutateClaim(token, () =>
      this.stopProcess(token, locator, timeoutMs, options)
    );
  }

  /** Before a worker's locator is replaced or cleared during `spawnWorker`'s own launch decision,
   * `markWorkerDeadLocked`'s dead-socket retirement, or a full tree `closeTree` (via
   * `stopProcessSerialized`), retires whatever process it may still be running -- a shorter timeout
   * than a full tree shutdown when called for a single stale worker, not a whole tree. May throw
   * `StopFailed`; callers never treat a locator as safe to clear or a replacement as safe to
   * launch when it does. */
  private async retireWorkerLocator(token: string, locator: Locator): Promise<void> {
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

  /** The daemon-configured `worker_rpc_timeout_seconds` (default 5), in milliseconds -- the
   * timeout every runtime dial and `probeWorker` call in this file uses for a single
   * worker RPC request (`negotiate_protocol`/`get_state`), including the background
   * connect `markTreeReady`/`workerReady`/`markControllerReady` kick off after
   * `/process/ready`/`/worker/ready`/`/controller/ready` already responded. */
  private get workerRpcTimeoutMs(): number {
    return this.deps.config.workerRpcTimeoutSeconds * 1000;
  }

  /** Probes the controller's recorded locator for liveness through the runtime, clearing the
   * locator on a dead verdict (a dead controller's record must never keep `ensureController`
   * from spawning a fresh one). A controller that is present but not the recorded process -- a
   * pane id reissued to another role's OMP, or a legacy record with no identity to verify -- is
   * logged once with both identities and asked to shut down over its own role-scoped socket
   * first (`stopProcess`, its destroy step refused on this same verdict), so a still-live legacy
   * controller exits cleanly instead of running beside its replacement until the sweep reaps
   * it, and a stranger's process is never killed. An `unknown` verdict is refused exactly as
   * `probe` refuses it for a tree — never treated as dead, which would delete the record of a
   * possibly-live controller and spawn a second one beside it. */
  private async controllerAlive(): Promise<boolean> {
    const locator = this.deps.state.controllerLocator;
    if (!locator) return false;
    const result = await this.probeLocator(locator, "the controller");
    if (result.status === "alive") return true;
    if (result.reason === "not-recorded-process") {
      console.error(`[legion] treating the controller as dead: ${result.detail}`);
      try {
        await this.stopProcess(
          controllerToken(this.deps.state.project),
          locator,
          this.workerStopTimeoutMs,
          { refuseKill: true }
        );
      } catch (error) {
        console.error(
          "[legion] failed to stop the controller's recorded process before replacing it:",
          error
        );
      }
    }
    delete this.deps.state.controllerLocator;
    return false;
  }

  /** Resurrects `treeKey` onto a fresh process unless its recorded one still probes alive. The
   * probe is taken afresh here -- a caller's earlier verdict may be stale by now (a held
   * resurrection replayed after the launch hold, a root that came back between two probes) --
   * but silently: the caller's own decision point (`probeTree`) has already logged why the root
   * is being treated as dead, and this verdict is handed down to `removeTreeProcess` so the stop
   * does not decide -- or log -- the same thing again. A probe that cannot complete throws
   * through to the caller, which re-arms or retries (`escalateOrRetryUnconfirmedRoot`, the
   * resync tick, `handleException`'s log) without touching the tree. */
  private async resurrectDeadTree(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.status === "queued") {
      // Already waiting for a slot (the at-cap branch below, or `admit`): it holds no process,
      // and the promotion sweep starts it -- a second wake must not open a pane past the cap or
      // queue it twice.
      console.error(
        `[legion] not resurrecting ${treeKey}: its tree is queued for an admission slot and holds no process; the promotion sweep starts it`
      );
      return;
    }
    const verdict = tree.locator
      ? await this.probeLocator(tree.locator, treeKey)
      : ({ status: "dead", reason: "gone" } satisfies ProbeResult);
    if (verdict.status === "alive") return;
    const resumeSessionFile = tree.locator?.ompSessionFile ?? tree.resumeSessionFile;
    await this.removeTreeProcess(tree, verdict);
    tree.status = "dead";
    if (resumeSessionFile !== undefined) tree.resumeSessionFile = resumeSessionFile;
    // Persist the recoverable handoff before the next await reaches workspace provisioning or the
    // root spawn. A daemon crash here leaves a dead tree holding its slot with the same-agent
    // session file, which boot and resync resume rather than treating as a live root.
    await this.persist();
    // The slot is guaranteed before anything spawns (LEGION-83): a tree the daemon resumes keeps
    // the slot it holds; one that lost it (its own exit released it, and the sweep gave it away)
    // takes a free one, or waits at the HEAD of the queue -- it was already admitted, ahead of
    // never-started issues -- with its session file kept, and the sweep promotes it with
    // `--resume` once a slot frees. `spawnTree`'s `status = "active"` is therefore reached only by
    // a tree that holds a slot.
    const admission = this.deps.state.admission;
    if (!admission.active.includes(treeKey)) {
      if (admission.active.length < admission.cap && this.launchesEnabled) {
        admission.active.push(treeKey);
        console.error(
          `[legion] ${treeKey}'s root is resumed without an admission slot; taking a free one (${admission.active.length}/${admission.cap})`
        );
      } else {
        tree.status = "queued";
        if (resumeSessionFile !== undefined) tree.resumeSessionFile = resumeSessionFile;
        const queueIndex = admission.queue.indexOf(treeKey);
        if (queueIndex !== -1) admission.queue.splice(queueIndex, 1);
        admission.queue.unshift(treeKey);
        await this.persist();
        console.error(
          `[legion] ${treeKey}'s root is resumed but admission is full (${admission.active.length}/${admission.cap}); queued at the head of admission.queue with its session file kept, promoted with --resume when a slot frees`
        );
        return;
      }
    }
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

  private publishController(
    payload:
      | { type: "revive-failed"; issue: IssueKey; role: LegionRole }
      | { type: "launch-failed"; issue: IssueKey; failures: number }
  ): void {
    this.deps.publishRole(
      roleTopic(controllerToken(this.deps.state.project)),
      JSON.stringify(payload)
    );
  }

  /** The commit identity a phase worker's pane carries for its whole life: the GitHub App its role
   * acts as (`appRoleForLegionRole` — the mapping `/worker/started`'s lease and `legion gh` use),
   * read from the token manager's lease *before* the pane opens, so no worker ever commits without
   * one and a token-manager failure is a launch failure, never a pane with a generic author.
   * Environment, not `jj config`: every issue workspace is a workspace of the one shared clone, and
   * jj's repository-scoped config is a single file for all of them — a worker that wrote its
   * identity there set the author and committer for every other tree's commits (LEGION-44). Root
   * architect and controller panes never commit and carry none of these. */
  private async workerIdentityEnv(role: LegionRole): Promise<Record<string, string>> {
    return gitIdentityEnv(await this.workerGitIdentity(role));
  }

  /** The jj half of the same lease identity, for `Runtime.adoptWorkingCopy`: `jj metaedit` reads
   * `JJ_USER`/`JJ_EMAIL` and nothing else. */
  private async workerJjIdentity(role: LegionRole): Promise<JjIdentity> {
    const identity = await this.workerGitIdentity(role);
    return { jjUser: identity.name, jjEmail: identity.email };
  }

  private async workerGitIdentity(role: LegionRole): Promise<{ name: string; email: string }> {
    const [owner] = this.deps.config.repo.split("/") as [string, string];
    const lease = await this.deps.workerCatchup.tokenManager.getToken(
      appRoleForLegionRole(role),
      owner
    );
    return lease.gitIdentity;
  }

  /** The credential environment a root, worker, or controller pane carries for life — never per
   * command. `LEGION_GRANT_FILE` names the 0600 file under `<state_dir>/secrets` the pi-envoy
   * extension writes each bash command's freshly minted grant to (and `legion credential`,
   * `legion gh`, and `legion handoff complete` read ahead of `LEGION_GRANT`); the daemon only names
   * it here and prunes it with the pane's boot-token file (`trackProcessSecrets`). PATH puts
   * `<state_dir>/worker-bin` (the `gh` shim `index.ts` installs at startup) first exactly once:
   * `processPath` is the daemon's resolved PATH with every inherited `worker-bin` entry already
   * stripped by `resolveDaemonEnvironment` (a daemon started from inside a Legion pane inherits
   * that pane's shim-first PATH through `mise env`), so the daemon's own `gh` is never the shim and
   * the prefix added here is the only one. How PATH reaches the process is the runtime's business:
   * this record is runtime-neutral, and the tmux runtime delivers PATH through the pane's shell
   * command rather than as a `-e` pair, since tmux replaces a pane's PATH from the unattached
   * client's environment after copying the `-e` pairs (LEGION-91) — a Kubernetes runtime would set
   * it on the pod verbatim.
   * `GH_CONFIG_DIR` isolates a raw `gh` from any operator login state, and the emptied
   * `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` (tmux renders `''` as `-e KEY=`, an empty variable) keep an
   * ambient token or host from shadowing the per-call one `legion gh` redeems. */
  private credentialProcessEnvironment(token: string): Record<string, string> {
    const stateDir = this.deps.config.stateDir;
    return {
      PATH: `${workerBinDir(stateDir)}${path.delimiter}${this.deps.processPath}`,
      GH_CONFIG_DIR: path.join(stateDir, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_GRANT_FILE: secretFilePath(stateDir, grantSecretName(token)),
    };
  }

  /** The secrets every process receives beyond its own boot token or controller secret, name →
   * value, from config: `ENVOY_TOKEN` when the daemon has an Envoy bearer (a listener bound off
   * loopback requires one from every publisher, the pi-envoy extension included). Delivered
   * through `SpawnSpec.secrets` — never `env` — so each runtime writes it as a `<NAME>_FILE`
   * pointer exactly like the boot token. */
  private sharedProcessSecrets(): Partial<Record<SharedSecretName, string>> {
    const envoyToken = this.deps.config.envoyToken;
    return envoyToken === undefined ? {} : { ENVOY_TOKEN: envoyToken };
  }

  /** Tracks every file a pane's role token names (`processSecretNames`: its own secret, one file
   * per `SHARED_SECRET_NAMES` entry whether or not this configuration delivers it, its grant file)
   * so the steady-state prune reaps them together once the pane's locator clears. */
  private trackProcessSecrets(token: string): void {
    for (const name of processSecretNames(token)) this.processSecretFiles.add(name);
  }

  /** Holds every `<state_dir>/secrets` file the role token names (`processSecretNames`: its
   * boot-token/controller-secret file and its grant file) exempt from pruning while a launch for
   * that role is in
   * flight. The launch owner (`spawnRoot`, `launchWorker`, `spawnController`) takes the hold
   * before anything is written and releases it only once the process's locator is stored in
   * state — or the launch has given up — immediately before the persist that follows, so that
   * persist's prune sees the file referenced by a live locator, or reaps it. Refcounted, not a
   * flag: two launches of one role can overlap (a park-then-re-admit starts a newer root
   * generation while the older one is still blocked in the runtime, see `spawnTree`), and the
   * older one settling must never expose the file the newer process is about to read. The
   * returned release is idempotent. */
  private holdProcessSecret(token: string): () => void {
    this.launchingSecrets.set(token, (this.launchingSecrets.get(token) ?? 0) + 1);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const remaining = (this.launchingSecrets.get(token) ?? 1) - 1;
      if (remaining > 0) this.launchingSecrets.set(token, remaining);
      else this.launchingSecrets.delete(token);
    };
  }

  /** Every secret file some live process still needs: the shared Dispatch bearer, and — for each
   * tree root with a locator, each worker claim with a locator, the controller when it has a
   * locator, and every launch currently in flight — every file its role token names
   * (`processSecretNames`). */
  private liveSecretFiles(): Set<string> {
    const project = this.deps.state.project;
    const tokens = [...this.launchingSecrets.keys()];
    if (this.deps.state.controllerLocator) tokens.push(controllerToken(project));
    for (const tree of Object.values(this.deps.state.trees)) {
      if (tree.locator) tokens.push(roleToken(project, tree.root, "architect"));
    }
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && claim.locator) tokens.push(token);
    }
    return new Set<string>([DISPATCH_TOKEN_SECRET, ...tokens.flatMap(processSecretNames)]);
  }

  /** The boot-time half of secret-file hygiene (`index.ts`): lists `<state_dir>/secrets` and
   * removes everything no live locator references — files a previous daemon process left behind
   * between clearing a locator and its save's prune. Every name a live locator references joins
   * `processSecretFiles` — whether the file exists yet or not (a grant file is written by the
   * extension later in the pane's life; an absent name is a no-op `rm --force`) — so the
   * steady-state prune reaps it the moment that locator clears, exactly like a file written on
   * this manager's behalf: a process's secret files live as long as its locator, across daemon
   * restarts too. Best-effort and never throws. */
  async pruneSecretFiles(): Promise<void> {
    const live = this.liveSecretFiles();
    for (const name of live) {
      if (name !== DISPATCH_TOKEN_SECRET) this.processSecretFiles.add(name);
    }
    try {
      await pruneSecretFiles(this.deps.config.stateDir, live);
    } catch (error) {
      console.error("[legion] failed to prune process secret files:", error);
    }
  }

  /** The steady-state half, run by every `persist()`: removes exactly the files this process
   * wrote (`processSecretFiles`) that no live locator references any more — no directory listing,
   * so a save that clears no locator costs no extra I/O. A file whose removal fails stays tracked
   * and is retried on the next persist; the failure is logged, never thrown, because hygiene must
   * never turn a successful save into a failed one. */
  private async pruneWrittenSecretFiles(): Promise<void> {
    const live = this.liveSecretFiles();
    const stale = [...this.processSecretFiles].filter((name) => !live.has(name));
    if (stale.length === 0) return;
    await Promise.all(
      stale.map(async (name) => {
        try {
          await rm(secretFilePath(this.deps.config.stateDir, name), { force: true });
          this.processSecretFiles.delete(name);
        } catch (error) {
          console.error(`[legion] failed to remove process secret file ${name}:`, error);
        }
      })
    );
  }

  private async persist(): Promise<void> {
    await this.deps.saveState();
    await this.pruneWrittenSecretFiles();
  }
}
