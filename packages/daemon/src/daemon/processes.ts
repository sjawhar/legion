import { createHash } from "node:crypto";
import { mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  type LegionRole,
  parseIssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
  type SpawnWorkerResponse,
  sanitizeToken,
} from "@legion/contracts";
import { provisionIssueWorkspace, type WorkspaceSpec } from "@legion/workspace";
import type { CommandRunnerOptions } from "../state/fetch";
import { rootForIssue as resolveRootForIssue } from "./api/context";
import { type WorkerCatchupDeps, workerCatchup } from "./catchup";
import type { DaemonConfig } from "./config";
import type { LegionState, TreeState, WorkerLocator, WorkerRoleClaim } from "./legion-state";
import * as tmux from "./tmux";
import type { WorkerRpcClient } from "./worker-rpc";

const HOUR_MS = 60 * 60 * 1000;

const MAX_LAUNCH_FAILURES = 3;
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
  | {
      type: "revive-worker";
      issue: IssueKey;
      role: LegionRole;
      agentId: string;
      parentSessionFile: string;
      redeliver: Redelivery;
    }
  | { type: "reclaim-architect"; issue: IssueKey; redeliver: Redelivery }
  | { type: "shutdown" };

export interface ExceptionInfo {
  roleToken: string;
  reason: "no_holder" | "delivery_failed";
  original: Redelivery;
  nack?: { type: "revive-worker"; issue: IssueKey; role: LegionRole };
}

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
  /** Overridable for tests; defaults to a real timer. Used only to bound the wait for a
   * retiring worker's pane to exit before it is killed outright. */
  sleep?(ms: number): Promise<void>;
  workerCatchup: WorkerCatchupDeps;
  now(): number;
}

type RoleBacking = WorkerRoleClaim;

/** The outcome of `promoteQueuedWorker`'s admission-lock critical section: `"launch"` carries
 * everything needed to run `launchWorker` outside the lock; `"prompt"` carries an already-live,
 * cached, idle client to prompt in place (the resume-at-cap path queued via
 * `enqueueIdleWorker` never touched the locator, so there is a real pane to reuse instead of
 * relaunching) — its token deliberately stays on the queue until the prompt actually succeeds
 * (see `promoteQueuedWorker`'s handling below), so a failed prompt never strands the
 * assignment. */
type PromotionDecision =
  | { kind: "retry" }
  | { kind: "stop" }
  | { kind: "stale" }
  | {
      kind: "prompt";
      client: WorkerRpcClient;
      treeKey: IssueKey;
      issue: IssueKey;
      role: LegionRole;
      sessionId: string;
      task: string;
    }
  | {
      kind: "launch";
      claim: WorkerRoleClaim;
      treeKey: IssueKey;
      issue: IssueKey;
      role: LegionRole;
      task: string;
    };

const MAX_TMUX_WINDOW_NAME_LENGTH = 160;
const TMUX_RECONCILIATION_GRACE_MS = 120_000;
/** Bounds for waiting on a retiring worker's pane to exit on its own before it is force-killed. */
const WORKER_RETIREMENT_POLL_ATTEMPTS = 20;
const WORKER_RETIREMENT_POLL_INTERVAL_MS = 100;

function treeName(issue: IssueKey): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
  const encodeIssuePart = (part: string) => {
    const normalized = part.toLowerCase();
    if (!/^[a-z0-9._-]+$/.test(normalized)) {
      throw new Error(`Invalid Legion issue token part: ${part}`);
    }
    return normalized.replaceAll("_", "_u").replaceAll(".", "_d").replaceAll("-", "_h");
  };
  const fullName = `${encodeIssuePart(parsed.owner)}__${encodeIssuePart(parsed.repo)}-${parsed.number}`;
  if (fullName.length <= MAX_TMUX_WINDOW_NAME_LENGTH) return fullName;

  const suffix = createHash("sha256").update(fullName).digest("hex").slice(0, 16);
  return `${fullName.slice(0, MAX_TMUX_WINDOW_NAME_LENGTH - suffix.length - 1)}-${suffix}`;
}

/**
 * A worker socket's basename must stay well under the ~100-byte Unix socket path limit
 * regardless of the issue key's length (unlike `treeName`, which only bounds itself to tmux's
 * much longer window-name limit): derived from the issue number and role alone, plus a short
 * hash of the full issue key to disambiguate the same issue number across different repos.
 */
