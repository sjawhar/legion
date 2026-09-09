import { mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  assertLegionProjectToken,
  formatIssueKey,
  type IssueKey,
  isLegionProjectToken,
  type LegionRole,
} from "@legion/contracts";
import { z } from "zod";
import type { CheckRunRef } from "../state/types";

/** Which read last set a fence's timestamp: a real GitHub webhook, or the daemon's own resync (board GraphQL/CI-status/merge-gate) read. At an identical clock a resync read is GitHub's authoritative source of truth and wins a tie against a disagreeing webhook observation. */
export type UpdateSource = "webhook" | "resync";

export interface IssueNode {
  key: IssueKey;
  title: string;
  state: "open" | "closed";
  parent?: IssueKey;
  children: IssueKey[];
  released: boolean;
  labels: string[];
  backlogMarker?: string;
  finalCommentRef?: string;
  /** The GitHub payload's `updated_at` (or, for a sub_issue event, `parent_issue.updated_at`) from the last event applied to this issue — a freshness fence, mirroring `PrState.headUpdatedAt`, against an out-of-order redelivery. */
  updatedAt?: number;
  /** The source of `updatedAt`'s last write; see `UpdateSource`. */
  updatedAtSource?: UpdateSource;
}

export interface HeldEvent {
  role: string;
  payloadJson: string;
  heldAt: string;
  eventId: string;
}
export interface RecoveryEvent {
  issue: IssueKey;
  role: LegionRole;
  original: { topic: string; payload: string; eventId: string };
}

export interface TmuxWindowLocator {
  tmuxSession: string;
  tmuxWindowId: string;
  tmuxPaneId?: string;
  socketPath?: string;
  ompSessionFile?: string;
}

export interface TreeState {
  root: IssueKey;
  generation: number;
  locator?: TmuxWindowLocator;
  status: "queued" | "active" | "lingering" | "dead" | "launch-failed" | "closed";
  lingerUntil?: string;
  launchFailures: number;
  heldEvents: HeldEvent[];
  recoveryEvents?: RecoveryEvent[];
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
}

export interface WorkerLocator {
  tmuxSession: string;
  tmuxWindowId: string;
  tmuxPaneId: string;
  socketPath: string;
  ompSessionFile?: string;
}

export interface WorkerRoleClaim {
  issue: IssueKey;
  role: string;
  sessionId?: string;
  agentId?: string;
  locator?: WorkerLocator;
  generation?: number;
  pendingAssignment?: string;
  launchFailures?: number;
  bootTokenHash?: string;
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
}

export interface LegionState {
  version: 14;
  project: string;
  issues: Record<IssueKey, IssueNode>;
  trees: Record<IssueKey, TreeState>;
  controllerLocator?: Pick<
    TmuxWindowLocator,
    "tmuxSession" | "tmuxWindowId" | "tmuxPaneId" | "socketPath"
  >;
  roles: Record<string, RoleClaim>;
  spawnCapabilities: Record<string, SpawnCapability>;
  prs: Record<string, PrState>;
  prByBranch: Record<string, string>;
  /** A closed-unmerged PR's `headUpdatedAt` at close, keyed by `repo#number`: keeps an older `opened`/`synchronize` redelivery from recreating a PR this state has already deleted (see `pullRequest` in reducers.ts). Pruned past 30 days by `pruneStalePrTombstones`. */
  prTombstones: Record<string, number>;
  admission: { cap: number; active: IssueKey[]; queue: IssueKey[] };
  phases: Record<IssueKey, { phase: string; sessionId: string } | undefined>;
  controllerHeldEvents: HeldEvent[];
  controllerCapabilityHash?: string;
}

export interface LegionStateInit {
  project: string;
  cap: number;
}

