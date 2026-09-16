import { randomUUID } from "node:crypto";
import type { SpawnWorkerResponse } from "@legion/contracts";
import { type IssueKey, type LegionRole, parseRoleToken } from "@legion/contracts";
import {
  activePhaseLabel,
  isBystanderCatchup,
  type LegionState,
  type PendingAssignment,
  samePendingTask,
  type WorkerRoleClaim,
} from "./legion-state";
import {
  PromptNotStarted,
  type PromptNotStartedReason,
  StopFailed,
  TreeClosingError,
} from "./process-errors";
import type { WorkerRpcClient } from "./worker-rpc";

/** Bounds a claim's `launchFailures` (cold-launch attempts) or `promptFailures` (queued
 * idle-resume prompt attempts against an already-live worker) before a permanently-broken
 * queue head is retired instead of retried forever. Shared with `ProcessManager.launchWorker`,
 * which increments and publishes against the same threshold for the launch path. */
export const MAX_LAUNCH_FAILURES = 3;

/** Bounds a claim's `promptRetires`: how many times a live worker may be retired and relaunched
 * for prompt failures before the retirement is terminal and the tree's architect hears
 * `worker-died` (LEGION-93). Two: one relaunch. Judged `>=`, so a claim that already ended in
 * `worker-died` and is spawned again by the architect gets one cold launch, `MAX_LAUNCH_FAILURES`
 * prompts, and `worker-died` again — the architect's own spawn is the retry, never a reset. */
export const MAX_PROMPT_RETIRES = 2;

/** What `recordPromptFailure` decided at the threshold: `"relaunch"` retires the pane and leaves
 * the task queued for a cold `--resume` relaunch on the next drain; `"died"` retires it
 * terminally — queue entry removed, `worker-died` published, nothing relaunches it. */
export type PromptRetireVerdict = "relaunch" | "died";

interface QueueMutation {
  changed: boolean;
  newlyQueued: boolean;
}

/** Where a queued worker sits relative to the others: lower runs first, FIFO within a tier. Work
 * that finishes an open pull request outranks work that opens a new one — a merger turns an
 * approved PR into a merge, a reviewer turns a tested PR into an approval, a tester turns an
 * implementation into a verdict, and an implementer on an issue whose PR already exists (a
 * corrective round, the retro commit) keeps that PR moving; a planner or sub-architect only
 * prepares work; an implementer on an issue with no PR yet creates one more PR that will need
 * every tier above it. Without this the queue drains in arrival order, so a burst of new
 * implementers starves every tester, reviewer, and merger behind it and work in progress only
 * grows (Sami, 2026-09-13: "If it starts implementers on new issues before it starts testers
 * and reviewers on existing issues, it'll just build up work in progress forever"). */
export function workerPriority(state: LegionState, token: string): number {
  const parsed = parseRoleToken(state.project, token);
  if (!parsed || "controller" in parsed) return 6;
  switch (parsed.role) {
    case "merger":
      return 0;
    case "reviewer":
      return 1;
    case "tester":
      return 2;
    case "implementer":
      return Object.values(state.prs).some((pr) => pr.key === parsed.issue) ? 3 : 5;
    case "planner":
    case "architect":
      return 4;
  }
}

/** Stable in-place ordering of the running-worker queue: clean tokens by `workerPriority`, ties
 * in arrival order (FIFO within a tier); then, behind every clean token whatever its tier, the
 * tokens that have failed to launch and not booted since (`launchFailures > 0`, reset only by a
 * confirmed `/worker/ready`), in the same tier-then-arrival order. That bottom shelf is where the
 * old arrival-order queue's rotate-to-tail left a failed token too, and it is what keeps a
 * persistently failing merger from taking the head on every pass: two drains can overlap (a
 * promotion trigger fires while a pass is awaiting persistence), and a fresh pass's sort must not
 * lift a token the other pass just rotated back above the workers it was rotated behind. Called
 * under `admissionLock` at the start of every drain pass and nowhere else: a tier can change
 * while a token waits (an implementer's PR opens) and a queue persisted by an older daemon is in
 * arrival order, so the pass that consumes the queue orders it first; an enqueue that lands
 * mid-pass only appends. */
