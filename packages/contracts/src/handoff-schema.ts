import { z } from "zod";

export const HANDOFF_SCHEMA_VERSION = 1 as const;
export const LEGION_DIR_NAME = ".legion";

export const HANDOFF_PHASES = ["architect", "plan", "implement", "test", "review"] as const;

export type HandoffPhase = (typeof HANDOFF_PHASES)[number];

export const PHASE_FILE_NAMES: Record<HandoffPhase, string> = {
  architect: "architect.json",
  plan: "plan.json",
  implement: "implement.json",
  test: "test.json",
  review: "review.json",
};

export interface RoutingHints {
  skipArchitect?: boolean;
  complexity?: "trivial" | "small" | "medium" | "large";
  estimatedImplementers?: number;
}

export interface RequiredSkills {
  implement?: string[];
  test?: string[];
  review?: string[];
}

/** The most rounds of plan review a planner runs before it proceeds with the plan (LEGION-421). */
export const PLAN_REVIEW_MAX_ROUNDS = 3;

export const PLAN_REVIEW_VERDICTS = ["approved", "rejected", "failed"] as const;

/** One finding of the gap analysis run before the plan was drafted, and the plan's answer to it. */
export interface GapFinding {
  /** The hidden requirement, ambiguity, or missing machine-checkable acceptance criterion. */
  finding: string;
  /** How the plan answers it: the task, criterion, or decision that settles it. */
  answer: string;
}

/** The gap analysis: its findings, or the error of the call that produced none. */
export interface GapAnalysis {
  /** Every finding with the plan's answer; empty when the analyst found none. */
  findings?: GapFinding[];
  /** The failed call's error. */
  error?: string;
}

/** One blocking issue the plan review's last round named. */
export interface PlanReviewIssue {
  issue: string;
  evidence: string;
}

/** The plan review: `approved` by its last round, still `rejected` after the last round the
 * planner may run, or `failed` when a review's call failed. */
export interface PlanReview {
  verdict: (typeof PLAN_REVIEW_VERDICTS)[number];
  /** The reviews run, a failed one included. */
  rounds: number;
  /** The blocking issues still standing: required when `rejected`, none when `approved`. */
  remainingIssues?: PlanReviewIssue[];
  /** The failed call's error: required when `failed`, and only then. */
  error?: string;
}

/** One production-like proof: the changed behaviour exercised on the surface a user reaches
 * it through, never a unit suite. The implementer records its own before its phase completes;
 * the tester verifies that one and records its own (LEGION-53). */
export interface Proof {
  /** The acceptance line this proves. */
  criterion: string;
  /** The real surface: a scratch daemon, a smoke rig, a sandbox repository, a browser, a stack. */
  surface: string;
  /** The exact command, run id, or URL. */
  command: string;
  /** What happened. */
  observed: string;
  /** The commit the proof was taken at. */
  headSha: string;
  /** The deliberately broken input and the refusal or failure it produced. */
  negativeControl: string;
}

export interface BaseHandoff {
  schemaVersion: 1;
  phase: HandoffPhase;
  completed: string;
  /** Canonical docs/solutions/ paths injected into this phase */
  learningsInjected?: string[];
  /** Subset of learningsInjected the worker found materially helpful */
  learningsHelpful?: string[];
}

export interface ArchitectHandoff extends BaseHandoff {
  phase: "architect";
  scope?: "trivial" | "small" | "medium" | "large";
  components?: string[];
  subIssues?: string[];
  routingHints?: RoutingHints;
  concerns?: string[];
}

export interface PlanHandoff extends BaseHandoff {
  phase: "plan";
  taskCount?: number;
  independentTasks?: number;
  routingHints?: RoutingHints;
  concerns?: string[];
  workflowRecommendation?: string;
  requiredSkills?: RequiredSkills;
  /** Required at write time; optional at read, so plans committed before the checks still load. */
  gapAnalysis?: GapAnalysis;
  /** Required at write time; optional at read, so plans committed before the checks still load. */
  planReview?: PlanReview;
}

