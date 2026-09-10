import type { SpawnWorkerResponse } from "@legion/contracts";
import { type IssueKey, type LegionRole, parseRoleToken } from "@legion/contracts";
import type { LegionState, WorkerRoleClaim } from "./legion-state";
import { StopFailed, TreeClosingError } from "./process-errors";
import type { WorkerRpcClient } from "./worker-rpc";

/** Bounds a claim's `launchFailures` (cold-launch attempts) or `promptFailures` (queued
 * idle-resume prompt attempts against an already-live worker) before a permanently-broken
 * queue head is retired instead of retried forever. Shared with `ProcessManager.launchWorker`,
 * which increments and publishes against the same threshold for the launch path. */
export const MAX_LAUNCH_FAILURES = 3;

/** The outcome of `promoteQueuedWorker`'s admission-lock critical section: `"launch"` carries
 * everything needed to run `launchWorker` outside the lock; `"prompt"` carries an already-live,
 * cached, idle client to prompt in place (the resume-at-cap path queued via
 * `enqueueIdleWorker` never touched the locator, so there is a real pane to reuse instead of
 * relaunching) — its token deliberately stays on the queue until the prompt actually succeeds
 * (see `promoteQueuedWorker`'s handling below), so a failed prompt never strands the
 * assignment. `"stop"` covers both "still at cap" and a queued locator-carrying claim whose
 * client is alive but not currently idle (mid-prompt or genuinely busy) — neither is stale, so
 * neither drops the entry; only `"stale"` does that (the client is gone, or a session was never
 * confirmed). */
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

/** Everything `WorkerAdmission` reads or triggers on the owning `ProcessManager` — durable state,
 * the configured cap, a live worker-client lookup (for `runState`), persistence, publishing, and
 * the two effects that actually touch a worker's pane or socket (`launchWorker` opens a fresh
 * one; `promptExistingWorker` reuses an already-live one). */
export interface WorkerAdmissionDeps {
  state: LegionState;
  config: { workerCap: number };
  getWorkerClient(token: string): WorkerRpcClient | undefined;
  persist(): Promise<void>;
  publishArchitect(
    treeKey: IssueKey,
    payload:
      | { type: "worker-queued"; issue: IssueKey; role: LegionRole }
      | { type: "worker-started"; issue: IssueKey; role: LegionRole }
  ): void;
  launchWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    task: string
  ): Promise<void>;
  promptExistingWorker(
    client: WorkerRpcClient,
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    task: string
  ): Promise<void>;
  /** Retires a persistently-broken worker's pane and clears its locator — assumes the caller
   * already holds this token's `roleLaunchQueue` critical section (see
   * `ProcessManager.markWorkerDeadLocked`), so the "prompt" decision's own rejection-count
   * circuit breaker can call it without re-acquiring (and deadlocking on) the same lock it is
   * already running inside. */
  retireDeadClaim(token: string, locator: NonNullable<WorkerRoleClaim["locator"]>): Promise<void>;
  /** Overridable for tests only, to observe the ordering of `launchOrQueue`/
   * `resumeOrQueueExisting`'s own reservation-release and queue-drain-trigger relative to the
   * `launchWorker`/`promptExistingWorker` call that precedes them — production never supplies
   * this. Fired exactly at the two points inside each of those methods' own `finally`, in the
   * order they actually run: `"reservation-released"` right after `this.launching`/`release`
   * drops the token, then `"queue-drain-triggered"` right after `promoteWorkerQueue()` is
   * called (not after the drain it triggers settles — that trigger is itself fire-and-forget). */
  onAdmissionEvent?(token: string, event: "reservation-released" | "queue-drain-triggered"): void;
  rootForIssue(issue: IssueKey): IssueKey | undefined;
}

/** Owns the running-worker cap: the admission decision, the FIFO queue, the reservation set that
 * covers the gap between a decision and its effect landing, and the promotion drain that
 * consumes the queue as slots free up. `ProcessManager` keeps every tmux/workspace/token/RPC/
 * reconnect/tree mechanic and calls into this module for admission decisions and the queue drain.
 */
