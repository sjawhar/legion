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

const routingHintsSchema = z
  .object({
    skipArchitect: z.boolean().optional(),
    complexity: z.enum(["trivial", "small", "medium", "large"]).optional(),
    estimatedImplementers: z.number().optional(),
  })
  .passthrough()
  .optional();

const baseHandoffSchema = z.object({
  schemaVersion: z.literal(HANDOFF_SCHEMA_VERSION),
  phase: handoffPhase,
  completed: isoTimestamp,
  learningsInjected: z.array(z.string()).optional(),
  learningsHelpful: z.array(z.string()).optional(),
});

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
  trickyParts: z.array(z.string()).optional(),
  deviations: z.array(z.string()).optional(),
  openQuestions: z.array(z.string()).optional(),
  subPlanningNeeded: z.boolean().optional(),
  discoveredComplexity: z.array(z.string()).optional(),
  suggestedSubWorkers: z.number().optional(),
});

const testSchema = baseHandoffSchema.extend({
  phase: z.literal("test"),
  passed: z.number().optional(),
  failed: z.number().optional(),
  failures: z
    .array(z.object({ criterion: z.string(), evidence: z.string() }).passthrough())
    .optional(),
  documentationFeedback: z.string().optional(),
  observations: z.array(z.string()).optional(),
});

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

const phaseHandoffSchema = z.discriminatedUnion("phase", [
  architectSchema.passthrough(),
  planSchema.passthrough(),
  implementSchema.passthrough(),
  testSchema.passthrough(),
  reviewSchema.passthrough(),
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

export function validateHandoffMessage(value: unknown): HandoffMessage | null {
  const result = handoffMessageSchema.safeParse(value);
  return result.success ? (result.data as HandoffMessage) : null;
}