export function orderWorkerQueue(state: LegionState): void {
  const queue = state.workerAdmission.queue;
  const ranked = queue.map((token, index) => {
    const claim = state.roles[token];
    const failed = claim && "issue" in claim && (claim.launchFailures ?? 0) > 0 ? 1 : 0;
    return { token, index, failed, tier: workerPriority(state, token) };
  });
  ranked.sort((a, b) => a.failed - b.failed || a.tier - b.tier || a.index - b.index);
  for (let i = 0; i < ranked.length; i += 1) queue[i] = ranked[i].token;
}

/** The outcome of `promoteQueuedWorker`'s admission-lock critical section: `"launch"` carries
 * everything needed to run `launchWorker` outside the lock; `"prompt"` carries an already-live,
 * cached, idle client to prompt in place (the resume-at-cap path queued via
 * `enqueueIdleWorker` never touched the locator, so there is a real pane to reuse instead of
 * relaunching) — its token deliberately stays on the queue until the worker's turn is observed
 * to start (`promptExistingWorker`'s commit removes it; see `promoteQueuedWorker`'s handling
 * below), so a prompt that is refused or acknowledged without a turn never strands the
 * assignment. `"stop"` covers both "still at cap" and a queued locator-carrying claim whose
 * client is alive but not currently idle (mid-prompt or genuinely busy) — neither is stale, so
 * neither drops the entry; only `"stale"` does that. `"skip"` rotates an unconfirmed
 * locator-carrying head so other queued work can proceed while its ready retry owns recovery.
 * A bystander's catch-up is cleared from its claim as it is dropped so neither delivery branch
 * ever runs for it. */