export class WorkerAdmission {
  /** Single-key critical section (via `serialize`, fixed key `"global"`) around every
   * running-worker admission decision: `launchOrQueue`'s read-count->decide->launch-or-enqueue,
   * and each `drainWorkerQueue` promotion attempt's read-count->re-peek-head->decide. Without it
   * two concurrent decisions (different roles, or two racing idle/exit promotions) could both
   * observe capacity before either commits it, over-admitting or double-launching a queue head.
   * Always acquired *inside* the per-role `roleLaunchQueue` critical section for the token in
   * play (via `mutateClaim`), never the other way around, so the two locks can never deadlock
   * on each other. */
  private readonly admissionLock = new Map<string, Promise<unknown>>();
  /** Serializes launch decisions per role — shared between `ProcessManager`'s own
   * `spawnWorker`/`markWorkerDead` (via `mutateClaim`) and this module's own `drainWorkerQueue`,
   * so a concurrent `spawnWorker` call and a promotion attempt for the same role token can never
   * both decide to launch it. */
  private readonly roleLaunchQueue = new Map<string, Promise<unknown>>();
  /** Role tokens currently mid-launch (decided admitted under `admissionLock`, but
   * `launchWorker`'s tmux/workspace-provisioning work is still running outside any lock so a
   * slow or hung launch never blocks other admission decisions). Counted by
   * `runningWorkerCount()` as occupying a slot; a token is added the moment admission is granted
   * and removed once `launchWorker` settles (success — the claim now has a real locator — or
   * failure). */
  private readonly launching = new Set<string>();
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

  constructor(private readonly deps: WorkerAdmissionDeps) {}

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

  /** The only way `workerPromotionEnabled` is ever set to `true`. Call once, after the
   * daemon's HTTP `api` is assigned, before the first explicit `reconcileWorkerAdmission()` —
   * every worker-queue drain from that point on (fire-and-forget triggers and the explicit
   * call alike) is safe to actually launch/prompt something. */
  enableWorkerPromotion(): void {
    this.workerPromotionEnabled = true;
  }

  /** Serializes every claim-touching decision for one role token — `ProcessManager`'s own
   * `spawnWorker`/`markWorkerDead` handling for an existing claim and this module's own
   * queue-drain attempts for that same token — so the two paths can never race each other. */
  mutateClaim<T>(token: string, fn: () => Promise<T>): Promise<T> {
    return this.serialize(this.roleLaunchQueue, token, fn);
  }

  /** Runs `fn` inside the single global admission-decision critical section (pure in-memory
   * reads/mutations only — no I/O), so a concurrent decision for a different role can never
   * observe the same free slot before this one commits it. */
  private withAdmissionLock<T>(fn: () => Promise<T>): Promise<T> {
    return this.serialize(this.admissionLock, "global", fn);
  }

  /** Reserves a token's running-worker slot the instant admission is granted, before its claim's
   * locator (or `runState`) reflects it. Must be called from inside `withAdmissionLock`. */
  private reserve(token: string): void {
    this.launching.add(token);
  }

  /** Releases a token's reservation the moment it becomes countable through another mechanism
   * (a written locator, or a client's `runState` flipping away from `"idle"`) — or, on failure,
   * once it is confirmed the reservation never took effect. Idempotent. */
  private release(token: string): void {
    this.launching.delete(token);
  }