export interface ImplementHandoff extends BaseHandoff {
  phase: "implement";
  filesChanged?: string[];
  /** Required, non-empty: this phase is not complete without its own production-like proof. */
  proof: Proof[];
  trickyParts?: string[];
  deviations?: string[];
  openQuestions?: string[];
  subPlanningNeeded?: boolean;
  discoveredComplexity?: string[];
  suggestedSubWorkers?: number;
}

export interface TestHandoff extends BaseHandoff {
  phase: "test";
  passed?: number;
  failed?: number;
  failures?: Array<{ criterion: string; evidence: string }>;
  /** This tester's verdict on the implementer's own proof, and how it checked. */
  implementerProof: { verdict: "verified" | "rejected"; how: string };
  /** The tester's own proof. Required when this handoff reports no failure. */
  proof?: Proof[];
  documentationFeedback?: string;
  observations?: string[];
}

export interface ReviewHandoff extends BaseHandoff {
  phase: "review";
  critical?: number;
  important?: number;
  minor?: number;
  verdict?: "approved" | "changes_requested";
  keyFindings?: Array<{ severity: string; file: string; description: string }>;
}

export type PhaseHandoff =
  | ArchitectHandoff
  | PlanHandoff
  | ImplementHandoff
  | TestHandoff
  | ReviewHandoff;

const isoTimestamp = z.string().regex(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);
const handoffPhase = z.enum(HANDOFF_PHASES);
const nonEmpty = z.string().trim().min(1);

const proofSchema = z
  .object({
    criterion: nonEmpty,
    surface: nonEmpty,
    command: nonEmpty,
    observed: nonEmpty,
    headSha: nonEmpty,
    negativeControl: nonEmpty,
  })
  .passthrough();

const routingHintsSchema = z
  .object({
    skipArchitect: z.boolean().optional(),
    complexity: z.enum(["trivial", "small", "medium", "large"]).optional(),
    estimatedImplementers: z.number().optional(),
  })
  .passthrough()
  .optional();

// Undeclared fields pass through untouched at every phase: the next worker may need them (the
// worker skill promises this). The one catchall lives here; `.extend()` carries it into each phase.
const baseHandoffSchema = z
  .object({
    schemaVersion: z.literal(HANDOFF_SCHEMA_VERSION),
    phase: handoffPhase,
    completed: isoTimestamp,
    learningsInjected: z.array(z.string()).optional(),
    learningsHelpful: z.array(z.string()).optional(),
  })
  .passthrough();

const architectSchema = baseHandoffSchema.extend({
  phase: z.literal("architect"),
  scope: z.enum(["trivial", "small", "medium", "large"]).optional(),
  components: z.array(z.string()).optional(),
  subIssues: z.array(z.string()).optional(),
  routingHints: routingHintsSchema,
  concerns: z.array(z.string()).optional(),
});

const requiredSkillsSchema = z
  .object({
    implement: z.array(z.string()).optional(),
    test: z.array(z.string()).optional(),
    review: z.array(z.string()).optional(),
  })
  .passthrough()
  .optional();

const gapAnalysisSchema = z
  .object({
    findings: z
      .array(
        z.object({ finding: z.string().optional(), answer: z.string().optional() }).passthrough()
      )
      .optional(),
    error: z.string().optional(),
  })
  .passthrough()
  .optional();

const planReviewSchema = z
  .object({
    verdict: z.enum(PLAN_REVIEW_VERDICTS).optional(),
    rounds: z.number().optional(),
    remainingIssues: z
      .array(
        z.object({ issue: z.string().optional(), evidence: z.string().optional() }).passthrough()
      )
      .optional(),
    error: z.string().optional(),
  })
  .passthrough()
  .optional();

const planSchema = baseHandoffSchema.extend({
  phase: z.literal("plan"),
  taskCount: z.number().optional(),
  independentTasks: z.number().optional(),
  routingHints: routingHintsSchema,
  concerns: z.array(z.string()).optional(),
  workflowRecommendation: z.string().optional(),
  requiredSkills: requiredSkillsSchema,
  gapAnalysis: gapAnalysisSchema,
  planReview: planReviewSchema,
});