type PromotionDecision =
  | { kind: "retry" }
  | { kind: "stop" }
  | { kind: "stale" }
  | { kind: "skip" }
  | {
      kind: "prompt";
      client: WorkerRpcClient;
      issue: IssueKey;
      role: LegionRole;
      sessionId: string;
      pending: PendingAssignment;
    }
  | {
      kind: "launch";
      claim: WorkerRoleClaim;
      treeKey: IssueKey;
      issue: IssueKey;
      role: LegionRole;
      pending: PendingAssignment;
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
  /** Publishes `worker-queued`/`worker-started` to the architect that owns the payload's issue
   * (`owningArchitect`, LEGION-86) -- resolved by the implementation, never passed in. */
  publishArchitect(
    payload:
      | { type: "worker-queued"; issue: IssueKey; role: LegionRole }
      | { type: "worker-started"; issue: IssueKey; role: LegionRole }
  ): void;
  launchWorker(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    pending: PendingAssignment
  ): Promise<void>;
  /** Prompts a live worker and commits the delivery — `phases[issue]` for an assignment,
   * `pendingAssignment` cleared, `promptFailures` reset, the token's queue entry removed
   * (`removeFromQueue`), persisted — only once the worker's turn is observed to start. Throws
   * the shim's own error for a refused prompt and `PromptNotStarted` for an acknowledgement no
   * turn followed within the bound; both leave the claim and queue exactly as they were. */
  promptExistingWorker(
    client: WorkerRpcClient,
    token: string,
    issue: IssueKey,
    role: LegionRole,
    sessionId: string,
    pending: PendingAssignment,
    afterPrompt?: () => void
  ): Promise<void>;
  /** Retires a persistently-broken worker's pane and clears its locator, returning whether this
   * invocation retired the still-current claim. The caller already holds this token's
   * `roleLaunchQueue` critical section (see `ProcessManager.retireDeadWorkerLocatorLocked`), so the
   * prompt-failure circuit breaker can call it without re-acquiring and deadlocking. */
  retireDeadClaim(
    token: string,
    locator: NonNullable<WorkerRoleClaim["locator"]>,
    verdict: PromptRetireVerdict
  ): Promise<boolean>;
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

/** Owns the running-worker cap: the admission decision, the priority queue (`workerPriority`:
 * finishing work first, FIFO within a tier; ordered at the start of every drain pass), the reservation set that
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
  /** Boot's launch hold on the worker side. False from construction, flipped once by
   * `enableWorkerPromotion` (called from `ProcessManager.enableLaunches()` after the daemon's
   * boot probes pass and its HTTP `api` is assigned). While false, two things hold: every
   * worker-queue drain (`promoteWorkerQueue`'s fire-and-forget trigger and
   * `reconcileWorkerAdmission`'s explicit call alike — neither bypasses this) is a logged no-op,
   * and a fresh `launchOrQueue`/`resumeOrQueueExisting` decision treats the cap as full, so a
   * `spawnWorker` arriving during boot queues (`worker-queued`) instead of opening a pane. The
   * drain gate also protects boot's worker reconnect: `reconnectWorkers` can synchronously
   * trigger a chain of `get_state` -> `isStreaming: false` -> `onIdle` -> `promoteWorkerQueue` ->
   * `launchWorker` -> `mintWorkerBootToken`, and a launch there would open a pane the probe has
   * not yet cleared. */
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

  /** The only way `workerPromotionEnabled` is ever set to `true`. Called once, from
   * `ProcessManager.enableLaunches()`, before the first explicit `reconcileWorkerAdmission()` —
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
   * live pane to reuse. The existing claim is mutated in place so `agentId`, `generation`, and
   * `launchFailures` survive. A replacement preserves its queue position and first `queuedAt`.
   */
  private enqueueClaimForLaunch(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    pending: PendingAssignment
  ): QueueMutation {
    const target: WorkerRoleClaim = claim ?? { issue, role };
    const queue = this.deps.state.workerAdmission.queue;
    const newlyQueued = !queue.includes(token);
    const previous = target.pendingAssignment;
    const replaced = !samePendingTask(previous, pending);
    if (replaced) {
      target.pendingAssignment = {
        ...pending,
        queuedAt: newlyQueued ? pending.queuedAt : (previous?.queuedAt ?? pending.queuedAt),
      };
    }
    const hadLocator = target.locator !== undefined;
    const resumeSessionFile = target.locator?.ompSessionFile ?? target.resumeSessionFile;
    delete target.locator;
    if (resumeSessionFile) target.resumeSessionFile = resumeSessionFile;
    else delete target.resumeSessionFile;
    this.deps.state.roles[token] = target;
    if (newlyQueued) queue.push(token);
    return { changed: replaced || newlyQueued || hadLocator, newlyQueued };
  }

  /**
   * Pure in-memory mutation (same `admissionLock`-only calling convention as
   * `enqueueClaimForLaunch`) for a worker whose pane is alive and idle but the cap has no free
   * slot. A replacement keeps its original `queuedAt` and does not become a new queue entry.
   */
  private enqueueIdleWorker(
    token: string,
    claim: WorkerRoleClaim,
    pending: PendingAssignment
  ): QueueMutation {
    const queue = this.deps.state.workerAdmission.queue;
    const newlyQueued = !queue.includes(token);
    const previous = claim.pendingAssignment;
    const replaced = !samePendingTask(previous, pending);
    if (replaced) {
      claim.pendingAssignment = {
        ...pending,
        queuedAt: newlyQueued ? pending.queuedAt : (previous?.queuedAt ?? pending.queuedAt),
      };
    }
    if (newlyQueued) queue.push(token);
    return { changed: replaced || newlyQueued, newlyQueued };
  }

  /** The one rule for telling the architect `worker-queued`: once, when the role's token joins
   * the queue, and only for an architect assignment. A replacement is silent; a catch-up is too. */
  private publishQueued(
    newlyQueued: boolean,
    issue: IssueKey,
    role: LegionRole,
    pending: PendingAssignment
  ): void {
    if (!newlyQueued || pending.kind !== "assignment") return;
    this.deps.publishArchitect({ type: "worker-queued", issue, role });
  }

  /**
   * Launches a brand-new worker pane when a running-worker slot is available, or queues the task
   * otherwise. A new queue entry is announced once to the architect; replacements are silent.
   * The actual launch runs outside the lock so it cannot block other admission decisions.
   */
  async launchOrQueue(
    token: string,
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim | undefined,
    pending: PendingAssignment
  ): Promise<SpawnWorkerResponse> {
    const decision = await this.withAdmissionLock(async () => {
      if (!this.workerPromotionEnabled || this.runningWorkerCount() >= this.deps.config.workerCap) {
        return {
          admitted: false as const,
          ...this.enqueueClaimForLaunch(token, issue, role, claim, pending),
        };
      }
      this.launching.add(token);
      return { admitted: true as const };
    });
    if (!decision.admitted) {
      if (decision.changed) await this.deps.persist();
      this.publishQueued(decision.newlyQueued, issue, role, pending);
      return { status: "queued", roleToken: token };
    }
    try {
      await this.deps.launchWorker(treeKey, issue, role, claim, pending);
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
   * separately from `enqueueForRetry` so a caller that must fold this push into a larger durable
   * transition can do so without triggering a save of its own. */
  async enqueueForRetryPending(token: string): Promise<void> {
    await this.withAdmissionLock(async () => {
      const queue = this.deps.state.workerAdmission.queue;
      if (!queue.includes(token)) queue.push(token);
    });
  }

  /**
   * Enqueues an already-cleared claim — its locator already gone, its `pendingAssignment`
   * already the task to retry — onto the running-worker queue, persists it, then triggers
   * the normal cap-aware, role-locked drain to relaunch it. Never launches directly itself: a
   * boot the watchdog (or a restart-time reconnect probe) confirmed dead must go through the
   * exact same admission decision as any other launch, not bypass it (over-admission past the
   * cap, or a second pane racing a concurrent same-role spawn, are exactly what that decision
   * exists to prevent). Safe to call before `enableWorkerPromotion()` — the queue push always
   * lands; `promoteWorkerQueue()` itself no-ops (logging) until promotion is enabled, so a
   * restart-time caller (`reconnectWorkers`, run during boot's launch hold) leaves the token for
   * the boot sequence's own `reconcileWorkerAdmission()` to promote once the hold releases.
   */
  async enqueueForRetry(token: string): Promise<void> {
    await this.enqueueForRetryPending(token);
    await this.deps.persist();
    this.promoteWorkerQueue();
  }

  /** Removes `token`'s queue entry, if any, under the shared admission lock — the commit
   * half of a delivered prompt (`ProcessManager.commitPromptDelivery`), called once the worker's
   * turn is observed to start, whether in the bound or late. A no-op for a token that was never
   * queued (the direct `resumed` path and `/worker/ready`'s delivery). No persist: the caller
   * folds the removal into its own commit save. */
  async removeFromQueue(token: string): Promise<void> {
    await this.withAdmissionLock(async () => {
      const queue = this.deps.state.workerAdmission.queue;
      const index = queue.indexOf(token);
      if (index !== -1) queue.splice(index, 1);
    });
  }

  /** Counts one failed prompt attempt against `token`'s claim — a refusal, an acknowledgement no
   * turn followed within the bound (`PromptNotStarted`, `"no-turn"`), or a whole exhausted
   * ready-time delivery cycle (`ProcessManager.workerReady`, LEGION-39) — and, at
   * `MAX_LAUNCH_FAILURES` with a locator still recorded, retires the worker: that many consecutive
   * failures against an already-live, already-idle-cached socket is not a fluke a retry will fix —
   * a worker that keeps refusing or swallowing prompts needs replacing, the same recovery a dead
   * socket gets. Retiring the pane and clearing the locator (inside the caller's role lock — see
   * `WorkerAdmissionDeps.retireDeadClaim`'s doc comment for why this must never re-acquire it)
   * lets the still-queued assignment fall through to the normal launch path rather than looping
   * through the exact same broken prompt forever. The token joins the running-worker queue before
   * `retireDeadClaim` persists its cleared locator, so that one durable retirement state can never
   * leave the pending assignment locator-less and unqueued after a crash. The enqueue is
   * idempotent (`enqueueForRetryPending` never duplicates): a no-op for the promote path and for
   * `queueUnstartedPrompt`, whose tokens are already queued, and the one thing that makes the
   * ready path's cold `--resume` relaunch happen — a token `launchOrQueue` admitted and launched
   * directly is on no queue. Returns the resulting failure count, whether retirement landed, and
   * whether it reached the terminal `died` verdict. On `StopFailed`, neither counter moves, the
   * locator and queue entry remain, and the next failure tries the same retirement again. No
   * persist and no drain trigger: every caller saves as part of its own larger transition, the
   * promote path deliberately never drains from its failure branch (it would re-peek the same
   * head), and the ready path triggers one itself after its critical section, exactly as
   * `markWorkerDead` does. Every caller holds `token`'s role lock. */
  async recordPromptFailure(
    token: string
  ): Promise<{ failures: number; retired: boolean; died: boolean }> {
    const claim = this.deps.state.roles[token];
    if (!claim || !("issue" in claim)) return { failures: 0, retired: false, died: false };
    const previousFailures = claim.promptFailures ?? 0;
    const failures = previousFailures + 1;
    if (failures < MAX_LAUNCH_FAILURES || !claim.locator) {
      claim.promptFailures = failures;
      return { failures, retired: false, died: false };
    }
    const retires = (claim.promptRetires ?? 0) + 1;
    const verdict: PromptRetireVerdict = retires >= MAX_PROMPT_RETIRES ? "died" : "relaunch";
    await this.enqueueForRetryPending(token);
    const retired = await this.deps.retireDeadClaim(token, claim.locator, verdict);
    if (!retired) return { failures: previousFailures, retired: false, died: false };
    claim.promptRetires = retires;
    console.error(
      `[legion] ${token}: retiring after ${MAX_LAUNCH_FAILURES} prompts with no turn started; relaunch cycle ${retires} (bound ${MAX_PROMPT_RETIRES})`
    );
    claim.promptFailures = verdict === "relaunch" ? 0 : failures;
    return { failures, retired: true, died: verdict === "died" };
  }

  private rotatePendingDelivery(pending: PendingAssignment): void {
    pending.deliveryId = randomUUID();
  }

  /** Queues a task whose acknowledged prompt did not start a turn. A `no-turn` failure rotates
   * its delivery id before persistence so the next drain sends a new prompt, preserving the
   * LEGION-60 retry behavior; this is intentionally distinct from a transport timeout, which
   * retries the same id so the shim can suppress duplicate execution. A `"socket-closed"` reason
   * is counted against nothing: the socket-close handler owns retirement. No drain starts here,
   * so a merely slow worker is not re-prompted immediately. The caller holds `token`'s role lock. */
  async queueUnstartedPrompt(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim,
    pending: PendingAssignment,
    reason: PromptNotStartedReason
  ): Promise<void> {
    let queued: QueueMutation = { changed: false, newlyQueued: false };
    await this.withAdmissionLock(async () => {
      queued = this.enqueueIdleWorker(token, claim, pending);
    });
    const stored = claim.pendingAssignment ?? pending;
    const failure =
      reason === "no-turn" || reason === "refused-late"
        ? await this.recordPromptFailure(token)
        : undefined;
    if (reason === "no-turn" || reason === "refused-late") this.rotatePendingDelivery(stored);
    await this.deps.persist();
    if (!failure?.died) this.publishQueued(queued.newlyQueued, issue, role, stored);
  }

  /**
   * Decides whether an already-connected, already-idle client below the running-worker cap
   * should be prompted immediately in place, or queued via `enqueueIdleWorker` instead — either
   * because it is at cap (persisted, and `worker-queued` published through `publishQueued` — once,
   * for an assignment that newly joined the queue — before this resolves) or because it is not
   * currently idle at all (`"running"`/`"unknown"`): injecting a prompt into a client mid-turn is
   * never safe, so that case is queued exactly like the at-cap one, and delivered once the
   * client's own idle transition (`onIdle` -> `promoteWorkerQueue`) re-drains the queue and
   * finds it genuinely idle. Below cap and idle, reserves the token via `reserve` for the gap
   * between this decision and `client.prompt()` actually flipping `runState` away from
   * `"idle"`, then — exactly like `launchOrQueue` owns its own `launchWorker` call — runs the
   * actual `deps.promptExistingWorker` call itself and releases the reservation (and re-checks
   * the queue) the instant it settles, so the caller (`ProcessManager.spawnWorker`) never has to
   * remember admission bookkeeping around its own prompt call. `"resumed"` means the worker's
   * turn was observed to start; a prompt the worker acknowledged without starting a turn
   * (`PromptNotStarted`) answers `"queued"` instead, the task queued for promotion through
   * `queueUnstartedPrompt` — the architect hears `worker-queued` now (for an assignment) and
   * `worker-started` on the retry or the late start — or, when that failure is the one that
   * exhausts `MAX_PROMPT_RETIRES`, still `"queued"` (the HTTP answer is part of the daemon API
   * contract) with `worker-died` as the notice instead. A refused prompt still throws.
   */
  async resumeOrQueueExisting(
    token: string,
    issue: IssueKey,
    role: LegionRole,
    claim: WorkerRoleClaim,
    sessionId: string,
    pending: PendingAssignment,
    client: WorkerRpcClient
  ): Promise<{ kind: "queued" } | { kind: "resumed" }> {
    const decision = await this.withAdmissionLock(async () => {
      if (
        client.runState !== "idle" ||
        !this.workerPromotionEnabled ||
        this.runningWorkerCount() >= this.deps.config.workerCap
      ) {
        return { prompt: false as const, ...this.enqueueIdleWorker(token, claim, pending) };
      }
      this.reserve(token);
      return { prompt: true as const };
    });
    if (!decision.prompt) {
      if (decision.changed) await this.deps.persist();
      this.publishQueued(decision.newlyQueued, issue, role, pending);
      return { kind: "queued" };
    }
    let notStarted = false;
    try {
      await this.deps.promptExistingWorker(client, token, issue, role, sessionId, pending);
    } catch (error) {
      if (!(error instanceof PromptNotStarted)) throw error;
      notStarted = true;
      console.error(`[legion] ${token}: ${error.message}; queued for promotion`);
      await this.queueUnstartedPrompt(token, issue, role, claim, pending, error.reason);
      return { kind: "queued" };
    } finally {
      this.release(token);
      this.deps.onAdmissionEvent?.(token, "reservation-released");
      if (!notStarted) {
        this.promoteWorkerQueue();
        this.deps.onAdmissionEvent?.(token, "queue-drain-triggered");
      }
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

  /** Removes every queued token whose tree is `treeKey` — called by `closeTreeLocked` as
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

  /** Promotes queued workers in priority order (`workerPriority`; FIFO within a tier), one at a
   * time, while a running-worker slot is
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
    // The one place the queue is ordered (see `orderWorkerQueue`): once, before the pass peeks.
    await this.withAdmissionLock(async () => orderWorkerQueue(this.deps.state));
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
      if (isBystanderCatchup(this.deps.state, parsed.issue, parsed.role, claim.pendingAssignment)) {
        // The phase moved on while this catch-up waited for a slot: whether the worker is a live
        // idle pane or a retired claim, delivering it would only prompt or relaunch a finished
        // worker with nothing to do. Dropped and drained like any other consumed entry; the
        // architect's next spawn_worker is what resumes a finished worker.
        console.info(
          `[legion] dropping queued catch-up for ${token}: ${parsed.issue}'s active phase is ${activePhaseLabel(this.deps.state, parsed.issue)}; only spawn_worker resumes a finished worker`
        );
        delete claim.pendingAssignment;
        queue.shift();
        return { kind: "stale" };
      }
      if (claim.locator !== undefined) {
        const client = this.deps.getWorkerClient(token);
        if (!client || !claim.sessionId) {
          // Truly stale: the client is gone (evicted/never connected) or a session was never
          // established — the idle-resume-at-cap invariant this entry was queued under can
          // never hold again on its own. Drop and continue.
          queue.shift();
          return { kind: "stale" };
        }
        if (claim.readyConfirmedAt === undefined) {
          // An in-flight ready retry owns this live but unconfirmed boot. Rotate rather than
          // blocking unrelated queued workers behind it; its retry or boot watchdog will retry
          // this claim, while a breaker StopFailed leaves the locator intact.
          queue.shift();
          queue.push(token);
          return { kind: "skip" };
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
          issue: parsed.issue,
          role: parsed.role,
          sessionId: claim.sessionId,
          pending: claim.pendingAssignment,
        };
      }
      this.launching.add(token);
      return {
        kind: "launch",
        claim,
        treeKey,
        issue: parsed.issue,
        role: parsed.role,
        pending: claim.pendingAssignment,
      };
    });

    if (decision.kind === "retry") return true;
    if (decision.kind === "stale" || decision.kind === "skip") {
      await this.deps.persist();
      return true;
    }
    if (decision.kind === "stop") return false;

    if (decision.kind === "prompt") {
      try {
        await this.deps.promptExistingWorker(
          decision.client,
          token,
          decision.issue,
          decision.role,
          decision.sessionId,
          decision.pending
        );
      } catch (error) {
        // One line per attempt; a `PromptNotStarted`'s own message names the token and the
        // observation (`get_state: isStreaming=false`, `get_state failed: …`, `socket closed`).
        console.error(`[legion] failed to prompt queued worker ${token}:`, error);
        // The token remains queued and `promptExistingWorker` commits nothing before a started
        // turn. A no-turn result rotates the delivery ID so the next drain is a new OMP prompt;
        // a refusal preserves the ID because OMP never accepted that prompt. Stop draining
        // instead of looping straight back into the same broken client (an immediate
        // `promoteWorkerQueue()` call would re-peek this head and widen the double-send window).
        // A socket that closed during the wait is counted against nothing: its close handler
        // alone retires it, preventing a second threshold retirement race.
        this.launching.delete(token);
        if (error instanceof PromptNotStarted && error.reason === "no-turn") {
          this.rotatePendingDelivery(decision.pending);
        }
        if (!(error instanceof PromptNotStarted) || error.reason === "no-turn") {
          await this.recordPromptFailure(token);
        }
        await this.deps.persist();
        return false;
      }
      // `promptExistingWorker`'s commit already removed this token from the queue and persisted;
      // WorkerAdmission still owns the reservation's timing since it owns `this.launching`. No
      // `promoteWorkerQueue()` call needed here: this method's own caller (`drainWorkerQueue`)
      // already continues its loop on a `true` return, which is exactly "re-check the queue" — an
      // extra trigger here would just start a second, separately-tracked drain pass racing the
      // one already in progress.
      this.launching.delete(token);
      this.deps.publishArchitect({
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
        decision.pending
      );
      this.deps.publishArchitect({
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
        // This token's tree is confirmed gone. `spawnWorker`'s own entry check already rejects a
        // fresh enqueue against an already-closed tree before this point (see `isTreeGone`), so
        // reaching this case now only happens via the narrower "closed mid-decision" race, with
        // no `closeTreeLocked` call left running to prune this entry via its own
        // `pruneQueueForTree` -- a dead entry here would otherwise wedge every worker queued
        // behind it forever. Drops the token and clears whatever locator-less claim
        // `launchWorker`'s entry-check check left untouched (its own post-open branch, if that
        // is the leg that fired instead, has already deleted its fresh claim itself before
        // throwing).
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
          // To the tail for the rest of this pass — and `orderWorkerQueue` keeps it behind every
          // clean token on later passes too, until a confirmed ready resets `launchFailures`.
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