  /**
   * Counts role claims (phase workers and sub-architects — never the controller, which is
   * tracked separately via `state.controllerLocator`) currently occupying a running-worker slot:
   * a claim with a locator and no cached shim client (booting, or not yet reconnected after a
   * restart) counts conservatively as running; one with a cached client counts unless that
   * client's `runState` is `"idle"`. Also counts every token in `this.launching` — reserved by
   * `launchOrQueue`/`promoteQueuedWorker`/`spawnWorker`'s idle-resume path the instant admission
   * is granted, before the claim's locator (or `runState`) reflects it, so work still in flight
   * is never double-admitted alongside a concurrent decision for a different role — *except* a
   * token whose claim already carries a locator: `launchWorker` writes that locator in memory
   * before its own `saveState` call resolves, well before the caller's `finally` releases
   * `this.launching` once the whole call (including that save) settles, so a `launching` token
   * with a locator already written is counted once via the locator branch below, never twice by
   * also counting it here. Computed fresh on every call: nothing besides `launching` is reserved
   * or released by hand. The root architect is never in `state.roles` at all (its own admission
   * is tree-scoped via `state.trees[issue].locator`), so it needs no special-case exclusion here
   * — a sub-architect claim always belongs in this count, including one whose child issue later
   * graduates into its own tree root.
   */
  private runningWorkerCount(): number {
    let count = 0;
    for (const token of this.launching) {
      const claim = this.deps.state.roles[token];
      if (!claim || !("issue" in claim) || claim.locator === undefined) count += 1;
    }
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if (!("issue" in claim) || claim.locator === undefined) continue;
      const client = this.deps.getWorkerClient(token);
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
  async launchOrQueue(
    token: string,
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    task: string
  ): Promise<SpawnWorkerResponse> {
    const admitted = await this.withAdmissionLock(async () => {
      if (this.runningWorkerCount() >= this.deps.config.workerCap) {
        this.enqueueClaimForLaunch(token, issue, role, claim, task);
        return false;
      }
      this.launching.add(token);
      return true;
    });
    if (!admitted) {
      await this.deps.persist();
      this.deps.publishArchitect(treeKey, { type: "worker-queued", issue, role });
      return { status: "queued", roleToken: token };
    }
    try {
      await this.deps.launchWorker(treeKey, issue, role, claim, task);
    } finally {
      this.launching.delete(token);
      this.deps.onAdmissionEvent?.(token, "reservation-released");
      this.promoteWorkerQueue();
      this.deps.onAdmissionEvent?.(token, "queue-drain-triggered");
    }
    return { status: "spawned", roleToken: token };
  }

  /** Pure in-memory queue-push mutation only — no persist, no drain trigger — locking the
   * shared admission queue against a concurrent decision for a different role. Exposed
   * separately from `enqueueForRetry` so a caller that must fold this push into a *larger*
   * durable transition of its own (`ProcessManager.retireUnconfirmedBoot`, whose locator-clear
   * and queue-push must land in the very same save — two separate saves would let a crash, or
   * a persist failure, between them strand the claim: locator-less, unqueued, and invisible to
   * `reconnectWorkers`' locator-only filter, with nothing left to ever retry it) can push
   * without triggering a save of its own. */
  async enqueueForRetryPending(token: string): Promise<void> {
    await this.withAdmissionLock(async () => {
      const queue = this.deps.state.workerAdmission.queue;
      if (!queue.includes(token)) queue.push(token);
    });
  }

  /**
   * Enqueues an already-cleared claim — its locator already gone, its `pendingAssignment`
   * already the task to retry — into the FIFO running-worker queue, persists it, then triggers
   * the normal cap-aware, role-locked drain to relaunch it. Never launches directly itself: a
   * boot the watchdog (or a restart-time reconnect probe) confirmed dead must go through the
   * exact same admission decision as any other launch, not bypass it (over-admission past the
   * cap, or a second pane racing a concurrent same-role spawn, are exactly what that decision
   * exists to prevent). Safe to call before `enableWorkerPromotion()` — the queue push always
   * lands; `promoteWorkerQueue()` itself no-ops (logging) until promotion is enabled, so a
   * restart-time caller (`reconnectWorkers`, run before `api` exists) leaves the token for the
   * boot sequence's own `reconcileWorkerAdmission()` to promote once ready.
   */
  async enqueueForRetry(token: string): Promise<void> {
    await this.enqueueForRetryPending(token);
    await this.deps.persist();
    this.promoteWorkerQueue();
  }

  /**
   * Decides whether an already-connected, already-idle client below the running-worker cap
   * should be prompted immediately in place, or queued via `enqueueIdleWorker` instead — either
   * because it is at cap (persisted and `worker-queued`-published before this resolves) or
   * because it is not currently idle at all (`"running"`/`"unknown"`): injecting a prompt into a
   * client mid-turn is never safe, so that case is queued exactly like the at-cap one, and
   * delivered once the client's own idle transition (`onIdle` -> `promoteWorkerQueue`) re-drains
   * the queue and finds it genuinely idle. Below cap and idle, reserves the token via `reserve`
   * for the gap between this decision and `client.prompt()` actually flipping `runState` away
   * from `"idle"`, then — exactly like `launchOrQueue` owns its own `launchWorker` call — runs
   * the actual `deps.promptExistingWorker` call itself and releases the reservation (and
   * re-checks the queue) the instant it settles, so the caller (`ProcessManager.spawnWorker`)
   * never has to remember admission bookkeeping around its own prompt call.
   */
  async resumeOrQueueExisting(
    token: string,
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim,
    sessionId: string,
    task: string,
    client: WorkerRpcClient
  ): Promise<{ kind: "queued" } | { kind: "resumed" }> {
    const shouldPrompt = await this.withAdmissionLock(async () => {
      if (client.runState !== "idle" || this.runningWorkerCount() >= this.deps.config.workerCap) {
        this.enqueueIdleWorker(token, claim, task);
        return false;
      }
      this.reserve(token);
      return true;
    });
    if (!shouldPrompt) {
      await this.deps.persist();
      this.deps.publishArchitect(treeKey, { type: "worker-queued", issue, role });
      return { kind: "queued" };
    }
    try {
      await this.deps.promptExistingWorker(client, token, issue, role, sessionId, task);
    } finally {
      this.release(token);
      this.deps.onAdmissionEvent?.(token, "reservation-released");
      this.promoteWorkerQueue();
      this.deps.onAdmissionEvent?.(token, "queue-drain-triggered");
    }
    return { kind: "resumed" };
  }

  /** Fire-and-forget trigger: "something may have changed, re-check the running-worker queue."
   * Safe to call any number of times from any locator-clearing or client-idle/close event —
   * `drainWorkerQueue` serializes itself per role token and re-derives occupancy on every step.
   * No-ops with a log until `enableWorkerPromotion` has been called — see
   * `workerPromotionEnabled`'s doc comment for why. */
  promoteWorkerQueue(): void {
    if (!this.workerPromotionEnabled) {
      console.error(
        "[legion] worker-queue promotion trigger ignored: not yet enabled (still booting)"
      );
      return;
    }
    void this.drainWorkerQueue().catch((error) => {
      console.error("[legion] worker queue promotion failed:", error);
    });
  }

  /** Removes every FIFO-queued token whose tree is `treeKey` — called by `closeTreeLocked` as
   * part of its final cleanup, so a tree that has fully closed never leaves behind a queue entry
   * a later, unrelated drain would try to promote against a tree that no longer exists (the
   * promotion catch's `TreeClosingError` handling stops that one attempt from corrupting
   * `launchFailures`/rotation, but does nothing to remove the entry itself). Runs inside
   * `admissionLock` — the same critical section every other queue mutation uses — even though
   * the caller already owns this tree's own teardown, so a concurrent admission decision for a
   * different tree can never observe a half-pruned queue array. */
  async pruneQueueForTree(treeKey: IssueKey): Promise<void> {
    await this.withAdmissionLock(async () => {
      const queue = this.deps.state.workerAdmission.queue;
      for (let index = queue.length - 1; index >= 0; index -= 1) {
        const parsed = parseRoleToken(this.deps.state.project, queue[index]);
        if (
          parsed &&
          !("controller" in parsed) &&
          this.deps.rootForIssue(parsed.issue) === treeKey
        ) {
          queue.splice(index, 1);
        }
      }
    });
  }

  /** Promotes queued workers in FIFO order, one at a time, while a running-worker slot is
   * available. Each pass re-peeks the current queue head (outside any lock — staleness is
   * re-validated inside `promoteQueuedWorker`) and runs the actual admission decision through
   * the same per-role launch queue `spawnWorker` uses, keyed to that token, so a concurrent
   * `spawnWorker` call and a promotion attempt for the same role token can never both decide to
   * launch it. Two concurrent `drainWorkerQueue` runs racing for one freed slot both peek the
   * same head and both queue up on that token's `roleLaunchQueue` entry; only the first actually
   * launches; the second finds (inside the lock) that the head has already moved on and no-ops.
   * Tracks every token this pass has already given a turn: a below-threshold launch/prompt
   * failure rotates its token to the tail rather than removing it, so with two or more queued
   * tokens all failing, the head keeps changing on every rotation and would otherwise let this
   * loop cycle through every token repeatedly in one synchronous pass, burning through
   * `MAX_LAUNCH_FAILURES` for all of them at once instead of one attempt each. Stopping the
   * instant a re-peeked head has already been attempted this pass leaves the rest to the
   * periodic sweep or the next idle/dead event — one launch attempt per token per drain pass. */
  private async drainWorkerQueue(): Promise<void> {
    const attempted = new Set<string>();
    for (;;) {
      const token = this.deps.state.workerAdmission.queue[0];
      if (token === undefined) return;
      if (attempted.has(token)) return;
      attempted.add(token);
      const shouldContinue = await this.mutateClaim(token, () => {
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
   * place rather than relaunching a pane that is already running — unless that client is alive
   * but not idle, which is not stale (dropping it would strand a live assignment) but also not
   * yet promotable; that case stops the drain instead. Returns `true` if `drainWorkerQueue`
   * should loop again (progress was made, a stale entry was dropped, this attempt was a no-op
   * because the head moved on, an unlaunchable head was retired at `MAX_LAUNCH_FAILURES` so the
   * rest of the queue is not permanently blocked behind it, or a below-threshold failure
   * rotated the head to the tail so the next-in-line gets a turn) or `false` if it should stop
   * (still at cap, queue empty, or the head is a live claim that is not yet idle again). */
  private async promoteQueuedWorker(token: string): Promise<boolean> {
    const decision = await this.withAdmissionLock(async (): Promise<PromotionDecision> => {
      const queue = this.deps.state.workerAdmission.queue;
      if (queue[0] !== token) return { kind: "retry" };
      if (this.runningWorkerCount() >= this.deps.config.workerCap) return { kind: "stop" };
      const claim = this.deps.state.roles[token];
      const parsed = parseRoleToken(this.deps.state.project, token);
      const treeKey =
        parsed && !("controller" in parsed) ? this.deps.rootForIssue(parsed.issue) : undefined;
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
        const client = this.deps.getWorkerClient(token);
        if (!client || !claim.sessionId) {
          // Truly stale: the client is gone (evicted/never connected) or a session was never
          // confirmed — the idle-resume-at-cap invariant this entry was queued under can never
          // hold again on its own. Drop and continue.
          queue.shift();
          return { kind: "stale" };
        }
        if (client.runState !== "idle") {
          // NOT stale — the client is alive and this claim still has a real pending
          // assignment. It is either mid-prompt (a rejected prompt's `runState` restoration
          // racing this same check — see `WorkerRpcClient.prompt`'s doc comment) or genuinely
          // busy running something else. Dropping it here would strand a live, still-queued
          // assignment with nothing left to redrive it. Leave it at the head and stop the
          // drain: the close handler (if it turns out dead) or the next idle event (if it is
          // just busy) re-triggers `promoteWorkerQueue` once the picture is clear.
          return { kind: "stop" };
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
    });

    if (decision.kind === "stale") {
      await this.deps.persist();
      return true;
    }
    if (decision.kind === "retry") return true;
    if (decision.kind === "stop") return false;

    if (decision.kind === "prompt") {
      try {
        await this.deps.promptExistingWorker(
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
        // head and retry the same broken prompt again).
        this.launching.delete(token);
        const claim = this.deps.state.roles[token];
        if (claim && "issue" in claim) {
          const failures = (claim.promptFailures ?? 0) + 1;
          claim.promptFailures = failures;
          if (failures >= MAX_LAUNCH_FAILURES && claim.locator) {
            // Persistently broken, not transient: `MAX_LAUNCH_FAILURES` consecutive rejections
            // against an already-live, already-idle-cached socket is not a fluke a retry will
            // fix — a worker that keeps refusing prompts needs replacing, the same recovery a
            // dead socket gets. Retires the pane and clears the locator (inside this same
            // roleLock — see `WorkerAdmissionDeps.retireDeadClaim`'s doc comment for why this
            // must never re-acquire it) so the still-queued assignment falls through to the
            // launch path's own threshold and `launch-failed` publish on the next drain,
            // instead of looping through the exact same broken prompt forever.
            await this.deps.retireDeadClaim(token, claim.locator);
          }
        }
        await this.deps.persist();
        return false;
      }
      // `promptExistingWorker` no longer releases the reservation itself — WorkerAdmission owns
      // that timing since it owns `this.launching`. No `promoteWorkerQueue()` call needed here:
      // this method's own caller (`drainWorkerQueue`) already continues its loop on a `true`
      // return, which is exactly "re-check the queue" — an extra trigger here would just start
      // a second, separately-tracked drain pass racing the one already in progress.
      this.launching.delete(token);
      await this.withAdmissionLock(async () => {
        const queue = this.deps.state.workerAdmission.queue;
        const index = queue.indexOf(token);
        if (index !== -1) queue.splice(index, 1);
      });
      await this.deps.persist();
      this.deps.publishArchitect(decision.treeKey, {
        type: "worker-started",
        issue: decision.issue,
        role: decision.role,
      });
      return true;
    }

    try {
      await this.deps.launchWorker(
        decision.treeKey,
        decision.issue,
        decision.role,
        decision.claim,
        decision.task
      );
      this.deps.publishArchitect(decision.treeKey, {
        type: "worker-started",
        issue: decision.issue,
        role: decision.role,
      });
      return true;
    } catch (error) {
      console.error(`[legion] failed to promote queued worker ${token}:`, error);
      // launchWorker's own catch deliberately rethrows both TreeClosingError and StopFailed
      // without touching `launchFailures` (see there) -- neither is an ordinary launch failure,
      // so bumping that counter or rotating the token to the tail would be wrong for both. They
      // still need different queue treatment, though.
      if (error instanceof TreeClosingError) {
        // This token's tree is confirmed gone. Unlike round 7's fix (leave it for
        // `closeTreeLocked`'s own `pruneQueueForTree` to remove during that tree's teardown),
        // reaching this case now means the tree was ALREADY closed with nothing left to prune it
        // — `spawnWorker`'s own entry check rejects a fresh enqueue against an already-closed
        // tree before this point (see `isTreeGone`), so this is now only reachable via the
        // narrower "closed mid-decision" race, but a dead entry here would otherwise wedge every
        // worker queued behind it forever, since no `closeTreeLocked` call is left running to
        // prune it. Drops the token and clears whatever locator-less claim `launchWorker`'s
        // entry-check check left untouched (its own post-open branch, if that is the leg that
        // fired instead, has already deleted its fresh claim itself before throwing).
        await this.withAdmissionLock(async () => {
          const queue = this.deps.state.workerAdmission.queue;
          const index = queue.indexOf(token);
          if (index !== -1) queue.splice(index, 1);
          delete this.deps.state.roles[token];
        });
        await this.deps.persist();
        return true;
      }
      if (error instanceof StopFailed) {
        // A post-open retire's real kill-pane failure -- the pane this launch just opened might
        // still be alive under the locator `launchWorker` already wrote and persisted before
        // throwing. Leave the token and its claim exactly as `launchWorker` left them for
        // `closeTreeLocked` (or the periodic sweep) to retry -- dropping it here, unlike the
        // TreeClosingError case above, could strand a genuinely still-running pane with no
        // durable record and no queue entry either.
        return true;
      }
      // launchWorker's own catch already recorded launchFailures on the claim and published
      // `launch-failed` exactly once, at the tick it crosses MAX_LAUNCH_FAILURES. Mirror tree
      // admission: once that threshold is hit, retire the head from the queue so an
      // unlaunchable entry (e.g. a missing session file — a deterministic, permanent failure)
      // never blocks every other queued worker behind it forever. Below the threshold, rotate
      // the head to the tail instead of leaving it parked at the head: nothing else ever retries
      // a queue head on its own (there is no periodic promotion tick besides the boot/cap-change
      // sweep and this drain loop itself), so leaving it in place would starve every worker
      // queued behind it until this exact token's own next `spawnWorker` call happens to retry
      // it. Always returns `true` (continue) here — `drainWorkerQueue`'s own attempted-token
      // tracking, not this return value, is what stops the pass once the next re-peeked head
      // (rotated or otherwise) has already been given a turn this pass, so a rotation never
      // burns through every queued token's `MAX_LAUNCH_FAILURES` attempts in one synchronous
      // drain.
      await this.withAdmissionLock(async () => {
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
      });
      await this.deps.persist();
      return true;
    } finally {
      // This releases the reservation regardless of outcome (`launchWorker` itself no longer
      // touches admission bookkeeping — see its doc comment). No `promoteWorkerQueue()` call
      // here, deliberately: the success path already re-checked the queue by returning `true`,
      // which is exactly what makes `drainWorkerQueue`'s own loop continue; the failure path
      // also always returns `true` above and relies on `drainWorkerQueue`'s own attempted-token
      // tracking to stop the pass — calling `promoteWorkerQueue` here would start a brand-new,
      // separately-tracked drain pass and defeat that tracking.
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
}
