import { randomUUID } from "node:crypto";
import { mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  assertLegionProjectToken,
  ISSUE_STATUSES,
  type IssueKey,
  type IssueStatus,
  isLegionProjectToken,
  isLegionRole,
  LEGION_ROLES,
  type LegionRole,
  roleToken,
  type SpawnWorkerResponse,
} from "@legion/contracts";
import { z } from "zod";
import type { CheckRunRef } from "../state/types";
import type { Locator } from "./runtime";

/** Which read last set a fence's timestamp: a real GitHub webhook, or the daemon's own resync (board GraphQL/CI-status) read. At an identical clock a resync read is GitHub's authoritative source of truth and wins a tie against a disagreeing webhook observation. */
export type UpdateSource = "webhook" | "resync";

/** A daemon status intent is valid only against the issue's own local status at the moment the
 * write failed (`statusAtRecord`). At retry, a single remote read resolves it: the remote status
 * already matching the intended `status` means it landed; the remote status disagreeing with
 * `statusAtRecord` means a real status change (human or another daemon-owned transition)
 * superseded it; otherwise the retry PATCHes again. See `resync.ts`'s `retryPendingStatusWrites`. */
export interface PendingStatusWrite {
  status: IssueStatus;
  statusAtRecord?: IssueStatus;
}

export interface IssueNode {
  key: IssueKey;
  title: string;
  parent?: IssueKey;
  children: IssueKey[];
  finalCommentRef?: string;
  /** The issue's Dispatch lifecycle status, set from `issue.created`/`issue.updated` events.
   * Absent on an issue this daemon has not yet observed through the Dispatch lane. A `"done"`
   * status is this daemon's only notion of closed — GitHub's separate open/closed issue state
   * (and its labels, `released` flag, and backlog marker) no longer exist on a Legion issue. */
  status?: IssueStatus;
  /** The highest Dispatch event `seq` this daemon has applied to this issue, across every event
   * type keyed on it (`issue.created`/`updated`/`closed` on itself, `child.status` delivered to
   * it as a parent, `artifact.approved`/`artifact.changes_requested`/`artifact.version` against
   * its own design gate) — the at-most-once fence: `reduceDispatchEvent` drops any incoming event
   * with `seq <= lastAppliedSeq` before mutating state or emitting an effect, and stamps this to
   * the incoming `seq` after processing every event it does not drop, including a no-op one, so a
   * redelivered no-op can't be reprocessed either. `child.status` and the artifact events against
   * an issue this daemon has never created a node for stay unfenced by this field, but that is
   * harmless: their own reducers are already no-ops without a node (`child.status`'s target) or a
   * registered gate on that node (the artifact events'), so there is nothing for a redelivery to
   * corrupt. Undefined until this issue's first Dispatch event is applied — Dispatch's own
   * `Issue.last_seq` is the source this daemon is fencing against, so a millisecond-precision
   * `updated_at` comparison is neither needed nor safe (same-millisecond redeliveries are
   * indistinguishable by clock alone; this replaces that former fence). */
  lastAppliedSeq?: number;
}

export interface TreeState {
  root: IssueKey;
  generation: number;
  locator?: Locator;
  status: "queued" | "active" | "lingering" | "dead" | "launch-failed" | "closed";
  lingerUntil?: string;
  launchFailures: number;
  /** Set only after `/process/ready` has confirmed this exact `generation` -- `spawnTree`
   * clears it (implicitly, by never carrying it forward onto a fresh locator) on every root
   * launch; `confirmRootReady` sets it. Mirrors `WorkerRoleClaim`'s own `readyConfirmedAt`, but
   * lives on the tree itself so a boot-time reconnect pass (`reconnectRoots`) can decide whether
   * an active tree's root registration deadline needs re-arming without resolving its architect
   * claim first. */
  readyConfirmedAt?: number;
  /** The root's OMP session file, kept across a cleared pane so the next launch resumes the same
   * agent with `--resume` instead of starting fresh -- the tree-level twin of
   * `WorkerRoleClaim.resumeSessionFile`. Written by `recordRootExit` (a root that exited on its
   * own: copied from its locator's `ompSessionFile` before the locator is deleted) and by
   * `resurrectDeadTree` immediately after clearing a root's locator, before it starts a
   * replacement; read by `spawnRoot`, which resumes whenever it is set whatever its caller asked,
   * and by `spawnTree` as the last fallback for the resume file; cleared by `spawnTree` when it
   * records the fresh locator, and when the tree reaches `launch-failed` (a controller re-admit
   * starts fresh, as before). LEGION-83. */
  resumeSessionFile?: string;
}

export interface PrState {
  key: IssueKey;
  repo: `${string}/${string}`;
  number: number;
  headSha: string;
  headUpdatedAt?: number;
  /** The source of `headUpdatedAt`'s last write; see `UpdateSource`. */
  headUpdatedAtSource?: UpdateSource;
  /** The last SETTLED verdict for this head; a rerun in flight makes it unknown at the next resync. */
  verdict: "green" | "red" | null;
  /** Failing check runs: reported by the listener or by GitHub's rollup. */
  failing: string[];
  /** Failing commit statuses (no check run): only GitHub's rollup reports or retires these. */
  failingStatuses: string[];
  ciSettledAt: number | null;
  /** The settlement's attempt set: the latest check-run id per check name, sorted by name. Null until a settlement or a rollup with check runs is accepted. */
  ciCheckRuns: CheckRunRef[] | null;
  ciSettlementGeneration: number | null;
  ciSnapshot: string | null;
  /** True while a terminal GitHub read holds the tie at the stored attempt set; released when the set advances or a pending read clears it. */
  ciReconciled: boolean;
  fixAttempts: number;
  reviewDecision?: "approved" | "changes_requested";
  /** Present exactly when the current `headSha`'s arrival in `resetPrHead` incremented
   * `fixAttempts` (prior verdict was red and no pending push classified this sha handoff-only). A
   * later handoff-only push webhook whose `after` equals `headSha` takes the attempt back and
   * deletes it; the next `resetPrHead` sets or deletes it afresh. Never written as `false` —
   * literal `true` keeps one representation of "not counted" (absent), mirroring
   * `reviewDecision`'s set/delete handling. */
  headCounted?: true;
  /** The latest push webhook's classification for a head that has not arrived yet (push `after`
   * !== `headSha`). A later push for another not-yet-arrived sha overwrites it (latest push wins,
   * one slot). `resetPrHead` consumes (deletes) it when a head with that exact sha arrives — from
   * the synchronize webhook or from resync's GitHub read alike. A push for the CURRENT head never
   * touches this slot (it only takes back). Keyed by sha, so a stale slot can only ever describe
   * the commit it names. */
  pendingPush?: { sha: string; handoffOnly: boolean };
  /** The `fixAttempts` value the last `pr-blocked` was published for. `reduceCiEmission`
   * publishes `pr-blocked` only when `fixAttempts >= maxFixAttempts` AND `fixAttempts !==
   * blockedAttempts`, then records `fixAttempts` here. A take-back whose pre-decrement
   * `fixAttempts` equals `blockedAttempts` deletes it (a pr-blocked published for that count is
   * forgotten). */
  blockedAttempts?: number;
}

/** A prompt queued on a worker claim for delivery once the worker is ready or admitted.
 * `assignment` is an architect's `spawn_worker` task: the one delivery that makes its role the
 * issue's active phase (`state.phases[issue]`). `catchup` is the daemon's own `catchup-worker`
 * recovery prompt (`resumeWorker`), which never changes the phase. `deliveryId` identifies one
 * logical prompt: v31 -> v32 stamps every persisted task missing it before runtime validation. */
export interface PendingAssignment {
  kind: "assignment" | "catchup";
  task: string;
  /** ISO time this role first entered the queue. An identical re-send and a replacement in the
   * same queue slot keep it; a newly queued task receives the current time. */
  queuedAt: string;
  deliveryId: string;
}

/** The identical re-send rule (LEGION-102): a task of the same kind and text as the one a role
 * already holds changes nothing — no persist, no `worker-queued`, the entry keeps its position
 * and its `queuedAt`. */
export function samePendingTask(
  existing: PendingAssignment | undefined,
  next: Pick<PendingAssignment, "kind" | "task">
): boolean {
  return existing !== undefined && existing.kind === next.kind && existing.task === next.task;
}

