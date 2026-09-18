import { z } from "zod";

export const HANDOFF_SCHEMA_VERSION = 1 as const;
export const LEGION_DIR_NAME = ".legion";
export const MESSAGES_DIR_NAME = "messages";

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

export interface HandoffMessage {
  from: HandoffPhase;
  to: HandoffPhase;
  body: string;
  timestamp: string;
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

const planSchema = baseHandoffSchema.extend({
  phase: z.literal("plan"),
  taskCount: z.number().optional(),
  independentTasks: z.number().optional(),
  routingHints: routingHintsSchema,
  concerns: z.array(z.string()).optional(),
  workflowRecommendation: z.string().optional(),
  requiredSkills: requiredSkillsSchema,
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

/** Write-time contract for a plan handoff: every downstream role's skill list is present and
 * non-empty, so a plan that names no skills is refused before it reaches the branch. A legitimate
 * "nothing applies" is the single entry `none: <what was looked through and why nothing fits>`
 * (Sami, AGENTC-370, 2026-09-18: a nascent project may have no agent skills yet). Read-time
 * validation (`planSchema`) stays tolerant so plans committed before this rule still load. */
const planWriteSchema = planSchema.extend({
  requiredSkills: z
    .object({
      implement: nonEmptySkillList,
      test: nonEmptySkillList,
      review: nonEmptySkillList,
    })
    .passthrough(),
});

const phaseHandoffSchema = z.discriminatedUnion("phase", [
  architectSchema,
  planSchema,
  implementSchema,
  testSchema,
  reviewSchema,
]);

const handoffMessageSchema = z.object({
  from: handoffPhase,
  to: handoffPhase,
  body: z.string(),
  timestamp: isoTimestamp,
});

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
 * phase adds on top — today only the plan's `requiredSkills` contract. Empty for a writable handoff. */
export function describePhaseHandoffWriteProblems(value: unknown): string[] {
  const problems = describePhaseHandoffProblems(value);
  if (problems.length > 0) return problems;
  if ((value as { phase?: unknown }).phase !== "plan") return [];
  const result = planWriteSchema.safeParse(value);
  if (result.success) return [];
  const fields = [...new Set(result.error.issues.map((issue) => issue.path.map(String).join(".")))];
  return fields.map(
    (field) =>
      `${field}: missing or empty — name the skills this role must load, or state \`none: <what you looked through and why nothing fits>\``
  );
}

export function validateHandoffMessage(value: unknown): HandoffMessage | null {
  const result = handoffMessageSchema.safeParse(value);
  return result.success ? (result.data as HandoffMessage) : null;
}