function workerSocketBasename(issue: IssueKey, role: LegionRole): string {
  const parsed = parseIssueKey(issue);
  if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
  const hash = createHash("sha256").update(issue).digest("hex").slice(0, 8);
  return `${parsed.number}-${role}-${hash}`;
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
  /** Serializes tmux window creation per issue and launch decisions per role, so two concurrent
   * spawns never each see "no window yet" and open two, or each see "no live claim" and launch
   * twice. Keyed by issue for the former, by role token for the latter. */
  private readonly issueLaunchQueue = new Map<IssueKey, Promise<unknown>>();
  private readonly roleLaunchQueue = new Map<string, Promise<unknown>>();
  /** Single-key critical section (via `serialize`, fixed key `"global"`) around every
   * running-worker admission decision: `launchOrQueue`'s read-count->decide->launch-or-enqueue,
   * and each `drainWorkerQueue` promotion attempt's read-count->re-peek-head->decide. Without it
   * two concurrent decisions (different roles, or two racing idle/exit promotions) could both
   * observe capacity before either commits it, over-admitting or double-launching a queue head.
   * Always acquired *inside* the per-role `roleLaunchQueue` critical section for the token in
   * play, never the other way around, so the two locks can never deadlock on each other. */
  private readonly admissionLock = new Map<string, Promise<unknown>>();
  /** Role tokens currently mid-launch (decided admitted under `admissionLock`, but
   * `launchWorker`'s tmux/workspace-provisioning work is still running outside any lock so a
   * slow or hung launch never blocks other admission decisions). Counted by
   * `runningWorkerCount()` as occupying a slot; a token is added the moment admission is granted
   * and removed once `launchWorker` settles (success — the claim now has a real locator — or
   * failure). */
  private readonly launching = new Set<string>();
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
  private promotionSweep?: { attempted: Set<IssueKey>; inFlight: number };
  /** Every in-flight `startRoot` call, including ones fired without being awaited (`admit`'s promotion). `drainSpawns` awaits these so `stop()`'s final save observes each spawn's own persisted state instead of racing it. */
  private readonly spawns = new Set<Promise<void>>();
  /** Gates every worker-queue drain (`promoteWorkerQueue`'s fire-and-forget trigger and
   * `reconcileWorkerAdmission`'s explicit call alike — neither bypasses this) until
   * `enableWorkerPromotion` flips it. False from construction protects boot: `reconnectWorkers`
   * (called before the daemon's HTTP `api` is ever assigned, since it needs no `api` reference)
   * can synchronously trigger a chain of `get_state` -> `isStreaming: false` -> `onIdle` ->
   * `promoteWorkerQueue` -> `launchWorker` -> `mintWorkerBootToken`, and that last step reads
   * `api` by reference through a closure that does not exist yet at that point in boot — a
   * queued token would otherwise mint nothing (a spurious launch failure) at every daemon
   * restart with a non-empty queue.
   */
  private workerPromotionEnabled = false;

  constructor(private readonly deps: ProcessManagerDeps) {}

  /** The only way `workerPromotionEnabled` is ever set to `true`. Call once, after the
   * daemon's HTTP `api` is assigned, before the first explicit `reconcileWorkerAdmission()` —
   * every worker-queue drain from that point on (fire-and-forget triggers and the explicit
   * call alike) is safe to actually launch/prompt something. */
  enableWorkerPromotion(): void {
    this.workerPromotionEnabled = true;
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

  async registerRoleBacking(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    agentId: string,
    sessionId: string
  ): Promise<void> {
    if (this.rootForIssue(issue) !== treeKey) {
      throw new Error(`Issue ${issue} does not belong to Legion tree ${treeKey}`);
    }
    const token = roleToken(this.deps.state.project, issue, role);
    const existing = this.deps.state.roles[token];
    this.deps.state.roles[token] = {
      ...(existing && "issue" in existing ? existing : {}),
      issue,
      role,
      agentId,
      sessionId,
    };
    this.redeliverHeldRoleEvents(treeKey, issue, role);
    await this.deps.saveState();
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
    if (role === "architect" && this.issueDepth(issue) >= this.deps.config.maxRecursionDepth) {
      throw new Error(
        `Refusing to spawn a sub-architect for ${issue}: recursion depth already at the configured maximum (${this.deps.config.maxRecursionDepth})`
      );
    }
    const token = roleToken(this.deps.state.project, issue, role);
    return this.serialize(this.roleLaunchQueue, token, async () => {
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
        const client = await this.workerClient(token, socketPath).catch(() => undefined);
        const alive = client
          ? await client
              .getState(5_000)
              .then(() => true)
              .catch(() => false)
          : false;
        if (client && alive) {
          // A worker whose `runState` is already non-idle ("running"/"unknown") already occupies
          // its running-worker slot — prompting it is not a new admission, so it goes straight
          // through. An *idle* client is not currently counted by `runningWorkerCount()`, so
          // prompting it unconditionally would flip an uncounted-idle worker to running past
          // `config.workerCap`: gate that one case through the same `admissionLock` decision
          // every other admission goes through. Below cap, `this.launching` reserves the token
          // for the gap between the decision and `client.prompt()` actually flipping `runState`
          // (which happens synchronously as `prompt()`'s first action, but only once called
          // outside the lock — see `launchOrQueue`'s doc comment for why no I/O runs inside it).
          const shouldPrompt = await this.serialize(this.admissionLock, "global", async () => {
            if (client.runState !== "idle") return true;
            if (this.runningWorkerCount() >= this.deps.config.workerCap) {
              this.enqueueIdleWorker(token, claim, task);
              return false;
            }
            this.launching.add(token);
            return true;
          });
          if (!shouldPrompt) {
            await this.deps.saveState();
            this.publishArchitect(treeKey, { type: "worker-queued", issue, role });
            return { status: "queued", roleToken: token };
          }
          try {
            await this.promptExistingWorker(client, token, issue, role, claim.sessionId, task);
            return { status: "resumed", roleToken: token };
          } finally {
            this.launching.delete(token);
            this.promoteWorkerQueue();
          }
        }
        await this.retireWorkerLocator(token, claim.locator);
        return this.launchOrQueue(token, treeKey, issue, role, claim, task);
      }

      return this.launchOrQueue(token, treeKey, issue, role, claim, task);
    });
  }

  /**
   * Launches a brand-new worker pane when a running-worker slot is available (fewer than
   * `config.workerCap` role tokens currently busy on a turn), or queues the task on a
   * locator-less claim and publishes `worker-queued` to the tree's architect otherwise. Every
   * queued task is promoted in FIFO order by `promoteWorkerQueue` once a slot frees up. Only the
   * read-count/decide/reserve-or-enqueue step runs inside the `admissionLock` critical section
   * (pure in-memory claim/queue mutation only — no tmux, workspace, or save I/O), so a
   * concurrent decision for a different role can never observe the same free slot before this
   * one commits it, and never blocks behind this decision's own `saveState`/publish; the actual
   * `launchWorker` call runs outside the lock so a slow or hung launch never wedges every other
   * admission decision.
   */
  private async launchOrQueue(
    token: string,
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    task: string
  ): Promise<SpawnWorkerResponse> {
    const admitted = await this.serialize(this.admissionLock, "global", async () => {
      if (this.runningWorkerCount() >= this.deps.config.workerCap) {
        this.enqueueClaimForLaunch(token, issue, role, claim, task);
        return false;
      }
      this.launching.add(token);
      return true;
    });
    if (!admitted) {
      await this.deps.saveState();
      this.publishArchitect(treeKey, { type: "worker-queued", issue, role });
      return { status: "queued", roleToken: token };
    }
    try {
      await this.launchWorker(treeKey, issue, role, claim, task);
    } finally {
      this.launching.delete(token);
      this.promoteWorkerQueue();
    }
    return { status: "spawned", roleToken: token };
  }

  /**
   * Counts role claims (phase workers and sub-architects — never the controller, which is
   * tracked separately via `state.controllerLocator`) currently occupying a running-worker slot:
   * a claim with a locator and no cached shim client (booting, or not yet reconnected after a
   * restart) counts conservatively as running; one with a cached client counts unless that
   * client's `runState` is `"idle"`. Also counts every token in `this.launching` — reserved by
   * `launchOrQueue`/`promoteQueuedWorker`/`spawnWorker`'s idle-resume path the instant admission
   * is granted, before the claim's locator (or `runState`) reflects it, so work still in flight
   * is never double-admitted alongside a concurrent decision for a different role. Computed
   * fresh on every call: nothing besides `launching` is reserved or released by hand. The root
   * architect is never in `state.roles` at all (its own admission is tree-scoped via
   * `state.trees[issue].locator`), so it needs no special-case exclusion here — a sub-architect
   * claim always belongs in this count, including one whose child issue later graduates into
   * its own tree root.
   */
  private runningWorkerCount(): number {
    let count = this.launching.size;
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if (!("issue" in claim) || claim.locator === undefined) continue;
      const client = this.workerClients.get(token);
      if (!client || client.runState !== "idle") count += 1;
    }
    return count;
  }

  /**
   * Pure in-memory mutation (called only from inside `admissionLock`'s critical section — no
   * I/O here; the caller does `saveState`/publish after the lock releases) for a worker with no
   * live pane to reuse: records the task on the existing claim (mutated in place, never
   * replaced, so `agentId`/`generation`/`launchFailures` survive) and appends its token to the
   * FIFO running-worker queue. Any existing locator is cleared, moving its `ompSessionFile` to
   * `resumeSessionFile` so the eventual promoted launch still resumes the same agent;
   * `sessionId` is deliberately kept (never deleted) so a subsequent respawn's boot token still
   * names this session as the one it must resume — the exact path a delayed promotion depends
   * on to reject a boot that comes back as a different agent.
   */
  private enqueueClaimForLaunch(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    task: string
  ): void {
    const target: WorkerRoleClaim = claim ?? { issue, role };
    const resumeSessionFile = target.locator?.ompSessionFile ?? target.resumeSessionFile;
    target.pendingAssignment = task;
    delete target.locator;
    if (resumeSessionFile) target.resumeSessionFile = resumeSessionFile;
    else delete target.resumeSessionFile;
    this.deps.state.roles[token] = target;
    const queue = this.deps.state.workerAdmission.queue;
    if (!queue.includes(token)) queue.push(token);
  }

  /**
   * Pure in-memory mutation (same `admissionLock`-only calling convention as
   * `enqueueClaimForLaunch`) for a worker whose pane is still alive and idle but the cap has no
   * free slot right now: the locator and cached client are left completely alone — the worker
   * is not retired, its pane is not touched — since `promoteQueuedWorker`'s `"prompt"` branch
   * only needs to `prompt()` it in place once a slot frees up, never relaunch it.
   */
  private enqueueIdleWorker(token: string, claim: WorkerRoleClaim, task: string): void {
    claim.pendingAssignment = task;
    const queue = this.deps.state.workerAdmission.queue;
    if (!queue.includes(token)) queue.push(token);
  }

  /** Prompts an already-connected, already-live worker client with `task` — the shared
   * implementation behind every "resume an existing worker" path (`spawnWorker`'s direct
   * alive-idle-resume-below-cap branch, `promoteQueuedWorker`'s `"prompt"` decision, and
   * `/worker/ready`), as opposed to `launchWorker`, which opens a fresh pane. Releases this
   * token's `launching` reservation and re-checks the queue the instant `prompt()` resolves and
   * flips `runState` away from `"idle"` — `runningWorkerCount()`'s client-based counting picks
   * this worker up from that exact moment, so the reservation would otherwise outlive its
   * purpose and needlessly delay a concurrent decision that could safely see this slot as
   * occupied. Then registers the current phase (`state.phases[issue]`) so `phase/complete` can
   * find it, clears the claim's `pendingAssignment` if it was still set, and persists. Throws
   * (without touching phases/`pendingAssignment`/persisting) if `prompt()` itself rejects — the
   * caller decides what "the prompt failed" means for its own bookkeeping. */
  private async promptExistingWorker(
    client: WorkerRpcClient,
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    task: string
  ): Promise<void> {
    await client.prompt(task);
    this.launching.delete(token);
    this.promoteWorkerQueue();
    this.deps.state.phases[issue] = { phase: role, sessionId };
    const claim = this.deps.state.roles[token];
    if (claim && "issue" in claim) delete claim.pendingAssignment;
    await this.persist();
  }

  /** Fire-and-forget trigger: "something may have changed, re-check the running-worker queue."
   * Safe to call any number of times from any locator-clearing or client-idle/close event —
   * `drainWorkerQueue` serializes itself per role token and re-derives occupancy on every step.
   * No-ops with a log until `enableWorkerPromotion` has been called — see
   * `workerPromotionEnabled`'s doc comment for why. */
  private promoteWorkerQueue(): void {
    if (!this.workerPromotionEnabled) {
      console.error(
        "[legion] worker-queue promotion trigger ignored: not yet enabled (still booting)"
      );
      return;
    }
    void this.drainWorkerQueue();
  }

  /** Promotes queued workers in FIFO order, one at a time, while a running-worker slot is
   * available. Each pass re-peeks the current queue head (outside any lock — staleness is
   * re-validated inside `promoteQueuedWorker`) and runs the actual admission decision through
   * the same per-role launch queue `spawnWorker` uses, keyed to that token, so a concurrent
   * `spawnWorker` call and a promotion attempt for the same role token can never both decide to
   * launch it. Two concurrent `drainWorkerQueue` runs racing for one freed slot both peek the
   * same head and both queue up on that token's `roleLaunchQueue` entry; only the first actually
   * launches; the second finds (inside the lock) that the head has already moved on and no-ops. */
  private async drainWorkerQueue(): Promise<void> {
    for (;;) {
      const token = this.deps.state.workerAdmission.queue[0];
      if (token === undefined) return;
      const shouldContinue = await this.serialize(this.roleLaunchQueue, token, () => {
        return this.promoteQueuedWorker(token);
      });
      if (!shouldContinue) return;
    }
  }

  /** Runs the actual admission decision for one candidate queue-head token. Only the
   * read-count/re-peek-head/decide step runs inside the single global `admissionLock` critical
   * section (nested inside this token's `roleLaunchQueue` entry, acquired by the caller) — so
   * the running-worker count is read and committed atomically against every other admission
   * decision, whether that is another promotion attempt or a concurrent `spawnWorker` call for a
   * different role — while the actual `launchWorker`/`client.prompt()` call runs outside the
   * lock, so a slow or hung launch never wedges every other admission decision. Re-checks that
   * `token` is still the queue head before doing anything: by the time this call gets its turn
   * on the per-role queue, another promotion may already have consumed or dropped it. A claim
   * that still carries a locator (queued via `spawnWorker`'s idle-resume-at-cap path, which
   * never touched it) is promoted by prompting its already-live, already-idle-cached client in
   * place rather than relaunching a pane that is already running. Returns `true` if
   * `drainWorkerQueue` should loop again (progress was made, a stale entry was dropped, this
   * attempt was a no-op because the head moved on, an unlaunchable head was retired at
   * `MAX_LAUNCH_FAILURES` so the rest of the queue is not permanently blocked behind it, or a
   * below-threshold failure rotated the head to the tail so the next-in-line gets a turn) or
   * `false` if it should stop (still at cap, or queue empty). */
  private async promoteQueuedWorker(token: string): Promise<boolean> {
    const decision = await this.serialize(
      this.admissionLock,
      "global",
      async (): Promise<PromotionDecision> => {
        const queue = this.deps.state.workerAdmission.queue;
        if (queue[0] !== token) return { kind: "retry" };
        if (this.runningWorkerCount() >= this.deps.config.workerCap) return { kind: "stop" };
        const claim = this.deps.state.roles[token];
        const parsed = parseRoleToken(this.deps.state.project, token);
        const treeKey =
          parsed && !("controller" in parsed) ? this.rootForIssue(parsed.issue) : undefined;
        if (
          claim === undefined ||
          !("issue" in claim) ||
          claim.pendingAssignment === undefined ||
          parsed === undefined ||
          "controller" in parsed ||
          treeKey === undefined
        ) {
          // Stale entry: the claim vanished, lost its task, or isn't a real issue/tree. Drop and
          // continue.
          queue.shift();
          return { kind: "stale" };
        }
        if (claim.locator !== undefined) {
          const client = this.workerClients.get(token);
          if (!client || client.runState !== "idle" || !claim.sessionId) {
            // The idle-resume-at-cap invariant this entry was queued under no longer holds (the
            // client died, started running some other way, or never confirmed a session) —
            // stale, drop and continue.
            queue.shift();
            return { kind: "stale" };
          }
          this.launching.add(token);
          // Deliberately NOT shifted here — this token stays on the queue until the prompt
          // below actually succeeds (see the `"prompt"` handling), so a failed prompt never
          // strands the assignment with no queue entry and no launched pane.
          return {
            kind: "prompt",
            client,
            treeKey,
            issue: parsed.issue,
            role: parsed.role,
            sessionId: claim.sessionId,
            task: claim.pendingAssignment,
          };
        }
        this.launching.add(token);
        return {
          kind: "launch",
          claim,
          treeKey,
          issue: parsed.issue,
          role: parsed.role,
          task: claim.pendingAssignment,
        };
      }
    );

    if (decision.kind === "stale") {
      await this.persist();
      return true;
    }
    if (decision.kind === "retry") return true;
    if (decision.kind === "stop") return false;

    if (decision.kind === "prompt") {
      try {
        await this.promptExistingWorker(
          decision.client,
          token,
          decision.issue,
          decision.role,
          decision.sessionId,
          decision.task
        );
      } catch (error) {
        console.error(`[legion] failed to prompt queued worker ${token}:`, error);
        // The token was never removed from the queue for this decision (see the admission-lock
        // section above) and `promptExistingWorker` only mutates the claim's
        // `pendingAssignment` after `client.prompt()` succeeds — both are left exactly as they
        // were. Stop draining instead of looping straight back into the same broken client
        // (deliberately no `promoteWorkerQueue()` call here — that would just re-peek this same
        // head and retry the same broken prompt again): the close handler will notice this
        // socket died (or never confirms idle again) and clear the locator via
        // `markWorkerDead`, whose own `promoteWorkerQueue()` call then re-promotes this
        // still-queued assignment through a fresh `launchWorker` relaunch once that happens.
        this.launching.delete(token);
        await this.persist();
        return false;
      }
      await this.serialize(this.admissionLock, "global", async () => {
        const queue = this.deps.state.workerAdmission.queue;
        const index = queue.indexOf(token);
        if (index !== -1) queue.splice(index, 1);
      });
      await this.persist();
      this.publishArchitect(decision.treeKey, {
        type: "worker-started",
        issue: decision.issue,
        role: decision.role,
      });
      return true;
    }

    try {
      await this.launchWorker(
        decision.treeKey,
        decision.issue,
        decision.role,
        decision.claim,
        decision.task
      );
      this.publishArchitect(decision.treeKey, {
        type: "worker-started",
        issue: decision.issue,
        role: decision.role,
      });
      return true;
    } catch (error) {
      console.error(`[legion] failed to promote queued worker ${token}:`, error);
      // launchWorker's own catch already recorded launchFailures on the claim and published
      // `launch-failed` exactly once, at the tick it crosses MAX_LAUNCH_FAILURES. Mirror tree
      // admission: once that threshold is hit, retire the head from the queue so an
      // unlaunchable entry (e.g. a missing session file — a deterministic, permanent failure)
      // never blocks every other queued worker behind it forever. Below the threshold, rotate
      // the head to the tail instead of leaving it parked at the head: nothing else ever retries
      // a queue head on its own (there is no periodic promotion tick besides the boot/cap-change
      // sweep and this drain loop itself), so leaving it in place would starve every worker
      // queued behind it until this exact token's own next `spawnWorker` call happens to retry
      // it. A single-item queue rotating in place exposes the same head again — the returned
      // `shouldContinue` below stops the drain in that case instead of burning every
      // MAX_LAUNCH_FAILURES attempt in one pass; the periodic sweep or the next idle/dead event
      // retries it.
      const shouldContinue = await this.serialize(this.admissionLock, "global", async () => {
        const claim = this.deps.state.roles[token];
        const failures = claim && "issue" in claim ? (claim.launchFailures ?? 0) : 0;
        const queue = this.deps.state.workerAdmission.queue;
        if (failures >= MAX_LAUNCH_FAILURES) {
          const index = queue.indexOf(token);
          if (index !== -1) queue.splice(index, 1);
        } else if (queue[0] === token) {
          queue.shift();
          queue.push(token);
        }
        return queue[0] !== token;
      });
      await this.persist();
      return shouldContinue;
    } finally {
      // No `promoteWorkerQueue()` call here: the success path already released the reservation
      // and re-checked the queue inside `launchWorker` itself, right after its locator write;
      // the failure path's `shouldContinue` above already tells `drainWorkerQueue`'s own loop
      // whether to continue — calling it again here would re-peek this same still-at-the-head
      // token immediately on a `false` result, defeating the very rotation-stop this method just
      // decided on.
      this.launching.delete(token);
    }
  }

  /** Re-evaluates the running-worker queue against the current `config.workerCap` — called at
   * boot (after `reconnectWorkers` has rebuilt live shim connections) and by the periodic linger
   * sweep, so a cap raised between restarts, or a below-threshold launch failure that rotated
   * its head to the tail with nothing else to trigger a retry, both eventually get another
   * promotion attempt. Awaited by the boot caller (unlike the fire-and-forget
   * `promoteWorkerQueue` trigger used elsewhere) so a live, real request racing in during boot
   * serializes on `admissionLock` behind this convergence instead of deciding against a
   * possibly-stale running-worker count. Never bypasses `workerPromotionEnabled` — the same gate
   * `promoteWorkerQueue` respects — so a caller (boot, the periodic sweep, or a test) must
   * `enableWorkerPromotion()` first; there is no second, ungated way into `drainWorkerQueue`. */
  async reconcileWorkerAdmission(): Promise<void> {
    if (!this.workerPromotionEnabled) {
      console.error(
        "[legion] worker-admission reconciliation skipped: promotion not yet enabled (call enableWorkerPromotion() first)"
      );
      return;
    }
    await this.drainWorkerQueue();
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
    const token = roleToken(this.deps.state.project, issue, role);
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
  }

  /** A worker's socket confirmed dead — a boot-time reconnect probe failed, or a live
   * connection's own `client.closed` handler tried and failed its one reconnect attempt — so
   * its locator is retired and cleared: retires whatever pane it may still be running, then
   * clears the locator — moving its `ompSessionFile` to `resumeSessionFile` so the eventual
   * respawn/promotion still resumes the same agent — leaving `pendingAssignment` so it still
   * delivers the queued task, then re-checks the running-worker queue, since clearing the
   * locator may have freed the slot this worker was occupying. Runs inside this token's
   * `roleLaunchQueue` critical section — the same one `launchWorker`/`promoteQueuedWorker` use —
   * and re-checks that the claim's current locator still matches the one it was handed before
   * touching anything: by the time this call gets its turn, a newer launch for the same token
   * may already have replaced it (or the claim may be gone entirely, e.g. `closeTree`), and
   * this must never delete a newer launch's locator or retire a pane that isn't the one it was
   * told to. */
  private async markWorkerDead(token: string, locator: WorkerLocator): Promise<void> {
    await this.serialize(this.roleLaunchQueue, token, async () => {
      const current = this.deps.state.roles[token];
      if (!current || !("issue" in current) || current.locator?.tmuxPaneId !== locator.tmuxPaneId) {
        return;
      }
      await this.retireWorkerLocator(token, locator);
      const resumeSessionFile = current.locator?.ompSessionFile ?? current.resumeSessionFile;
      delete current.locator;
      if (resumeSessionFile) current.resumeSessionFile = resumeSessionFile;
      await this.persist();
    });
    this.promoteWorkerQueue();
  }

  /** Reconnects to every live worker's shim socket after a daemon restart, probing liveness.
   * A connect failure means the worker is confirmed dead. A connect that succeeds but whose
   * follow-up `get_state` fails (times out, say) means only that the shim is busy answering
   * this one request in time — never a reason to kill a live worker — so the claim is left
   * exactly as is, with its conservative "unknown"-counts-as-running `runState`. */
  async reconnectWorkers(): Promise<void> {
    const claims = Object.entries(this.deps.state.roles).filter(
      (entry): entry is [string, WorkerRoleClaim & { locator: WorkerLocator }] =>
        "issue" in entry[1] && entry[1].locator !== undefined
    );
    await Promise.all(
      claims.map(async ([token, claim]) => {
        let client: WorkerRpcClient;
        try {
          client = await this.workerClient(token, claim.locator.socketPath);
        } catch (error) {
          console.error(`[legion] failed to reconnect worker ${token}:`, error);
          await this.markWorkerDead(token, claim.locator);
          return;
        }
        try {
          // Seeds `runState` from the response's `isStreaming` field: a worker that was already
          // idle before this restart is not left stuck at the conservative "unknown" default
          // (counted as running) forever, since nothing else will ever observe an
          // `agent_start`/`agent_end` frame to correct it if it never runs another turn.
          await client.getState(5_000);
        } catch (error) {
          console.error(
            `[legion] worker ${token} reconnected but get_state failed (treating as busy, not dead):`,
            error
          );
        }
      })
    );
  }

  async markProcessDead(treeKey: IssueKey, generation?: number): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.generation !== (generation ?? tree.generation)) return;
    await this.removeTreeWindow(tree);
    tree.status = "dead";
    await this.releaseSlot(treeKey);
    await this.deps.saveState();
  }

  /**
   * Kills every tmux window this tree's root and worker claims recorded, not just the root's own
   * — a child-issue worker window is never referenced by `tree.locator`, so closing a tree only
   * ever killed the root's window and left every other issue's worker window orphaned until the
   * next reconciliation sweep. Shutdown today is a direct pane/window kill, without first asking
   * each worker's shim to close its OMP process's stdin.
   */
  async closeTree(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    const windowIds = new Set<string>();
    if (tree.locator?.tmuxWindowId) windowIds.add(tree.locator.tmuxWindowId);
    for (const claim of Object.values(this.deps.state.roles)) {
      if ("issue" in claim && this.rootForIssue(claim.issue) === treeKey && claim.locator) {
        windowIds.add(claim.locator.tmuxWindowId);
      }
    }
    for (const windowId of windowIds) {
      await tmux.killWindow(this.deps.run, windowId);
    }
    delete tree.locator;
    tree.status = "closed";
    delete tree.lingerUntil;
    await this.releaseSlot(treeKey);
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && this.rootForIssue(claim.issue) === treeKey) {
        delete this.deps.state.roles[token];
      }
    }
    this.clearTreePhases(treeKey);
    await this.deps.saveState();
    // Deleting this tree's claims may have freed running-worker slots other trees' queues are
    // waiting on.
    this.promoteWorkerQueue();
  }

  /**
   * Kills every `@legion_owner`-marked tmux window this state does not
   * name (via a tree's or the controller's locator) and that has been idle
   * at least `graceMs` — long enough that a window mid-creation (activity
   * not yet recorded) is never mistaken for an orphan. Boot calls this with
   * `graceMs: 0`: it never trusts a window it did not itself record, so
   * there is no such race to protect against there (see `reconcileAdmission`).
   */
  async reconcileTmuxWindows(graceMs = TMUX_RECONCILIATION_GRACE_MS): Promise<void> {
    const session = `legion-${this.deps.state.project}`;
    const owner = session;
    const known = new Set(
      [
        ...Object.values(this.deps.state.trees).map((tree) => tree.locator?.tmuxWindowId),
        ...Object.values(this.deps.state.roles).map((claim) =>
          "issue" in claim ? claim.locator?.tmuxWindowId : undefined
        ),
        this.deps.state.controllerLocator?.tmuxWindowId,
      ].filter((windowId): windowId is string => windowId !== undefined)
    );
    const unknown = await tmux.listUnknownOwnedWindows(this.deps.run, session, owner, known);
    for (const { windowId, activityAt } of unknown) {
      if (this.deps.now() - activityAt < graceMs) continue;
      await tmux.killWindow(this.deps.run, windowId);
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

  async ensureController(): Promise<void> {
    if (await this.controllerAlive()) return;
    if (!this.controllerSpawn) {
      this.controllerSpawn = (async () => {
        const controllerSecret = await this.deps.mintControllerCapability();
        await this.spawnController(controllerSecret);
      })().finally(() => {
        this.controllerSpawn = undefined;
      });
    }
    await this.controllerSpawn;
  }

  async probe(treeKey: IssueKey): Promise<"alive" | "dead"> {
    const tree = this.deps.state.trees[treeKey];
    const locator = tree?.locator;
    if (!locator) return "dead";

    const target = locator.tmuxPaneId ?? locator.tmuxWindowId;
    const pid = await tmux.panePid(this.deps.run, target);
    if (pid === undefined) return "dead";
    return (await this.isOmpPane(pid)) ? "alive" : "dead";
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
        role: directive.type === "revive-worker" ? directive.role : "architect",
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
  async markTreeReady(treeKey: IssueKey): Promise<void> {
    const tree = this.requireTree(treeKey);
    if (tree.locator?.socketPath) {
      // The root architect is a worker of its own issue: connect exactly as workerReady does so
      // its shim's pre-connect backlog drains and it can be prompted/resumed later.
      await this.workerClient(
        roleToken(this.deps.state.project, treeKey, "architect"),
        tree.locator.socketPath
      );
    }
    const recoveries = tree.recoveryEvents;
    if (!recoveries?.length) return;
    for (const recovery of [...recoveries]) {
      let recovered = false;
      if (recovery.issue === treeKey && recovery.role === "architect") {
        this.deps.natsPublish(recovery.original.topic, recovery.original.payload);
        recovered = true;
      } else {
        const backing = this.roleBacking(recovery.issue, recovery.role);
        if (backing?.agentId) {
          recovered = await this.reviveWorker(
            treeKey,
            recovery.issue,
            recovery.role,
            backing.agentId,
            recovery.original
          );
        }
      }
      if (recovered) recoveries.splice(recoveries.indexOf(recovery), 1);
    }
    await this.deps.saveState();
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

  async expireLinger(treeKey: IssueKey): Promise<void> {
    void this.controlDirective(treeKey, { type: "shutdown" }).catch((error) => {
      console.error(`[legion] shutdown directive failed for ${treeKey}:`, error);
    });
    await this.closeTree(treeKey);
  }

  async handleException(exception: ExceptionInfo): Promise<void> {
    if (exception.nack) {
      this.publishController({
        type: "revive-failed",
        issue: exception.nack.issue,
        role: exception.nack.role,
      });
      return;
    }

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
        await this.persistRecovery(root, parsed.issue, parsed.role, exception.original);
        await this.resurrect(root);
      }
      return;
    }

    const backing = this.roleBacking(parsed.issue, parsed.role);
    if (!backing?.agentId) {
      this.hold(root, parsed.role, exception.original);
      await this.deps.saveState();
      return;
    }

    if ((await this.probe(root)) === "dead") {
      await this.persistRecovery(root, parsed.issue, parsed.role, exception.original);
      await this.resurrect(root);
      return;
    }

    await this.reviveWorker(root, parsed.issue, parsed.role, backing.agentId, exception.original);
  }

  private async reviveWorker(
    root: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    agentId: string,
    original: Redelivery
  ): Promise<boolean> {
    const sessionFile = this.requireTree(root).locator?.ompSessionFile;
    if (!sessionFile) throw new Error(`Live Legion tree ${root} has no OMP session file`);
    const revived = await this.controlDirective(
      root,
      {
        type: "revive-worker",
        issue,
        role,
        agentId,
        parentSessionFile: sessionFile,
        redeliver: original,
      },
      false
    );
    if (!revived) return false;
    const catchup = await workerCatchup(this.deps.state, issue, role, this.deps.workerCatchup);
    this.deps.natsPublish(
      roleTopic(roleToken(this.deps.state.project, issue, role)),
      JSON.stringify(catchup)
    );
    this.deps.natsPublish(original.topic, original.payload);
    return true;
  }

  private async persistRecovery(
    root: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    original: Redelivery
  ): Promise<void> {
    const tree = this.requireTree(root);
    let recoveries = tree.recoveryEvents;
    if (!recoveries) {
      recoveries = [];
      tree.recoveryEvents = recoveries;
    }
    if (
      !recoveries.some(
        (recovery) =>
          recovery.issue === issue &&
          recovery.role === role &&
          recovery.original.eventId === original.eventId
      )
    ) {
      recoveries.push({ issue, role, original });
    }
    await this.deps.saveState();
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
      heldEvents: [],
    };
    this.deps.state.trees[issue] = tree;
    return tree;
  }

  private requireTree(treeKey: IssueKey): TreeState {
    const tree = this.deps.state.trees[treeKey];
    if (!tree) throw new Error(`Unknown Legion tree: ${treeKey}`);
    return tree;
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
    const parsed = parseIssueKey(issue);
    if (!parsed) throw new Error(`Invalid IssueKey: ${issue}`);
    return provisionIssueWorkspace(issue, {
      extensionPackage: EXTENSION_PACKAGE,
      stateDir: this.deps.config.stateDir,
      provisioningToken: async () => await this.deps.provisioningToken(parsed.owner),
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
      // DISPATCH_MCP_URL is a transitional alias for clients that still read it instead of
      // DISPATCH_URL directly.
      DISPATCH_URL: this.deps.config.dispatchUrl,
      DISPATCH_MCP_URL: this.deps.config.dispatchUrl && `${this.deps.config.dispatchUrl}/mcp`,
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
    tree.locator = locator;
    tree.status = "active";
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
      client.onIdle(() => this.promoteWorkerQueue());
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

  /**
   * Fires when a cached worker connection's socket closes at runtime (not a boot-time
   * `reconnectWorkers` probe). A replaced/retired client's late close (this cached entry has
   * already moved on to a newer connection) is a stale event and must never touch the current
   * connection's claim — it returns immediately without evicting anything or reconnecting.
   * Otherwise evicts the now-stale cache entry, then tries at most one reconnect per claim
   * generation — the shim may still be listening with a fresh socket, or momentarily
   * restarting — before concluding the worker is truly dead; a second close for the same
   * generation (the reconnect's own connection also died) goes straight to `markWorkerDead`
   * instead of chaining into another reconnect attempt, so a flapping socket can never loop
   * forever. A reconnect that *fails to connect* means the worker is confirmed dead; one that
   * connects but whose follow-up `get_state` fails only means the shim is busy — never a reason
   * to kill a live worker, so the claim is left as is. A root architect or controller
   * connection has no matching `WorkerRoleClaim` (they are tracked via
   * `state.trees`/`state.controllerLocator`), so this is a no-op for them past the cache
   * eviction — `markWorkerDead` clears the claim's *persistent* locator (not just the ephemeral
   * client cache entry), which is what lets `runningWorkerCount()` stop counting a dead worker
   * as running once its socket is confirmed closed.
   */
  private async onWorkerClientClosed(token: string, client: WorkerRpcClient): Promise<void> {
    if (this.workerClients.get(token) !== client) return;
    this.workerClients.delete(token);
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim) || claim.locator === undefined) return;
    const locator = claim.locator;
    const attemptKey = `${token}:${claim.generation ?? 0}`;
    if (this.reconnectAttempted.has(attemptKey)) {
      await this.markWorkerDead(token, locator);
      return;
    }
    this.reconnectAttempted.add(attemptKey);
    let reconnected: WorkerRpcClient;
    try {
      reconnected = await this.workerClient(token, locator.socketPath);
    } catch {
      // Reconnect failed to connect at all; the worker is confirmed dead.
      await this.markWorkerDead(token, locator);
      return;
    }
    try {
      await reconnected.getState(5_000);
    } catch (error) {
      console.error(
        `[legion] worker ${token} reconnected but get_state failed (treating as busy, not dead):`,
        error
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
        // DISPATCH_MCP_URL is a transitional alias for clients that still read it instead of
        // DISPATCH_URL directly.
        DISPATCH_URL: this.deps.config.dispatchUrl,
        DISPATCH_MCP_URL: this.deps.config.dispatchUrl && `${this.deps.config.dispatchUrl}/mcp`,
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
      // `closeTree` may have deleted this tree (and every claim under it) while this launch's
      // I/O was in flight above — writing a fresh claim for a tree that is now closed would
      // create a zombie occupying a running-worker slot forever, since nothing would ever
      // release it (closeTree already ran its own release/queue-promotion pass and has no way
      // to know this launch was still in flight). Retire the pane this launch just opened and
      // return without writing anything.
      if (this.rootForIssue(issue) !== treeKey || this.requireTree(treeKey).status === "closed") {
        await this.retireWorkerLocator(token, locator);
        // This launch never wrote a claim — the slot it was reserving is simply free again.
        this.launching.delete(token);
        this.promoteWorkerQueue();
        return;
      }

      this.deps.state.roles[token] = {
        issue,
        role,
        // sessionId deliberately not carried over: it stays unset until /worker/started
        // confirms this generation's boot, so a concurrent spawnWorker call during the boot
        // window sees an unconfirmed claim rather than racing a stale one (both for a fresh
        // spawn and a resume, where OMP reports the same session id it had before).
        ...(claim?.agentId ? { agentId: claim.agentId } : {}),
        generation,
        // A successful launch always starts this claim's failure count fresh: a prior claim's
        // accumulated `launchFailures` (e.g. from being rotated through the queue below
        // MAX_LAUNCH_FAILURES — see `promoteQueuedWorker`) must never carry over past a launch
        // that actually succeeds, mirroring `tree.launchFailures = 0` on a successful root spawn.
        launchFailures: 0,
        pendingAssignment: task,
        locator: {
          ...locator,
          ...(resumeSessionFile ? { ompSessionFile: resumeSessionFile } : {}),
        },
        // resumeSessionFile deliberately dropped: a fresh locator now carries its own
        // ompSessionFile, so the standalone fallback field is stale.
      };
      const queueIndex = this.deps.state.workerAdmission.queue.indexOf(token);
      if (queueIndex !== -1) this.deps.state.workerAdmission.queue.splice(queueIndex, 1);
      // The claim now has a locator: runningWorkerCount()'s claim-based counting picks this
      // worker up from this instant (a claim with a locator and no cached client counts
      // conservatively as running), so the `launching` reservation held since admission is no
      // longer needed — release it (and re-check the queue) now, before the saveState below,
      // instead of leaving a concurrent admission decision waiting on this launch's full I/O
      // (workspace provisioning, saveState) to settle before it can see this slot as occupied.
      this.launching.delete(token);
      this.promoteWorkerQueue();
      await this.deps.saveState();
    } catch (error) {
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
      // DISPATCH_MCP_URL is a transitional alias for clients that still read it instead of
      // DISPATCH_URL directly.
      DISPATCH_URL: this.deps.config.dispatchUrl,
      DISPATCH_MCP_URL: this.deps.config.dispatchUrl && `${this.deps.config.dispatchUrl}/mcp`,
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
   * `/controller/ready` from accepting the role and replaying held events — the connection is
   * retried on the next `spawnWorker`/`workerReady`/reconnect attempt that touches this socket.
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

  private async removeTreeWindow(tree: TreeState): Promise<void> {
    const windowId = tree.locator?.tmuxWindowId;
    delete tree.locator;
    if (!windowId) return;
    await tmux.killWindow(this.deps.run, windowId);
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

  private async sleep(ms: number): Promise<void> {
    if (this.deps.sleep) return this.deps.sleep(ms);
    await new Promise<void>((resolve) => setTimeout(resolve, ms));
  }

  /**
   * Before a worker's locator is replaced or cleared (a dead spawn-time reconnect, or a failed
   * `reconnectWorkers` probe at boot), retires whatever pane it may still be running: best-effort
   * asks the shim to close its OMP child's stdin, gives it a bounded window to exit on its own,
   * then kills the pane directly. This still runs when the RPC connection to the shim already
   * failed, since a dead/unreachable shim can still leave its OMP child — or the pane itself —
   * running and holding the role's Envoy subscription.
   */
  private async retireWorkerLocator(token: string, locator: WorkerLocator): Promise<void> {
    try {
      const client = await this.workerClient(token, locator.socketPath);
      client.shutdown();
    } catch {
      // Shim unreachable or already gone; fall through to polling the pane directly.
    }
    this.workerClients.delete(token);
    const target = locator.tmuxPaneId ?? locator.tmuxWindowId;
    for (let attempt = 0; attempt < WORKER_RETIREMENT_POLL_ATTEMPTS; attempt += 1) {
      const pid = await tmux.panePid(this.deps.run, target);
      if (pid === undefined || !(await this.isOmpPane(pid))) return;
      await this.sleep(WORKER_RETIREMENT_POLL_INTERVAL_MS);
    }
    if (locator.tmuxPaneId) await tmux.killPane(this.deps.run, locator.tmuxPaneId);
    else await tmux.killWindow(this.deps.run, locator.tmuxWindowId);
  }

  private async controllerAlive(): Promise<boolean> {
    const windowId = this.deps.state.controllerLocator?.tmuxWindowId;
    if (!windowId) return false;
    const pid = await tmux.panePid(this.deps.run, windowId);
    if (pid === undefined) {
      delete this.deps.state.controllerLocator;
      return false;
    }
    const alive = await this.isOmpPane(pid);
    if (!alive) delete this.deps.state.controllerLocator;
    return alive;
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

  private roleBacking(issue: IssueKey, role: LegionRole): RoleBacking | undefined {
    for (const claim of Object.values(this.deps.state.roles) as RoleBacking[]) {
      if (claim.issue === issue && claim.role === role) return claim;
    }
    return undefined;
  }

  private hold(root: IssueKey, role: LegionRole, event: Redelivery): void {
    this.requireTree(root).heldEvents.push({
      role,
      payloadJson: event.payload,
      heldAt: new Date(this.deps.now()).toISOString(),
      eventId: event.eventId,
    });
  }
  private redeliverHeldRoleEvents(treeKey: IssueKey, issue: IssueKey, role: LegionRole): void {
    if (this.deps.state.issues[issue]?.released !== true) return;
    const token = roleToken(this.deps.state.project, issue, role);
    const heldEvents = this.requireTree(treeKey).heldEvents;
    for (let index = 0; index < heldEvents.length; ) {
      const held = heldEvents[index];
      if (held?.role !== role) {
        index += 1;
        continue;
      }
      this.deps.natsPublish(roleTopic(token), held.payloadJson);
      heldEvents.splice(index, 1);
    }
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