export interface WorkerRoleClaim {
  issue: IssueKey;
  role: string;
  /** Set only by `/worker/started`, for the current generation (`launchWorker` clears it on every
   * relaunch). Once set, a registration from any other session at this generation is refused. */
  sessionId?: string;
  /** Set only after `/worker/ready` has connected its shim and delivered any pending assignment;
   * `sessionId` is intentionally established earlier by `/worker/started` for capability auth. */
  readyConfirmedAt?: number;
  agentId?: string;
  locator?: Locator;
  generation?: number;
  pendingAssignment?: PendingAssignment;
  launchFailures?: number;
  /** Consecutive failed prompt attempts against this claim's already-live socket — a refusal,
   * or an acknowledgement no turn followed within the bound (`PromptNotStarted`) — reset to 0
   * once a prompt's turn is observed to start. The live-worker prompt paths' own failure
   * counter, mirroring `launchFailures` for the cold-launch path. At `MAX_LAUNCH_FAILURES` the
   * worker is considered persistently broken: its locator is retired and cleared so the
   * still-queued assignment falls through to the launch path's own threshold on the next drain. */
  promptFailures?: number;
  /** Retirements this claim has taken for prompt failures (`recordPromptFailure` at
   * `MAX_LAUNCH_FAILURES`) — relaunch cycles of a live worker that acknowledges prompts but never
   * starts a turn. Carried onto the relaunched claim by `launchWorker`, never reset by a relaunch or
   * an architect's `spawn_worker`; deleted only once a prompt's turn is observed to start
   * (`commitPromptDelivery`). At `MAX_PROMPT_RETIRES` the retirement is terminal: `worker-died`. */
  promptRetires?: number;
  bootTokenHash?: string;
  /** The dead worker's last OMP session file, preserved when its locator is cleared (retired or
   * found dead on reconnect) so a later promoted/resumed launch still resumes the same agent via
   * `--resume` instead of starting fresh. Cleared once a launch records a fresh locator. */
  resumeSessionFile?: string;
  /** The session id `spawnWorker`/`launchWorker` expected this generation to resume, persisted
   * alongside `bootTokenHash` so a daemon restart before `/worker/started` still enforces the
   * same-agent invariant that the in-memory boot token's `expectedSessionId` otherwise carries. */
  expectedSessionId?: string;
}

export interface ControllerRoleClaim {
  role: "controller";
  sessionId: string;
}

export type RoleClaim = WorkerRoleClaim | ControllerRoleClaim;
export interface SpawnCapability {
  tree: IssueKey;
  issue: IssueKey;
  role: string;
  /** The process generation the boot token was minted for. Written by `mintBootToken` so a
   * root pod re-hello-ing after a daemon restart (the in-memory mint record gone) still has its
   * generation checked; absent only on a record persisted before it was written. */
  generation?: number;
}

/** A controller-bound notification whose text cannot be re-derived from durable state once the
 * controller reconnects. It is queued on a 404 no-holder response and drained in order by
 * `/controller/ready`. */
export interface ControllerPendingNotice {
  payloadJson: string;
  eventId: string;
}

/** The design gate on a root issue: a human's approval of the root's spec document, pinned to a
 * document version the way a pull-request review is pinned to a commit. `artifactId` is the
 * Dispatch artifact id of that document and `latestVersion` the highest version number this
 * daemon has seen for it (from `register_gate`'s `version`, then every `artifact.version` event);
 * `approvedVersion` is the version the latest `artifact.approved` pinned, absent until a human
 * approves and deleted again by `artifact.changes_requested`. The gate is open exactly when
 * `approvedVersion === latestVersion` (`designGateOpen`): a spec edited after approval is closed
 * again until the new version is approved. */
export interface DesignGate {
  artifactId: string;
  latestVersion: number;
  approvedVersion?: number;
}

/** Whether the design gate is open: approved at the document's current version. */
export function designGateOpen(gate: DesignGate): boolean {
  return gate.approvedVersion !== undefined && gate.approvedVersion === gate.latestVersion;
}

/** Whether an issue with this Dispatch status has left the waiting line: a human-owned status
 * other than `todo` (`triage`, `icebox`, `backlog`) or `done`. A `queued` tree or an
 * `admission.queue` entry for such an issue is stale -- the reducer drops it on the status event
 * (the `dequeue` effect), and the boot and resync sweeps (`staleQueueEntryReason`) drop one that
 * is already there. `todo` is the waiting line itself; a daemon-owned status (`in_progress`,
 * `testing`, `needs_review`, `retro`) on a queued entry is the legitimate shape of a tree boot
 * demoted back to the queue after admission wrote `in_progress` but before its pane was recorded,
 * never stale. Exhaustive over `IssueStatus`: a new status is a compile error here, not a silent
 * default. */
export function isStaleQueuedStatus(status: IssueStatus): boolean {
  switch (status) {
    case "triage":
    case "icebox":
    case "backlog":
    case "done":
      return true;
    case "todo":
    case "in_progress":
    case "testing":
    case "needs_review":
    case "retro":
      return false;
    default: {
      const unhandled: never = status;
      throw new Error(`unknown issue status: ${JSON.stringify(unhandled)}`);
    }
  }
}

/** Why an `admission.queue` entry must go, or `undefined` when it may stay: `unknown issue` when
 * no `issues` node exists for it (nothing could ever run it correctly, and it would hold a queue
 * position forever), else `Dispatch status "<status>"` when the node's status
 * `isStaleQueuedStatus`. A node with no status yet (never described by the Dispatch lane) stays:
 * the predicate has nothing to judge, and the resync drift heal assigns its status. Shared by
 * `ProcessManager.reconcileAdmission`'s boot sweep and `resync.ts`'s periodic sweep so the two
 * cannot disagree; the text is the tail of their `dropped <KEY> from the admission queue at
 * <boot|resync>: <reason>` line. */
export function staleQueueEntryReason(state: LegionState, issue: IssueKey): string | undefined {
  const node = state.issues[issue];
  if (!node) return "unknown issue";
  if (node.status !== undefined && isStaleQueuedStatus(node.status)) {
    return `Dispatch status "${node.status}"`;
  }
  return undefined;
}

/** A fulfilled `spawn_worker` result kept long enough for a transport retry to survive a daemon
 * restart. It intentionally includes the original task so a reused id with different work is
 * still refused, while the response lets a repeat return without touching ProcessManager. */
export interface PersistedSpawnRequest {
  tree: IssueKey;
  issue: IssueKey;
  role: LegionRole;
  task: string;
  result: SpawnWorkerResponse;
  settledAt: number;
}

export interface LegionState {
  version: 33;
  project: string;
  issues: Record<IssueKey, IssueNode>;
  trees: Record<IssueKey, TreeState>;
  /** The controller's interactive OMP pane. Unlike a root's or worker's locator it carries no
   * shim socket: `ompSessionFile` is what `ensureController` resumes when the pane is found
   * dead. Shares the runtime-discriminated `Locator` union; `migrateV32State` strips the socket
   * the headless controller used to have. */
  controllerLocator?: Locator;
  roles: Record<string, RoleClaim>;
  spawnCapabilities: Record<string, SpawnCapability>;
  prs: Record<string, PrState>;
  prByBranch: Record<string, string>;
  /** A closed-unmerged PR's `headUpdatedAt` at close, keyed by `repo#number`: keeps an older `opened`/`synchronize` redelivery from recreating a PR this state has already deleted (see `pullRequest` in reducers.ts). Pruned past 30 days by `pruneStalePrTombstones`. */
  prTombstones: Record<string, number>;
  admission: { cap: number; active: IssueKey[]; queue: IssueKey[] };
  /** FIFO role tokens waiting for a running-worker slot (config.workerCap); the currently-running
   * set is derived from live RPC frames and kept in-memory by ProcessManager, never persisted. */
  workerAdmission: { queue: string[] };
  /** Fulfilled `spawn_worker` requests retained for `SPAWN_REQUEST_RETENTION_MS` so a retry after
   * a daemon restart returns its original result instead of re-delivering the task. */
  spawnRequests: Record<string, PersistedSpawnRequest>;
  /** The issue's active phase, keyed by issue. Written in exactly one place — the delivery of an
   * architect assignment (`promptExistingWorker`, kind `assignment`), which stamps `assignedAt`
   * (ISO) with the delivery time — and marked `completed` by `/phase/complete` when no architect is
   * live to receive the completion. `assignedAt` is absent on a record persisted before v29 and is
   * never backfilled: the refusal log prints it as `unknown`. */
  phases: Record<
    IssueKey,
    | {
        phase: string;
        sessionId: string;
        assignedAt?: string;
        completed?: { summary: string; at: string };
      }
    | undefined
  >;
  controllerCapabilityHash?: string;
  controllerPendingNotices: ControllerPendingNotice[];
  /** The design gate per root issue (see `DesignGate`). Absent until the architect registers
   * one via `/legion/v1/gates/register`. */
  gates: Record<IssueKey, DesignGate>;
  /** A daemon-owned lifecycle status write that failed its Dispatch PATCH. The recorded status
   * fence (`PendingStatusWrite.statusAtRecord`) lets resync discard the intent once a real
   * status change has superseded it for that issue. */
  pendingStatusWrites: Record<IssueKey, PendingStatusWrite>;
}

