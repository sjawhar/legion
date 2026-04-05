import { z } from "zod";
import type { HandoffMessage, HandoffPhase, PhaseHandoff } from "./types";

export const HANDOFF_SCHEMA_VERSION = 1 as const;
export const LEGION_DIR_NAME = ".legion";
export const MESSAGES_DIR_NAME = "messages";

export const HANDOFF_PHASES: readonly HandoffPhase[] = [
  "architect",
  "plan",
  "implement",
  "test",
  "review",
  "retro",
];

export const PHASE_FILE_NAMES: Record<HandoffPhase, string> = {
  architect: "architect.json",
  plan: "plan.json",
  implement: "implement.json",
  test: "test.json",
  review: "review.json",
  retro: "retro.json",
};

const isoTimestamp = z.string().regex(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);

const handoffPhase = z.enum(["architect", "plan", "implement", "test", "review", "retro"]);

const routingHintsSchema = z
  .object({
    skipRetro: z.boolean().optional(),
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
  /** @deprecated Use learningsInjected on BaseHandoff instead */
  learningsUsed: z.array(z.string()).optional(),
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

const retroSchema = baseHandoffSchema.extend({
  phase: z.literal("retro"),
  skipped: z.boolean().optional(),
  reason: z.string().optional(),
  docsCreated: z.array(z.string()).optional(),
});

const phaseHandoffSchema = z.discriminatedUnion("phase", [
  architectSchema.passthrough(),
  planSchema.passthrough(),
  implementSchema.passthrough(),
  testSchema.passthrough(),
  reviewSchema.passthrough(),
  retroSchema.passthrough(),
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
  if (!result.success) {
    return null;
  }
  const data = result.data;
  // Backward compat: migrate plan.learningsUsed → learningsInjected
  if (data.phase === "plan" && "learningsUsed" in data) {
    const { learningsUsed, ...rest } = data as Record<string, unknown>;
    if (Array.isArray(learningsUsed) && !rest.learningsInjected) {
      (rest as Record<string, unknown>).learningsInjected = learningsUsed;
    }
    return rest as unknown as PhaseHandoff;
  }
  return data as PhaseHandoff;
}

export function validateHandoffMessage(value: unknown): HandoffMessage | null {
  const result = handoffMessageSchema.safeParse(value);
  if (!result.success) {
    return null;
  }
  return result.data as HandoffMessage;
}