const ISSUE_KEY_PATTERN = /^[^/#]+\/[^/#]+#\d+$/;
const REPOSITORY_PATTERN = /^[^/]+\/[^/]+$/;
const ENVOY_ROLE_TOKEN_PATTERN = /^[a-z0-9][a-z0-9_-]*$/;

const IssueKeySchema = z.custom<IssueKey>(
  (value) => typeof value === "string" && ISSUE_KEY_PATTERN.test(value),
  { message: "Expected owner/repo#number issue key" }
);
const RepositorySchema = z.custom<`${string}/${string}`>(
  (value) => typeof value === "string" && REPOSITORY_PATTERN.test(value),
  { message: "Expected owner/repo repository" }
);
const GateLabelSchema = z.enum([
  "needs-approval",
  "human-approved",
  "legion-child",
  "legion-backlog",
]);
const IssueNodeSchema = z
  .object({
    key: IssueKeySchema,
    title: z.string(),
    state: z.enum(["open", "closed"]),
    parent: IssueKeySchema.optional(),
    children: z.array(IssueKeySchema),
    released: z.boolean(),
    labels: z.array(GateLabelSchema),
    backlogMarker: z.string().optional(),
    finalCommentRef: z.string().optional(),
    updatedAt: z.number().optional(),
    updatedAtSource: z.enum(["webhook", "resync"]).optional(),
  })
  .strict();
const HeldEventSchema = z
  .object({
    role: z.string(),
    payloadJson: z.string(),
    heldAt: z.string(),
    eventId: z.string(),
  })
  .strict();
const RecoveryEventSchema = z
  .object({
    issue: IssueKeySchema,
    role: z.enum(["architect", "planner", "implementer", "tester", "reviewer", "merger"]),
    original: z
      .object({
        topic: z.string(),
        payload: z.string(),
        eventId: z.string(),
      })
      .strict(),
  })
  .strict();

const TreeStateSchema = z
  .object({
    root: IssueKeySchema,
    generation: z.number().int().nonnegative(),
    locator: z
      .object({
        tmuxSession: z.string(),
        tmuxWindowId: z.string(),
        tmuxPaneId: z.string().optional(),
        socketPath: z.string().optional(),
        ompSessionFile: z.string().optional(),
      })
      .strict()
      .optional(),
    status: z.enum(["queued", "active", "lingering", "dead", "launch-failed", "closed"]),
    lingerUntil: z.string().optional(),
    launchFailures: z.number().int().nonnegative(),
    heldEvents: z.array(HeldEventSchema),
    recoveryEvents: z.array(RecoveryEventSchema).optional(),
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
  })
  .strict();
const WorkerLocatorSchema = z
  .object({
    tmuxSession: z.string(),
    tmuxWindowId: z.string(),
    tmuxPaneId: z.string(),
    socketPath: z.string(),
    ompSessionFile: z.string().optional(),
  })
  .strict();
const WorkerRoleClaimSchema = z
  .object({
    issue: IssueKeySchema,
    role: z.string(),
    sessionId: z.string().optional(),
    agentId: z.string().optional(),
    locator: WorkerLocatorSchema.optional(),
    generation: z.number().int().nonnegative().optional(),
    pendingAssignment: z.string().optional(),
    launchFailures: z.number().int().nonnegative().optional(),
    bootTokenHash: z.string().optional(),
  })
  .strict();
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
  })
  .strict();
const PhaseSchema = z
  .object({
    phase: z.string(),
    sessionId: z.string(),
  })
  .strict();
const LegionStateSchema = z
  .object({
    version: z.literal(14),
    project: z.string().refine(isLegionProjectToken, {
      message: "Expected valid Legion project token",
    }),
    issues: z.record(IssueKeySchema, IssueNodeSchema),
    trees: z.record(IssueKeySchema, TreeStateSchema),
    controllerLocator: z
      .object({
        tmuxSession: z.string(),
        tmuxWindowId: z.string(),
        tmuxPaneId: z.string().optional(),
        socketPath: z.string().optional(),
      })
      .strict()
      .optional(),
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
    phases: z.record(IssueKeySchema, PhaseSchema),
    controllerHeldEvents: z.array(HeldEventSchema).default([]),
    controllerCapabilityHash: z
      .string()
      .regex(/^[a-f0-9]{64}$/)
      .optional(),
  })
  .strict();

function hasErrnoCode(error: unknown, code: string): boolean {
  return typeof error === "object" && error !== null && "code" in error && error.code === code;
}

export function newLegionState(project: string, cap: number): LegionState {
  assertLegionProjectToken(project);

  return {
    version: 14,
    project,
    issues: {},
    trees: {},
    roles: {},
    spawnCapabilities: {},
    prs: {},
    prByBranch: {},
    prTombstones: {},
    admission: { cap, active: [], queue: [] },
    phases: {},
    controllerHeldEvents: [],
  };
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
  return formatIssueKey(owner, name, thread);
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
  const state = migrateV13State(
    migrateV12State(migrateV8State(migrateV7State(migrateV6State(migrateV5State(source)))))
  );
  let version: unknown;
  if (typeof state === "object" && state !== null && "version" in state) {
    version = state.version;
  }
  if (version !== 14) {
    throw new Error(`Unsupported Legion state version: ${String(version)}`);
  }

  const parsed = LegionStateSchema.safeParse(state);
  if (!parsed.success) {
    const issues = parsed.error.issues.map((issue) => issue.message).join(", ");
    throw new Error(`Invalid Legion state: ${issues}`);
  }

  // Zod v3 infers validated records as partial despite every mapped value being required.
  const validatedState = parsed.data as LegionState;
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
  }
  pruneStalePrTombstones(validatedState, Date.now());
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