/** Reads a root issue's spec document from Dispatch for `migrateV27State`: the id of the issue's
 * primary artifact and the highest version number it carries. */
export type SpecArtifactResolver = (
  issue: IssueKey
) => Promise<{ artifactId: string; latestVersion: number }>;

export interface LegionStateInit {
  project: string;
  cap: number;
  /** Required only when the file on disk is a v27 state with a design gate to keep (see
   * `migrateV27State`); every other load never calls it. */
  resolveSpecArtifact?: SpecArtifactResolver;
  /** Called once when `migrateV32State` strips a headless shim socket from `controllerLocator`
   * (the LEGION-16 upgrade): the daemon does not kill that still-running headless pane — it
   * probes alive and keeps its claim — so the operator must, after this boot. `index.ts` logs
   * the exact command; a load that strips nothing never calls it. */
  onHeadlessControllerStripped?: (locator: { tmuxSession?: string; tmuxPaneId?: string }) => void;
}

/** Legion's own issue key: the Dispatch key (`^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-7`). */
export const ISSUE_KEY_PATTERN = /^[A-Z][A-Z0-9]*-[0-9]+$/;
const REPOSITORY_PATTERN = /^[^/]+\/[^/]+$/;
const ENVOY_ROLE_TOKEN_PATTERN = /^[a-z0-9][a-z0-9_-]*$/;

const IssueKeySchema = z.custom<IssueKey>(
  (value) => typeof value === "string" && ISSUE_KEY_PATTERN.test(value),
  { message: "Expected a Dispatch PROJECT-number issue key" }
);
const RepositorySchema = z.custom<`${string}/${string}`>(
  (value) => typeof value === "string" && REPOSITORY_PATTERN.test(value),
  { message: "Expected owner/repo repository" }
);
const IssueNodeSchema = z
  .object({
    key: IssueKeySchema,
    title: z.string(),
    parent: IssueKeySchema.optional(),
    children: z.array(IssueKeySchema),
    finalCommentRef: z.string().optional(),
    status: z.enum(ISSUE_STATUSES).optional(),
    lastAppliedSeq: z.number().int().positive().optional(),
  })
  .strict();

const TmuxLocatorSchema = z
  .object({
    runtime: z.literal("tmux"),
    tmuxSession: z.string(),
    tmuxWindowId: z.string(),
    tmuxPaneId: z.string().optional(),
    /** The pane's root pid and that process's `/proc/<pid>/stat` start ticks, recorded by
     * `TmuxRuntime` at launch: the identity a pane must still carry to be trusted as this
     * locator's process (`runtime.ts`'s `TmuxLocator`). Absent only on a legacy locator. */
    panePid: z.number().int().positive().optional(),
    paneStartTicks: z.number().int().nonnegative().optional(),
    socketPath: z.string().optional(),
    ompSessionFile: z.string().optional(),
  })
  .strict();
/** No persisted state has ever carried a kubernetes locator — only `TmuxRuntime` has written
 * locators — so `roleToken`, a required field, costs no migration; the discriminated union
 * already admitted the member. */
const K8sLocatorSchema = z
  .object({
    runtime: z.literal("kubernetes"),
    namespace: z.string().min(1),
    podName: z.string().min(1),
    podUid: z.string().min(1),
    pvcName: z.string().min(1),
    roleToken: z.string().regex(ENVOY_ROLE_TOKEN_PATTERN),
    ompSessionFile: z.string().optional(),
  })
  .strict();
/** Every persisted locator names the runtime that owns its process (`runtime.ts`'s `Locator`). */
const LocatorSchema = z.discriminatedUnion("runtime", [TmuxLocatorSchema, K8sLocatorSchema]);

const TreeStateSchema = z
  .object({
    root: IssueKeySchema,
    generation: z.number().int().nonnegative(),
    locator: LocatorSchema.optional(),
    status: z.enum(["queued", "active", "lingering", "dead", "launch-failed", "closed"]),
    lingerUntil: z.string().optional(),
    launchFailures: z.number().int().nonnegative(),
    readyConfirmedAt: z.number().int().nonnegative().optional(),
    resumeSessionFile: z.string().optional(),
  })
  .strict();
const CheckRunRefSchema = z
  .object({ name: z.string().min(1), id: z.number().int().positive() })
  .strict();
/** The attempt set as persisted: strictly increasing names, so it is sorted and duplicate-free. */
const CheckRunSetSchema = z
  .array(CheckRunRefSchema)
  .refine(
    (runs) => runs.every((run, index) => index === 0 || (runs[index - 1]?.name ?? "") < run.name),
    {
      message: "ciCheckRuns must be sorted by name without duplicates",
    }
  );

const PrStateSchema = z
  .object({
    key: IssueKeySchema,
    repo: RepositorySchema,
    number: z.number().int().nonnegative(),
    headSha: z.string(),
    headUpdatedAt: z.number().optional(),
    headUpdatedAtSource: z.enum(["webhook", "resync"]).optional(),
    verdict: z.enum(["green", "red"]).nullable(),
    failing: z.array(z.string()),
    failingStatuses: z.array(z.string()),
    ciSettledAt: z.number().nullable(),
    ciCheckRuns: CheckRunSetSchema.nullable(),
    ciSettlementGeneration: z.number().int().nonnegative().nullable(),
    ciSnapshot: z.string().nullable(),
    ciReconciled: z.boolean(),
    fixAttempts: z.number().int().nonnegative(),
    reviewDecision: z.enum(["approved", "changes_requested"]).optional(),
    headCounted: z.literal(true).optional(),
    pendingPush: z
      .object({ sha: z.string().min(1), handoffOnly: z.boolean() })
      .strict()
      .optional(),
    blockedAttempts: z.number().int().nonnegative().optional(),
  })
  .strict();
const PendingAssignmentSchema = z
  .object({
    kind: z.enum(["assignment", "catchup"]),
    task: z.string(),
    queuedAt: z.string(),
    deliveryId: z.string().uuid(),
  })
  .strict();
const WorkerRoleClaimSchema = z
  .object({
    issue: IssueKeySchema,
    role: z.string(),
    sessionId: z.string().optional(),
    readyConfirmedAt: z.number().int().nonnegative().optional(),
    agentId: z.string().optional(),
    locator: LocatorSchema.optional(),
    generation: z.number().int().nonnegative().optional(),
    pendingAssignment: PendingAssignmentSchema.optional(),
    launchFailures: z.number().int().nonnegative().optional(),
    promptFailures: z.number().int().nonnegative().optional(),
    promptRetires: z.number().int().nonnegative().optional(),
    bootTokenHash: z.string().optional(),
    resumeSessionFile: z.string().optional(),
    expectedSessionId: z.string().optional(),
  })
  .strict()
  // A worker's tmux locator always carries its pane id and shim socket (the tmux runtime records
  // both on every spawn); one without them is a corrupt record that must fail here, at load, not
  // later at `connect`/`stop`. Tree and controller locators keep both optional: a root recorded
  // before the pane-id field existed is still a schema-valid record, but an identity-less one --
  // `TmuxRuntime.verifyPaneProcess` never confirms it, so its first probe reports it dead and it
  // is resurrected onto a fully-recorded locator, and its stop returns as already gone (the
  // graceful shutdown still goes out over its socket; nothing is ever killed for it).
  .superRefine((claim, context) => {
    if (claim.locator?.runtime !== "tmux") return;
    for (const field of ["tmuxPaneId", "socketPath"] as const) {
      if (claim.locator[field] === undefined) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          path: ["locator", field],
          message: `worker claim ${claim.issue}/${claim.role} has a tmux locator without ${field}`,
        });
      }
    }
  });
const ControllerRoleClaimSchema = z
  .object({
    role: z.literal("controller"),
    sessionId: z.string(),
  })
  .strict();
const RoleClaimSchema = z.union([WorkerRoleClaimSchema, ControllerRoleClaimSchema]);
const SpawnCapabilitySchema = z
  .object({
    tree: IssueKeySchema,
    issue: IssueKeySchema,
    role: z.string(),
    generation: z.number().int().nonnegative().optional(),
  })
  .strict();
const PhaseSchema = z
  .object({
    phase: z.string(),
    sessionId: z.string(),
    assignedAt: z.string().optional(),
    completed: z.object({ summary: z.string(), at: z.string() }).strict().optional(),
  })
  .strict();
const ControllerPendingNoticeSchema = z
  .object({
    payloadJson: z.string(),
    eventId: z.string(),
  })
  .strict();