const implementSchema = baseHandoffSchema.extend({
  phase: z.literal("implement"),
  filesChanged: z.array(z.string()).optional(),
  proof: z.array(proofSchema).min(1),
  trickyParts: z.array(z.string()).optional(),
  deviations: z.array(z.string()).optional(),
  openQuestions: z.array(z.string()).optional(),
  subPlanningNeeded: z.boolean().optional(),
  discoveredComplexity: z.array(z.string()).optional(),
  suggestedSubWorkers: z.number().optional(),
});

// A passing test handoff needs the tester's own proof; one that reports a failure does not — and
// a reported failure (`failed > 0`, or a rejected implementer proof) is a recorded one.
const testSchema = baseHandoffSchema
  .extend({
    phase: z.literal("test"),
    passed: z.number().optional(),
    failed: z.number().optional(),
    failures: z
      .array(z.object({ criterion: z.string(), evidence: z.string() }).passthrough())
      .optional(),
    implementerProof: z
      .object({ verdict: z.enum(["verified", "rejected"]), how: nonEmpty })
      .passthrough(),
    proof: z.array(proofSchema).min(1).optional(),
    documentationFeedback: z.string().optional(),
    observations: z.array(z.string()).optional(),
  })
  .refine(
    (handoff) =>
      (handoff.failures?.length ?? 0) > 0 ||
      (handoff.failed ?? 0) > 0 ||
      (handoff.proof?.length ?? 0) > 0,
    {
      path: ["proof"],
      message: "a passing test handoff needs the tester's own production-like proof",
    }
  )
  .refine((handoff) => (handoff.failed ?? 0) === 0 || (handoff.failures?.length ?? 0) > 0, {
    path: ["failures"],
    message: "a test handoff that reports failed > 0 records at least one failure",
  })
  .refine(
    (handoff) =>
      handoff.implementerProof.verdict !== "rejected" || (handoff.failures?.length ?? 0) > 0,
    {
      path: ["failures"],
      message: "a rejected implementer proof is a recorded failure",
    }
  );

const reviewSchema = baseHandoffSchema.extend({
  phase: z.literal("review"),
  critical: z.number().optional(),
  important: z.number().optional(),
  minor: z.number().optional(),
  verdict: z.enum(["approved", "changes_requested"]).optional(),
  keyFindings: z
    .array(
      z.object({ severity: z.string(), file: z.string(), description: z.string() }).passthrough()
    )
    .optional(),
});

const nonEmptySkillList = z.array(z.string().trim().min(1)).min(1);

/** A write-time plan check: the object is required, and its absence is named with what to record. */
const recorded = <Shape extends z.ZodRawShape>(shape: Shape, whatToRecord: string) =>
  z
    .object(shape, {
      error: (issue) =>
        issue.input === undefined ? `missing — record ${whatToRecord}` : undefined,
    })
    .passthrough();

/** The gap analysis before the plan was drafted: every finding with the plan's answer, or the
 * failed call's error, never both. */
const gapAnalysisWriteSchema = recorded(
  {
    findings: z.array(z.object({ finding: nonEmpty, answer: nonEmpty }).passthrough()).optional(),
    error: nonEmpty.optional(),
  },
  "the gap analyst's `findings`, each with how the plan answers it (`[]` when it found none), or its failed call's `error`"
).refine((analysis) => (analysis.findings === undefined) !== (analysis.error === undefined), {
  message: "record either `findings` or the failed call's `error`, not both",
});

/** The plan review after the draft: a rejection is recorded only after the last round the planner
 * runs, with the issues that round named; an approval leaves none; a failure names its error. */
