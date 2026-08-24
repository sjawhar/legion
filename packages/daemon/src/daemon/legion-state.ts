import { mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  assertLegionProjectToken,
  type IssueKey,
  isLegionProjectToken,
  type LegionRole,
} from "@legion/contracts";
import { z } from "zod";

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

export interface TreeState {
  root: IssueKey;
  generation: number;
  locator?: { tmuxSession: string; tmuxWindow: string; ompSessionFile?: string; pid?: number };
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
  checks: Record<string, { status: string; conclusion: string | null }>;
  firstRedEmitted: boolean;
  settledRedEmitted: boolean;
  greenEmitted: boolean;
  lastEventAt: number;
  fixAttempts: number;
  reviewDecision?: "approved" | "changes_requested";
}

export interface DispatchThread {
  repo: `${string}/${string}`;
  thread: number;
  role: string;
  issue: IssueKey;
  tree: IssueKey;
}

export interface AttributionEntry {
  sha?: string;
  commentId?: number;
  sessionId: string;
  issue: IssueKey;
  phase: string;
}

export interface WorkerRoleClaim {
  issue: IssueKey;
  role: string;
  sessionId?: string;
  agentId?: string;
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
  version: 5;
  project: string;
  issues: Record<IssueKey, IssueNode>;
  trees: Record<IssueKey, TreeState>;
  roles: Record<string, RoleClaim>;
  spawnCapabilities: Record<string, SpawnCapability>;
  prs: Record<string, PrState>;
  prByBranch: Record<string, string>;
  admission: { cap: number; active: IssueKey[]; queue: IssueKey[] };
  dispatchThreads: DispatchThread[];
  attribution: AttributionEntry[];
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
        tmuxWindow: z.string(),
        ompSessionFile: z.string().optional(),
        pid: z.number().int().optional(),
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
const PrStateSchema = z
  .object({
    key: IssueKeySchema,
    repo: RepositorySchema,
    number: z.number().int().nonnegative(),
    headSha: z.string(),
    checks: z.record(
      z.string(),
      z
        .object({
          status: z.string(),
          conclusion: z.string().nullable(),
        })
        .strict()
    ),
    firstRedEmitted: z.boolean(),
    settledRedEmitted: z.boolean(),
    greenEmitted: z.boolean(),
    lastEventAt: z.number(),
    fixAttempts: z.number().int().nonnegative(),
    reviewDecision: z.enum(["approved", "changes_requested"]).optional(),
  })
  .strict();
const WorkerRoleClaimSchema = z
  .object({
    issue: IssueKeySchema,
    role: z.string(),
    sessionId: z.string().optional(),
    agentId: z.string().optional(),
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
const DispatchThreadSchema = z
  .object({
    repo: RepositorySchema,
    thread: z.number().int().nonnegative(),
    role: z.string(),
    issue: IssueKeySchema,
    tree: IssueKeySchema,
  })
  .strict();
const AttributionEntrySchema = z
  .object({
    sha: z.string().optional(),
    commentId: z.number().int().nonnegative().optional(),
    sessionId: z.string(),
    issue: IssueKeySchema,
    phase: z.string(),
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
    version: z.literal(5),
    project: z
      .string()
      .refine(isLegionProjectToken, { message: "Expected valid Legion project token" }),
    issues: z.record(IssueKeySchema, IssueNodeSchema),
    trees: z.record(IssueKeySchema, TreeStateSchema),
    roles: z.record(z.string().regex(ENVOY_ROLE_TOKEN_PATTERN), RoleClaimSchema),
    spawnCapabilities: z.record(z.string().regex(/^[a-f0-9]{64}$/), SpawnCapabilitySchema),
    prs: z.record(z.string(), PrStateSchema),
    prByBranch: z.record(z.string(), z.string()),
    admission: z
      .object({
        cap: z.number().int().nonnegative(),
        active: z.array(IssueKeySchema),
        queue: z.array(IssueKeySchema),
      })
      .strict(),
    dispatchThreads: z.array(DispatchThreadSchema),
    attribution: z.array(AttributionEntrySchema),
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
    version: 5,
    project,
    issues: {},
    trees: {},
    roles: {},
    spawnCapabilities: {},
    prs: {},
    prByBranch: {},
    admission: { cap, active: [], queue: [] },
    dispatchThreads: [],
    attribution: [],
    phases: {},
    controllerHeldEvents: [],
  };
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

  const state: unknown = JSON.parse(raw);
  let version: unknown;
  if (typeof state === "object" && state !== null && "version" in state) {
    version = state.version;
  }
  if (version !== 5) {
    throw new Error(`Unsupported Legion state version: ${String(version)}`);
  }

  const parsed = LegionStateSchema.safeParse(state);
  if (!parsed.success) {
    const issues = parsed.error.issues.map((issue) => issue.message).join(", ");
    throw new Error(`Invalid Legion state: ${issues}`);
  }

  // Zod v3 infers validated records as partial despite every mapped value being required.
  const validatedState = parsed.data as LegionState;
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