const PersistedSpawnRequestSchema = z
  .object({
    tree: IssueKeySchema,
    issue: IssueKeySchema,
    role: z.enum(LEGION_ROLES),
    task: z.string().min(1),
    // A future SpawnWorker response may add fields while this ten-minute record remains on disk;
    // retaining it must not make a newly deployed daemon refuse to start.
    result: z
      .object({
        status: z.enum(["spawned", "resumed", "queued"]),
        roleToken: z.string().min(1),
      })
      .passthrough(),
    settledAt: z.number().nonnegative(),
  })
  .strict();

// The controller's pane runs interactive OMP with no `legion worker-shim`, so its locator never
// has a socket: one carrying `socketPath` is a corrupt record (or a pre-v33 file that skipped
// `migrateV32State`) and fails here, at load — the same load-time check `WorkerRoleClaimSchema`
// makes in the other direction for a worker's locator.
const ControllerLocatorSchema = LocatorSchema.superRefine((locator, context) => {
  if (locator.runtime === "tmux" && locator.socketPath !== undefined) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      path: ["socketPath"],
      message: "controller locator carries a shim socket; the controller pane has none",
    });
  }
});
const LegionStateSchema = z
  .object({
    version: z.literal(33),
    project: z.string().refine(isLegionProjectToken, {
      message: "Expected valid Legion project token",
    }),
    issues: z.record(IssueKeySchema, IssueNodeSchema),
    trees: z.record(IssueKeySchema, TreeStateSchema),
    controllerLocator: ControllerLocatorSchema.optional(),
    roles: z.record(z.string().regex(ENVOY_ROLE_TOKEN_PATTERN), RoleClaimSchema),
    spawnCapabilities: z.record(z.string().regex(/^[a-f0-9]{64}$/), SpawnCapabilitySchema),
    prs: z.record(z.string(), PrStateSchema),
    prByBranch: z.record(z.string(), z.string()),
    prTombstones: z.record(z.string(), z.number()).default({}),
    admission: z
      .object({
        cap: z.number().int().nonnegative(),
        active: z.array(IssueKeySchema),
        queue: z.array(IssueKeySchema),
      })
      .strict(),
    workerAdmission: z
      .object({
        queue: z.array(z.string().regex(ENVOY_ROLE_TOKEN_PATTERN)),
      })
      .strict(),
    spawnRequests: z.record(z.string().uuid(), PersistedSpawnRequestSchema),
    phases: z.record(IssueKeySchema, PhaseSchema),
    controllerCapabilityHash: z
      .string()
      .regex(/^[a-f0-9]{64}$/)
      .optional(),
    controllerPendingNotices: z.array(ControllerPendingNoticeSchema).default([]),
    gates: z
      .record(
        IssueKeySchema,
        z
          .object({
            artifactId: z.string().min(1),
            latestVersion: z.number().int().positive(),
            approvedVersion: z.number().int().positive().optional(),
          })
          .strict()
      )
      .default({}),
    pendingStatusWrites: z
      .record(
        IssueKeySchema,
        z
          .object({
            status: z.enum(ISSUE_STATUSES),
            statusAtRecord: z.enum(ISSUE_STATUSES).optional(),
          })
          .strict()
      )
      .default({}),
  })
  .strict();

function hasErrnoCode(error: unknown, code: string): boolean {
  return typeof error === "object" && error !== null && "code" in error && error.code === code;
}

export function newLegionState(project: string, cap: number): LegionState {
  assertLegionProjectToken(project);

  return {
    version: 33,
    project,
    issues: {},
    trees: {},
    roles: {},
    spawnCapabilities: {},
    prs: {},
    prByBranch: {},
    prTombstones: {},
    admission: { cap, active: [], queue: [] },
    workerAdmission: { queue: [] },
    spawnRequests: {},
    phases: {},
    controllerPendingNotices: [],
    gates: {},
    pendingStatusWrites: {},
  };
}

/** True when `role` is `issue`'s active phase: the phase the architect most recently assigned,
 * not yet completed. A `completed` record is a finished phase awaiting replay to the architect,
 * so its role is no longer active. */
export function isActivePhase(state: LegionState, issue: IssueKey, role: string): boolean {
  const phase = state.phases[issue];
  return phase !== undefined && !phase.completed && phase.phase === role;
}

/** The role `issue`'s active phase names, for log lines: `none` when no phase is active (absent,
 * or completed and awaiting replay to the architect). */
export function activePhaseLabel(state: LegionState, issue: IssueKey): string {
  const phase = state.phases[issue];
  return phase !== undefined && !phase.completed ? phase.phase : "none";
}

/** True when `role` is a phase worker on `issue` that is not its active phase -- a bystander: a
 * finished (or superseded) worker whose next task comes only from the architect's `spawn_worker`.
 * The one definition every bystander judgement uses (`resumeWorker`'s decision whether to queue a
 * catch-up, `isBystanderCatchup` at delivery, `handleWorkerStarted`'s registration log), so the
 * sub-architect rationale lives here once: an architect (`role === "architect"` on a child issue)
 * is never a bystander, because it is never its child's active phase -- once it has spawned a
 * planner, `phases[child]` names that phase worker -- yet it parks for the life of its subtree and
 * a catch-up is its only recovery path. The root architect never holds a phase either; its
 * recovery is `resurrect`, and the one of these paths it does enter (`resumeWorker`, via the
 * durable lane's `onUndeliverable`) exits at the no-resumable-identity guard before this judgement
 * is made. */
export function isBystanderRole(state: LegionState, issue: IssueKey, role: string): boolean {
  return role !== "architect" && !isActivePhase(state, issue, role);
}

/** True when `pending` is the daemon's own catch-up queued for a bystander role (`isBystanderRole`),
 * judged at delivery time (a queued promotion or `/worker/ready`), not only when `resumeWorker`
 * decides whether to queue one: the phase can move on while the catch-up waits behind the cap or
 * a boot. Such a catch-up is dropped rather than prompted or relaunched; only the architect's
 * next `spawn_worker` resumes a finished worker. */
export function isBystanderCatchup(
  state: LegionState,
  issue: IssueKey,
  role: string,
  pending: PendingAssignment | undefined
): boolean {
  return pending?.kind === "catchup" && isBystanderRole(state, issue, role);
}

/** Verifies that a persisted phase names a current Legion role and returns that role. */
export function assertKnownPhase(issue: IssueKey, phase: { phase: string }): LegionRole {
  if (!isLegionRole(phase.phase)) {
    throw new Error(
      `state.phases[${issue}] has an unrecognized phase: ${JSON.stringify(phase.phase)}`
    );
  }
  return phase.phase;
}

/** The tree that owns `issue` through its parent chain, if any: the nearest ancestor (never `issue`
 * itself) recorded in `state.trees`, unless that tree is `lingering` or `closed` -- a tree whose
 * architect is gone for good. `queued`, `active`, `dead`, and `launch-failed` all own: each is a
 * root tree that resumes (promotion, resurrection, the controller's re-admission). A child with no
 * owner is an orphan and admits as a root of its own. Shared by `reduceIssueUpdated` and the boot
 * repair `adoptOwnerlessChildTrees` so the two cannot disagree. Cycle-safe like `rootForIssue`. */
export function liveAncestorTree(state: LegionState, issue: IssueKey): TreeState | undefined {
  const seen = new Set<IssueKey>([issue]);
  let current = state.issues[issue]?.parent;
  while (current !== undefined && !seen.has(current)) {
    seen.add(current);
    const tree = state.trees[current];
    if (tree) {
      return tree.status === "lingering" || tree.status === "closed" ? undefined : tree;
    }
    current = state.issues[current]?.parent;
  }
  return undefined;
}

/** The architect that owns `issue`: the nearest architect claim at or above it, falling back to
 * the tree root. A claim, never a live pane, determines ownership; a dead sub-architect is
 * recovered through its own role. `role: "architect"` starts at the parent because a
 * sub-architect's own lifecycle wake belongs to the architect above it. The unreachable root
 * architect case returns its root; an ownership chain with no tree is invalid persisted state and
 * throws rather than silently choosing another route. */
export function owningArchitect(state: LegionState, issue: IssueKey, role?: LegionRole): IssueKey {
  const seen = new Set<IssueKey>();
  if (role === "architect" && state.issues[issue]?.parent === undefined && state.trees[issue]) {
    return issue;
  }
  let current = role === "architect" ? state.issues[issue]?.parent : issue;
  while (current !== undefined && !seen.has(current)) {
    seen.add(current);
    if (state.roles[roleToken(state.project, current, "architect")] !== undefined) return current;
    if (state.trees[current]) return current;
    current = state.issues[current]?.parent;
  }
  throw new Error(`No owning architect for ${issue}/${role ?? "issue"}: state has no tree`);
}