const planReviewWriteSchema = recorded(
  {
    verdict: z.enum(PLAN_REVIEW_VERDICTS),
    rounds: z.number().int().min(1).max(PLAN_REVIEW_MAX_ROUNDS),
    remainingIssues: z
      .array(z.object({ issue: nonEmpty, evidence: nonEmpty }).passthrough())
      .optional(),
    error: nonEmpty.optional(),
  },
  "the plan review's `verdict` and `rounds`, with `remainingIssues` when it was rejected or `error` when a review's call failed"
).superRefine((review, ctx) => {
  const remaining = review.remainingIssues?.length ?? 0;
  if (review.verdict === "rejected" && remaining === 0) {
    ctx.addIssue({
      code: "custom",
      path: ["remainingIssues"],
      message: "a rejected review records the blocking issues its last round named",
    });
  }
  if (review.verdict === "rejected" && review.rounds < PLAN_REVIEW_MAX_ROUNDS) {
    ctx.addIssue({
      code: "custom",
      path: ["rounds"],
      message: `a review still rejecting after ${review.rounds} of ${PLAN_REVIEW_MAX_ROUNDS} rounds is revised and reviewed again, not recorded`,
    });
  }
  if (review.verdict === "approved" && remaining > 0) {
    ctx.addIssue({
      code: "custom",
      path: ["remainingIssues"],
      message: "an approved review leaves no blocking issue standing",
    });
  }
  if ((review.verdict === "failed") !== (review.error !== undefined)) {
    ctx.addIssue({
      code: "custom",
      path: ["error"],
      message: "a failed review records its call's error, and only a failed review does",
    });
  }
});

const REQUIRED_SKILLS_PROBLEM =
  "missing or empty — name the skills this role must load, or state `none: <what you looked through and why nothing fits>`";

/** Write-time contract for a plan handoff, where read-time validation (`planSchema`) stays
 * tolerant so plans committed before each rule still load:
 * - every downstream role's skill list is present and non-empty, so a plan that names no skills
 *   is refused before it reaches the branch. A legitimate "nothing applies" is the single entry
 *   `none: <what was looked through and why nothing fits>`, since a nascent project may have no
 *   agent skills yet.
 * - the gap analysis before the draft and the plan review after it are both recorded, a failed
 *   call as its error, since a missing check never blocks the plan. */
const planWriteSchema = planSchema.extend({
  requiredSkills: z
    .object({
      implement: nonEmptySkillList,
      test: nonEmptySkillList,
      review: nonEmptySkillList,
    })
    .passthrough(),
  gapAnalysis: gapAnalysisWriteSchema,
  planReview: planReviewWriteSchema,
});

const phaseHandoffSchema = z.discriminatedUnion("phase", [
  architectSchema,
  planSchema,
  implementSchema,
  testSchema,
  reviewSchema,
]);

export function isHandoffPhase(value: unknown): value is HandoffPhase {
  return handoffPhase.safeParse(value).success;
}

export function validatePhaseHandoff(value: unknown): PhaseHandoff | null {
  const result = phaseHandoffSchema.safeParse(value);
  return result.success ? (result.data as PhaseHandoff) : null;
}

/** Every reason `validatePhaseHandoff` rejects `value`, each naming its field path, so a write
 * refusal and a read warning can both say which field is missing or malformed. Empty for a
 * valid handoff. */
export function describePhaseHandoffProblems(value: unknown): string[] {
  const result = phaseHandoffSchema.safeParse(value);
  if (result.success) return [];
  return result.error.issues.map((issue) => {
    const field = issue.path.length > 0 ? issue.path.map(String).join(".") : "<root>";
    // zod 4.3.6 reports a missing or unknown discriminator as `invalid_union` at ["phase"] with
    // the bare message "Invalid input"; no phase schema's own `phase` literal is a union, so this
    // is the one case, and the reader is told what was expected instead.
    if (issue.code === "invalid_union" && field === "phase") {
      return `phase: expected one of ${HANDOFF_PHASES.join("|")}`;
    }
    return `${field}: ${issue.message}`;
  });
}

/** Every reason a handoff may not be WRITTEN: the read-time problems, plus the write-only rules a
 * phase adds on top — today only the plan's: its `requiredSkills`, `gapAnalysis`, and
 * `planReview`. Empty for a writable handoff. */
export function describePhaseHandoffWriteProblems(value: unknown): string[] {
  const problems = describePhaseHandoffProblems(value);
  if (problems.length > 0) return problems;
  if ((value as { phase?: unknown }).phase !== "plan") return [];
  const result = planWriteSchema.safeParse(value);
  if (result.success) return [];
  const described = result.error.issues.map((issue) => {
    const field = issue.path.map(String).join(".");
    return issue.path[0] === "requiredSkills"
      ? `${field}: ${REQUIRED_SKILLS_PROBLEM}`
      : `${field}: ${issue.message}`;
  });
  return [...new Set(described)];
}