/** What `repairAdmissionDrift` changed: the trees it appended to `admission.active`, the entries
 * it removed (each with why), and -- when the repaired list is still longer than the cap -- the
 * occupancy it left in place. */
export interface AdmissionDriftRepair {
  added: IssueKey[];
  removed: Array<{ issue: IssueKey; reason: string }>;
  overCap?: { active: number; cap: number };
}

/** Restores the admission invariant in place (LEGION-83): every tree whose status is `active` is
 * in `admission.active` (appended; pulled out of `admission.queue` if it sits there), and every
 * `admission.active` entry names an `active` tree -- or a `dead` one, whose slot remains reserved
 * while its recovery is in flight. The reservation is admission bookkeeping only; it does not
 * prove a root process is live, so resync separately reports a dead root once no live resurrection
 * owns it. An entry whose tree is missing, `lingering`, `closed`, `launch-failed`, or `queued`,
 * and a duplicate of an earlier entry, is removed. Occupancy above `admission.cap` is reported,
 * never corrected: only a human raises or lowers occupancy by hand, and the queue drains as trees
 * close. Pure apart from the in-place mutation -- no I/O, no logging; the callers
 * (`ProcessManager.reconcileAdmission` at boot, `runResync` every interval) log its result
 * through `describeAdmissionDrift`, so the two cannot disagree on what a violation is. Stops
 * nothing. */
export function repairAdmissionDrift(state: LegionState): AdmissionDriftRepair {
  const admission = state.admission;
  const added: IssueKey[] = [];
  const removed: Array<{ issue: IssueKey; reason: string }> = [];
  const kept = new Set<IssueKey>();
  for (const issue of admission.active) {
    const tree = state.trees[issue];
    if (kept.has(issue)) {
      removed.push({ issue, reason: "duplicate entry" });
    } else if (!tree) {
      removed.push({ issue, reason: "no tree recorded" });
    } else if (tree.status !== "active" && tree.status !== "dead") {
      removed.push({ issue, reason: `tree is ${tree.status}` });
    } else {
      kept.add(issue);
    }
  }
  for (const tree of Object.values(state.trees)) {
    if (tree.status !== "active" || kept.has(tree.root)) continue;
    const queueIndex = admission.queue.indexOf(tree.root);
    if (queueIndex !== -1) admission.queue.splice(queueIndex, 1);
    kept.add(tree.root);
    added.push(tree.root);
  }
  // Spliced in place, never reassigned: `ProcessManager` reads `admission.active` by reference.
  if (added.length > 0 || removed.length > 0) {
    admission.active.splice(0, admission.active.length, ...kept);
  }
  if (admission.active.length > admission.cap) {
    return { added, removed, overCap: { active: admission.active.length, cap: admission.cap } };
  }
  return { added, removed };
}

/** The operator-facing lines for a repair: one per tree added, one per entry removed, and one
 * over-cap warning -- identical from boot and from resync (`source` names which). Empty for a
 * clean repair, so a consistent state logs nothing. */
export function describeAdmissionDrift(
  repair: AdmissionDriftRepair,
  source: "at boot" | "by resync"
): string[] {
  const lines = repair.added.map(
    (issue) =>
      `[legion] admission drift repaired ${source}: added ${issue} to admission.active (tree is active but held no slot)`
  );
  for (const { issue, reason } of repair.removed) {
    lines.push(
      `[legion] admission drift repaired ${source}: removed ${issue} from admission.active (${reason})`
    );
  }
  if (repair.overCap) {
    lines.push(
      `[legion] admission over cap ${source}: ${repair.overCap.active} active trees against a cap of ${repair.overCap.cap}; nothing is stopped and nothing is promoted until occupancy falls below the cap`
    );
  }
  return lines;
}

function migrateV5State(state: unknown): unknown {
  if (typeof state !== "object" || state === null || Array.isArray(state)) return state;
  if (!("version" in state) || state.version !== 5) return state;
  if (!("trees" in state) || typeof state.trees !== "object" || state.trees === null) {
    return { ...state, version: 6 };
  }

  const trees = Object.fromEntries(
    Object.entries(state.trees).map(([key, tree]) => {
      if (typeof tree !== "object" || tree === null || Array.isArray(tree)) return [key, tree];
      const { locator: _unsafeNameOnlyLocator, ...withoutLocator } = tree;
      return [key, withoutLocator];
    })
  );
  return { ...state, version: 6, trees };
}

function migrateV6State(state: unknown): unknown {
  if (typeof state !== "object" || state === null || Array.isArray(state)) return state;
  if (!("version" in state) || state.version !== 6) return state;
  const { attribution: _droppedAttribution, ...rest } = state as Record<string, unknown>;
  if (!("trees" in rest) || typeof rest.trees !== "object" || rest.trees === null) {
    return { ...rest, version: 7 };
  }

  const trees = Object.fromEntries(
    Object.entries(rest.trees).map(([key, tree]) => {
      if (typeof tree !== "object" || tree === null || Array.isArray(tree)) return [key, tree];
      if (!("locator" in tree) || typeof tree.locator !== "object" || tree.locator === null) {
        return [key, tree];
      }
      const { pid: _droppedPid, ...locator } = tree.locator as Record<string, unknown>;
      return [key, { ...tree, locator }];
    })
  );
  return { ...rest, version: 7, trees };
}

function recordValue(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function dispatchThreadKey(value: unknown): IssueKey | undefined {
  if (!recordValue(value)) return undefined;
  const { repo, thread } = value;
  if (typeof repo !== "string" || typeof thread !== "number" || !Number.isSafeInteger(thread)) {
    return undefined;
  }
  const [owner, name, ...extra] = repo.split("/");
  if (!owner || !name || extra.length > 0 || thread < 0) return undefined;
  // Reconstructs the legacy `owner/repo#number` GitHub issue key exactly as v7 state recorded
  // it, purely to match it against `state.issues`/`heldEvents` entries during this historical
  // migration — `@legion/contracts` no longer has a shared helper for this dead format.
  return `${owner}/${name}#${thread}`;
}

function withoutDispatchReplies(value: unknown): unknown {
  if (!Array.isArray(value)) return value;
  const retained = value.filter((held) => {
    if (!recordValue(held)) return true;
    return (
      typeof held.payloadJson !== "string" || !held.payloadJson.includes('"type":"dispatch-reply"')
    );
  });
  return retained.length === value.length ? value : retained;
}

function migrateV7State(state: unknown): unknown {
  if (typeof state !== "object" || state === null || Array.isArray(state)) return state;
  if (!("version" in state) || state.version !== 7) return state;
  const { dispatchThreads, ...rest } = state as Record<string, unknown>;
  const dispatchThreadKeys = new Set<string>(
    Array.isArray(dispatchThreads)
      ? dispatchThreads.map(dispatchThreadKey).filter((key): key is IssueKey => key !== undefined)
      : []
  );
  const issues = rest.issues;
  const trees = rest.trees;
  const migratedIssues = recordValue(issues)
    ? Object.fromEntries(
        Object.entries(issues)
          .filter(([key]) => !dispatchThreadKeys.has(key))
          .map(([key, node]) => {
            if (!recordValue(node) || !Array.isArray(node.children)) return [key, node];
            const retained = node.children.filter(
              (child) => typeof child !== "string" || !dispatchThreadKeys.has(child)
            );
            return retained.length === node.children.length
              ? [key, node]
              : [key, { ...node, children: retained }];
          })
      )
    : undefined;
  const migratedTrees = recordValue(trees)
    ? Object.fromEntries(
        Object.entries(trees).map(([key, tree]) => {
          if (!recordValue(tree)) return [key, tree];
          const retained = withoutDispatchReplies(tree.heldEvents);
          return retained === tree.heldEvents
            ? [key, tree]
            : [key, { ...tree, heldEvents: retained }];
        })
      )
    : undefined;
  return {
    ...rest,
    version: 8,
    ...(migratedIssues ? { issues: migratedIssues } : {}),
    ...(migratedTrees ? { trees: migratedTrees } : {}),
  };
}

function legacyChecksVerdict(checks: unknown): "green" | "red" | undefined {
  if (!recordValue(checks)) return undefined;
  const observations = Object.values(checks);
  if (
    observations.length === 0 ||
    !observations.every((check) => recordValue(check) && check.status === "completed")
  ) {
    return undefined;
  }
  return observations.some(
    (check) =>
      recordValue(check) &&
      check.conclusion !== "success" &&
      check.conclusion !== "neutral" &&
      check.conclusion !== "skipped"
  )
    ? "red"
    : "green";
}

function migrateV8State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 8) return state;
  const { prs, ...rest } = state;
  const migratedPrs = recordValue(prs)
    ? Object.fromEntries(
        Object.entries(prs).map(([key, pr]) => {
          if (!recordValue(pr)) return [key, pr];
          const {
            checks,
            firstRedEmitted,
            settledRedEmitted,
            greenEmitted,
            lastEventAt,
            ...withoutLegacyCi
          } = pr;
          const verdict =
            legacyChecksVerdict(checks) ??
            (greenEmitted === true
              ? "green"
              : firstRedEmitted === true || settledRedEmitted === true
                ? "red"
                : null);
          const ciSettledAt =
            verdict === null ||
            typeof lastEventAt !== "number" ||
            !Number.isSafeInteger(lastEventAt)
              ? null
              : lastEventAt;
          // v8 fenced nothing: every PR starts unfenced and unreconciled.
          return [
            key,
            {
              ...withoutLegacyCi,
              verdict,
              failing: [],
              failingStatuses: [],
              ciSettledAt,
              ciCheckRuns: null,
              ciSettlementGeneration: null,
              ciSnapshot: null,
              ciReconciled: false,
            },
          ];
        })
      )
    : undefined;
  return { ...rest, version: 12, ...(migratedPrs ? { prs: migratedPrs } : {}) };
}

function migrateV12State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 12) return state;
  return { ...state, version: 13 };
}

const PR_TOMBSTONE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;

/** Drops PR closed-tombstones older than 30 days: past that window an out-of-order `opened`/`synchronize` redelivery for the closed PR is no longer plausible, and leaving them forever would leak memory across a project's whole history. */
export function pruneStalePrTombstones(state: LegionState, now: number): void {
  for (const [key, closedAt] of Object.entries(state.prTombstones)) {
    if (now - closedAt > PR_TOMBSTONE_MAX_AGE_MS) delete state.prTombstones[key];
  }
}

/** v13 -> v14: worker role claims gain the per-process locator/identity fields; a legacy claim
 * that names a session but has no resumable locator drops that identity (it cannot be resumed). */
function migrateV13State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 13) return state;
  const { roles, ...rest } = state;
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) => {
          if (
            !recordValue(claim) ||
            !("issue" in claim) ||
            claim.locator !== undefined ||
            (claim.sessionId === undefined && claim.agentId === undefined)
          ) {
            return [key, claim];
          }
          // A role claim with a sessionId/agentId but no locator names a session this daemon has
          // no tmux pane or socket to resume; drop the identity so the next spawn starts fresh
          // instead of expecting a respawn to reproduce a session id it can never produce.
          const {
            sessionId: _droppedSessionId,
            agentId: _droppedAgentId,
            ...withoutIdentity
          } = claim;
          return [key, withoutIdentity];
        })
      )
    : undefined;
  return { ...rest, version: 14, ...(migratedRoles ? { roles: migratedRoles } : {}) };
}

/** v14 -> v15: `state.phases` entries gain an optional `completed` marker (set when a phase
 * finishes with no live architect holder to deliver it to); a pure version bump, since existing
 * phase entries already validate as-is with the field absent. */
function migrateV14State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 14) return state;
  return { ...state, version: 15 };
}

/** v15 -> v16: adds the FIFO running-worker admission queue (bounded by config.workerCap)
 * beside the existing tree admission queue; the currently-*running* set is derived live from
 * each worker's RPC socket by ProcessManager and is never persisted, so the migration only needs
 * to seed an empty queue. */
function migrateV15State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 15) return state;
  return { ...state, version: 16, workerAdmission: { queue: [] } };
}

/** v16 -> v17: drops the tree-scoped held-event plumbing — `tree.heldEvents` and
 * `tree.recoveryEvents` (a dead-architect exception's queued redelivery — held-event replay
 * under another name) — now that a missed wake is recovered by resuming the worker/root and
 * delivering a state-derived catch-up, never by replaying a queued raw event.
 * `controllerHeldEvents` is left untouched here and converted by the v17->v18 step immediately
 * below: unlike every other held event, a controller-bound one has no other source of truth to
 * recover it from (see `ControllerPendingNotice`'s own doc comment), so it is carried forward
 * rather than dropped. Never touches `workerAdmission`, added by the v15->v16 step immediately
 * above. */
function migrateV16State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 16) return state;
  const { trees, ...rest } = state;
  const migratedTrees = recordValue(trees)
    ? Object.fromEntries(
        Object.entries(trees).map(([key, tree]) => {
          if (!recordValue(tree)) return [key, tree];
          const {
            heldEvents: _droppedHeldEvents,
            recoveryEvents: _droppedRecoveryEvents,
            ...withoutHeldEvents
          } = tree;
          return [key, withoutHeldEvents];
        })
      )
    : trees;
  return { ...rest, version: 17, trees: migratedTrees };
}

/** v17 -> v18: adds `controllerPendingNotices` (see its own doc comment), converting any v16
 * `controllerHeldEvents` entry the v16->v17 step above carried forward unconverted —
 * `{role, payloadJson, heldAt, eventId}` — onto its final `{payloadJson, eventId}` shape. `role`
 * and `heldAt` have no counterpart here: every notice's role is always the controller by
 * construction, and durability never depended on when it was originally held, only that it still
 * is. Merged with (never overwriting) any `controllerPendingNotices` already present on the raw
 * state and deduped by `eventId` against both already-present notices and earlier converted
 * entries. A malformed entry (missing a `payloadJson`/`eventId` string pair) is logged and
 * discarded rather than carried forward broken. A state with no `controllerHeldEvents` gets an
 * empty array, same as before. */
function migrateV17State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 17) return state;
  const { controllerHeldEvents, controllerPendingNotices: existingNotices, ...rest } = state;
  const existingEventIds = new Set(
    Array.isArray(existingNotices)
      ? existingNotices
          .map((notice) => (recordValue(notice) ? notice.eventId : undefined))
          .filter((eventId): eventId is string => typeof eventId === "string")
      : []
  );
  const convertedNotices: Array<{ payloadJson: string; eventId: string }> = [];
  if (Array.isArray(controllerHeldEvents)) {
    for (const held of controllerHeldEvents) {
      if (
        recordValue(held) &&
        typeof held.payloadJson === "string" &&
        typeof held.eventId === "string"
      ) {
        if (existingEventIds.has(held.eventId)) continue;
        existingEventIds.add(held.eventId);
        convertedNotices.push({ payloadJson: held.payloadJson, eventId: held.eventId });
      } else {
        console.error(
          `[legion] discarding malformed v16 controllerHeldEvents entry during v17->v18 migration (expected {payloadJson: string, eventId: string}, got: ${JSON.stringify(held)})`
        );
      }
    }
  }
  const controllerPendingNotices = Array.isArray(existingNotices)
    ? [...existingNotices, ...convertedNotices]
    : convertedNotices;
  return { ...rest, version: 18, controllerPendingNotices };
}

/** v18 -> v19: switches issue lifecycle state to Dispatch. Pre-Dispatch trees and issues cannot
 * be represented safely, so they are rejected rather than discarded. */
function migrateV18State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 18) return state;
  if (
    (recordValue(state.trees) && Object.keys(state.trees).length > 0) ||
    (recordValue(state.issues) && Object.keys(state.issues).length > 0)
  ) {
    throw new Error("Cannot migrate a Legion state with active trees to the Dispatch lifecycle");
  }
  return { ...state, version: 19, gates: {}, pendingStatusWrites: {} };
}

/** v19 -> v20: `readyConfirmedAt` was added to `WorkerRoleClaim` without a version bump, so a
 * state persisted before that change has already-confirmed worker claims (`sessionId` and a
 * resumable `locator` both present -- the pre-upgrade meaning of "confirmed") with no
 * `readyConfirmedAt` at all. Left alone, `reconnectWorkers` would re-arm the boot watchdog for
 * every one of those healthy workers on the first restart after upgrade and retire them at the
 * registration deadline, since a worker that already finished `/worker/ready` in a prior process
 * lifetime never repeats it. Backfills `readyConfirmedAt` with this migration's own timestamp
 * for exactly those claims; a claim missing either field (never confirmed, or never even
 * started) is left untouched, and a claim that already carries `readyConfirmedAt` (persisted by
 * an already-patched daemon) is never overwritten. */
function migrateV19State(state: unknown, migratedAt: number): unknown {
  if (!recordValue(state) || state.version !== 19) return state;
  const { roles, ...rest } = state;
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) => {
          if (
            !recordValue(claim) ||
            !("issue" in claim) ||
            claim.readyConfirmedAt !== undefined ||
            claim.sessionId === undefined ||
            claim.locator === undefined
          ) {
            return [key, claim];
          }
          return [key, { ...claim, readyConfirmedAt: migratedAt }];
        })
      )
    : roles;
  return { ...rest, version: 20, roles: migratedRoles };
}

/** v20 -> v21: `readyConfirmedAt` is added to `TreeState` for the same reason
 * `migrateV19State` added it to `WorkerRoleClaim` -- a state persisted before this change has
 * active root trees whose pane is genuinely healthy (already reached `/process/ready` in a
 * prior process lifetime) with no `readyConfirmedAt` at all. Left alone, `reconnectRoots` would
 * arm a fresh registration deadline for every one of those healthy roots on the first restart
 * after upgrade and retire them at the deadline, since a root that already confirmed ready never
 * repeats it. Backfills `readyConfirmedAt` with this migration's own timestamp for every active
 * tree that has a recorded locator (the pre-upgrade meaning of "a root actually launched and is
 * presumably running fine"); a tree with no locator, not `active`, or already carrying
 * `readyConfirmedAt` (persisted by an already-patched daemon) is left untouched. */
function migrateV20State(state: unknown, migratedAt: number): unknown {
  if (!recordValue(state) || state.version !== 20) return state;
  const { trees, ...rest } = state;
  const migratedTrees = recordValue(trees)
    ? Object.fromEntries(
        Object.entries(trees).map(([key, tree]) => {
          if (
            !recordValue(tree) ||
            tree.status !== "active" ||
            tree.locator === undefined ||
            tree.readyConfirmedAt !== undefined
          ) {
            return [key, tree];
          }
          return [key, { ...tree, readyConfirmedAt: migratedAt }];
        })
      )
    : trees;
  return { ...rest, version: 21, trees: migratedTrees };
}

/** v21 -> v22: adds `approvalStatusPending` (see its own doc comment) -- a pure version bump,
 * since a state persisted before this change has no daemon-owned approval-status write in
 * flight to track: every prior attempt either already succeeded or was never recorded (it threw
 * and crashed the process instead). */
function migrateV21State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 21) return state;
  return { ...state, version: 22, approvalStatusPending: {} };
}

/** v22 -> v23: drops `approvalStatusPending` -- the daemon no longer writes a human-approval
 * backstop status; a human `APPROVED` PR review is validated by the repository's own branch
 * protection or CODEOWNERS rule, which Legion never reads or writes. */
function migrateV22State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 22) return state;
  const { approvalStatusPending: _droppedApprovalStatusPending, ...rest } = state;
  return { ...rest, version: 23 };
}

/** v23 -> v24: every persisted locator predates the runtime boundary and is a tmux one; tags
 * `TreeState.locator`, each `WorkerRoleClaim.locator`, and `controllerLocator` with
 * `runtime: "tmux"` so the discriminated `Locator` union can be strict. */
function migrateV23State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 23) return state;
  const { trees, roles, controllerLocator, ...rest } = state;
  const tag = (locator: unknown): unknown =>
    recordValue(locator) ? { ...locator, runtime: "tmux" } : locator;
  const migratedTrees = recordValue(trees)
    ? Object.fromEntries(
        Object.entries(trees).map(([key, tree]) =>
          recordValue(tree) && tree.locator !== undefined
            ? [key, { ...tree, locator: tag(tree.locator) }]
            : [key, tree]
        )
      )
    : trees;
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) =>
          recordValue(claim) && "issue" in claim && claim.locator !== undefined
            ? [key, { ...claim, locator: tag(claim.locator) }]
            : [key, claim]
        )
      )
    : roles;
  return {
    ...rest,
    version: 24,
    trees: migratedTrees,
    roles: migratedRoles,
    ...(controllerLocator === undefined ? {} : { controllerLocator: tag(controllerLocator) }),
  };
}

/** v24 -> v25: PrState gains the optional push-classification fields `headCounted`,
 * `pendingPush`, `blockedAttempts` (LEGION-33); a pure version bump — every existing record
 * validates with them absent, and absent is the correct starting value: no counted head with a
 * take-back pending, no push pending, no pr-blocked recorded (a red PR already past the limit
 * publishes pr-blocked once more on its next red settlement, then stops). */
function migrateV24State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 24) return state;
  return { ...state, version: 25 };
}

/** v25 -> v26: `WorkerRoleClaim.pendingAssignment` becomes `{ kind, task }`; each persisted
 * bare string is classified by its payload -- JSON whose `type` is `catchup-worker` is a
 * `catchup`, anything else (an architect's free-text task, or text that is not JSON) is an
 * `assignment`. Sits after the LEGION-33 v24 -> v25 bump above: a deployed daemon already
 * persists version 25 under that meaning, so this step must be the one that takes it to 26. */
function migrateV25State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 25) return state;
  const { roles, ...rest } = state;
  const classify = (task: string): { kind: "assignment" | "catchup"; task: string } => {
    let parsed: unknown;
    try {
      parsed = JSON.parse(task);
    } catch {
      return { kind: "assignment", task };
    }
    return {
      kind: recordValue(parsed) && parsed.type === "catchup-worker" ? "catchup" : "assignment",
      task,
    };
  };
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) =>
          recordValue(claim) && "issue" in claim && typeof claim.pendingAssignment === "string"
            ? [key, { ...claim, pendingAssignment: classify(claim.pendingAssignment) }]
            : [key, claim]
        )
      )
    : roles;
  return { ...rest, version: 26, roles: migratedRoles };
}

/** v26 -> v27: a tmux locator gains its recorded process identity (`panePid`,
 * `paneStartTicks`), both optional in the schema because a locator written before this version
 * has none -- and never gets one: `TmuxRuntime` verifies a pane only against an identity it
 * recorded at launch, so a legacy locator probes dead on its first probe and is resurrected onto
 * a fully-recorded one. Nothing to rewrite; the bump records that every locator saved from here
 * on carries the fields when its runtime wrote it. Sits after LEGION-37's v25 -> v26
 * (`pendingAssignment` classification) above: a deployed daemon already persists version 26 under
 * that meaning, so this step must be the one that takes it to 27. */
function migrateV26State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 26) return state;
  return { ...state, version: 27 };
}

/** v27 -> v28: the design gate becomes a version-pinned document approval (`DesignGate`) instead
 * of an ask id. A v27 record `{designAskId?, designApproved?}` (the shape v24 carried; LEGION-33's v24 -> v25 bump, LEGION-37's v25 -> v26 step, and LEGION-27's v26 -> v27 bump all left it untouched) cannot say which document or
 * version it refers to, so each kept gate is resolved once, here, from Dispatch: an approved gate
 * (a human answered `Approve`, or the daemon satisfied it under `gates.design: off`) becomes
 * approved at the spec's current version; a registered-but-unanswered one becomes an unapproved
 * gate on that document. Kept gates are exactly those whose tree record exists and is not closed;
 * a never-registered gate (neither field set), a gate with no tree record, and a closed tree's
 * gate are dropped with a log line naming the issue — a finished tree whose issue may be gone
 * from Dispatch must never keep the daemon from starting. A resolver failure for a kept gate, or
 * a kept gate with no resolver supplied, throws naming the issue: the daemon refuses to start
 * rather than mark a gate approved on a guess. */
async function migrateV27State(
  state: unknown,
  resolve: SpecArtifactResolver | undefined
): Promise<unknown> {
  if (!recordValue(state) || state.version !== 27) return state;
  const sourceGates = recordValue(state.gates) ? state.gates : {};
  const trees = recordValue(state.trees) ? state.trees : {};
  const gates: Record<string, DesignGate> = {};
  for (const [issue, source] of Object.entries(sourceGates)) {
    const gate = recordValue(source) ? source : {};
    const registered = typeof gate.designAskId === "string";
    const approved = typeof gate.designApproved === "string";
    const tree = recordValue(trees[issue]) ? trees[issue] : undefined;
    const dropReason =
      !registered && !approved
        ? "it was never registered"
        : !tree
          ? "it has no tree record"
          : tree.status === "closed"
            ? "its tree is closed"
            : undefined;
    if (dropReason !== undefined) {
      console.warn(
        `[legion] dropping the design gate for ${issue} during the v27->v28 migration: ${dropReason}`
      );
      continue;
    }
    if (!resolve) {
      throw new Error(
        `Cannot migrate the design gate for ${issue}: no Dispatch resolver was supplied`
      );
    }
    let spec: { artifactId: string; latestVersion: number };
    try {
      spec = await resolve(issue as IssueKey);
    } catch (error) {
      const cause = error instanceof Error ? error.message : String(error);
      throw new Error(`Cannot migrate the design gate for ${issue}: ${cause}`, { cause: error });
    }
    gates[issue] = approved
      ? {
          artifactId: spec.artifactId,
          latestVersion: spec.latestVersion,
          approvedVersion: spec.latestVersion,
        }
      : { artifactId: spec.artifactId, latestVersion: spec.latestVersion };
  }
  return { ...state, version: 28, gates };
}

/** One line per validation issue, naming where in the file it sits (`roles.<token>.locator.
 * roleToken`), so a corrupt record is refused with the field that broke it. A plain `z.union`
 * (`RoleClaimSchema`) collapses a type failure inside any option to a single `Invalid input`;
 * its per-option errors are expanded instead, since one of them is the real reason. */
function describeIssues(issues: z.ZodIssue[]): string[] {
  return issues.flatMap((issue) => {
    if (issue.code === z.ZodIssueCode.invalid_union) {
      return issue.unionErrors.flatMap((error) => describeIssues(error.issues));
    }
    return issue.path.length === 0
      ? [issue.message]
      : [`${issue.message} at ${issue.path.join(".")}`];
  });
}

/** v28 -> v29: `phases[issue]` records gain an optional `assignedAt` (the ISO time the architect
 * assignment that created the record was delivered); existing records validate as-is with the
 * field absent, so a v28-or-older record is exactly one that may lack it. A pure version bump:
 * nothing is backfilled — a fabricated timestamp would be worse than `assignedAt unknown` in the
 * completion route's refusal log. */
function migrateV28State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 28) return state;
  return { ...state, version: 29 };
}

/** v29 -> v30: `TreeState` gains the optional `resumeSessionFile` (LEGION-83) -- a pure version
 * bump: every existing tree validates with it absent, and absent is the correct starting value
 * (no root has yet had its session file kept across a cleared pane). */
function migrateV29State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 29) return state;
  return { ...state, version: 30 };
}

/** v30 -> v31: `PendingAssignment` gains `queuedAt` and fulfilled `spawn_worker` requests become
 * durable (LEGION-102). Older pending tasks recorded no queue time, so each missing timestamp is
 * stamped with this load's migration instant; the new request ledger starts empty. */
function migrateV30State(state: unknown, migratedAt: number): unknown {
  if (!recordValue(state) || state.version !== 30) return state;
  const { roles, ...rest } = state;
  const stamp = new Date(migratedAt).toISOString();
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) =>
          recordValue(claim) &&
          "issue" in claim &&
          recordValue(claim.pendingAssignment) &&
          typeof claim.pendingAssignment.queuedAt !== "string"
            ? [
                key,
                { ...claim, pendingAssignment: { ...claim.pendingAssignment, queuedAt: stamp } },
              ]
            : [key, claim]
        )
      )
    : roles;
  return { ...rest, version: 31, roles: migratedRoles, spawnRequests: {} };
}

/** v31 -> v32: every persisted pending assignment gains its stable prompt delivery identifier.
 * Existing identifiers remain unchanged; only an absent field is initialized. */
function migrateV31State(state: unknown): unknown {
  if (!recordValue(state) || state.version !== 31) return state;
  const { roles, ...rest } = state;
  const migratedRoles = recordValue(roles)
    ? Object.fromEntries(
        Object.entries(roles).map(([key, claim]) =>
          recordValue(claim) &&
          "issue" in claim &&
          recordValue(claim.pendingAssignment) &&
          typeof claim.pendingAssignment.deliveryId !== "string"
            ? [
                key,
                {
                  ...claim,
                  pendingAssignment: { ...claim.pendingAssignment, deliveryId: randomUUID() },
                },
              ]
            : [key, claim]
        )
      )
    : roles;
  return { ...rest, version: 32, roles: migratedRoles };
}

/** v32 -> v33: strips `socketPath` from `controllerLocator`. The controller ran behind
 * `legion worker-shim` until now, so the live deployment's state carries a socket on its
 * locator; the controller is an interactive OMP pane with no socket, the field means nothing for
 * it, and the redacted `/state` and AGENTS.md both say the controller locator has none. The
 * `runtime` tag and every other field stay. `ControllerLocatorSchema` rejects a socket at load,
 * so this migration is what keeps a v32 file loadable. Stripping a socket is also the one signal
 * that the pane it belonged to is the old headless controller, which nothing in the daemon kills
 * (see `LegionStateInit.onHeadlessControllerStripped`); the hook only reports, it never mutates
 * the locator — dropping it would fight the old pane's heartbeat. */
function migrateV32State(
  state: unknown,
  onHeadlessControllerStripped: LegionStateInit["onHeadlessControllerStripped"]
): unknown {
  if (!recordValue(state) || state.version !== 32) return state;
  if (!recordValue(state.controllerLocator)) return { ...state, version: 33 };
  const { socketPath, ...controllerLocator } = state.controllerLocator;
  if (socketPath !== undefined) {
    onHeadlessControllerStripped?.({
      tmuxSession:
        typeof controllerLocator.tmuxSession === "string"
          ? controllerLocator.tmuxSession
          : undefined,
      tmuxPaneId:
        typeof controllerLocator.tmuxPaneId === "string" ? controllerLocator.tmuxPaneId : undefined,
    });
  }
  return { ...state, version: 33, controllerLocator };
}

export async function loadState(file: string, init: LegionStateInit): Promise<LegionState> {
  let raw: string;
  try {
    raw = await readFile(file, "utf8");
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) {
      return newLegionState(init.project, init.cap);
    }
    throw error;
  }

  const source = JSON.parse(raw);
  const sourceVersion = recordValue(source) ? source.version : undefined;
  // A single timestamp for this whole load, used only by the migrations that backfill a time
  // (migrateV19State's and migrateV20State's readyConfirmedAt, migrateV30State's queuedAt) --
  // every claim/tree either touches in this one load gets the same migration instant.
  const migratedAt = Date.now();
  // Ordered oldest-to-newest: each migration is a no-op unless `state.version` matches the one
  // it upgrades from, so this reduce applies exactly the same chain the prior nested-call form
  // did, just as an auditable list instead of a call pyramid.
  const migrations: Array<(state: unknown) => unknown> = [
    migrateV5State,
    migrateV6State,
    migrateV7State,
    migrateV8State,
    migrateV12State,
    migrateV13State,
    migrateV14State,
    migrateV15State,
    migrateV16State,
    migrateV17State,
    migrateV18State,
    (state) => migrateV19State(state, migratedAt),
    (state) => migrateV20State(state, migratedAt),
    migrateV21State,
    migrateV22State,
    migrateV23State,
    migrateV24State,
    migrateV25State,
    migrateV26State,
  ];
  const preGateState = migrations.reduce((current, migrate) => migrate(current), source as unknown);
  const gatedState = await migrateV27State(preGateState, init.resolveSpecArtifact);
  const postGateMigrations: Array<(state: unknown) => unknown> = [
    migrateV28State,
    migrateV29State,
    (state) => migrateV30State(state, migratedAt),
    migrateV31State,
    (state) => migrateV32State(state, init.onHeadlessControllerStripped),
  ];
  const state = postGateMigrations.reduce((current, migrate) => migrate(current), gatedState);
  let version: unknown;
  if (typeof state === "object" && state !== null && "version" in state) {
    version = state.version;
  }
  if (version !== 33) {
    throw new Error(`Unsupported Legion state version: ${String(version)}`);
  }

  const parsed = LegionStateSchema.safeParse(state);
  if (!parsed.success) {
    throw new Error(`Invalid Legion state: ${describeIssues(parsed.error.issues).join(", ")}`);
  }

  // Zod v3 infers validated records as partial despite every mapped value being required.
  const validatedState = parsed.data as LegionState;
  pruneStalePrTombstones(validatedState, Date.now());
  if (
    typeof sourceVersion === "number" &&
    Number.isSafeInteger(sourceVersion) &&
    sourceVersion < validatedState.version
  ) {
    try {
      await writeFile(`${file}.v${sourceVersion}.bak`, raw, { encoding: "utf8", flag: "wx" });
    } catch (error) {
      if (!hasErrnoCode(error, "EEXIST")) throw error;
    }
    // A migration runs exactly once: the migrated state is written now, not at the first ordinary
    // save, so a restart before any event does not re-run the chain — migrateV27State's Dispatch
    // read per kept gate (and its refusal to start when one fails) would otherwise recur on every
    // boot until something else happened to save.
    await saveState(file, validatedState);
  }
  return validatedState;
}

export async function saveState(file: string, state: LegionState): Promise<void> {
  await mkdir(path.dirname(file), { recursive: true });

  const temporaryFile = `${file}.tmp-${process.pid}-${Date.now()}`;
  try {
    await writeFile(temporaryFile, JSON.stringify(state, null, 2), "utf8");
    await rename(temporaryFile, file);
  } catch (error) {
    try {
      await unlink(temporaryFile);
    } catch (cleanupError) {
      if (!hasErrnoCode(cleanupError, "ENOENT")) {
        throw new AggregateError([error, cleanupError], `Failed to save Legion state to ${file}`);
      }
    }
    throw error;
  }
}
