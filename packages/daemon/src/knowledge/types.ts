import { z } from "zod";

import { HANDOFF_PHASES } from "../handoff/schema";
import type { HandoffPhase } from "../handoff/types";

export const KNOWLEDGE_SCHEMA_VERSION = 1 as const;

export const KNOWLEDGE_NON_ARCHITECT_PHASES = [
  "plan",
  "implement",
  "test",
  "review",
  "retro",
] as const satisfies readonly HandoffPhase[];

const isoTimestampSchema = z.string().regex(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/);

export const LearningFeedbackPhaseSchema = z.object({
  helpful: z.array(z.string()).default([]),
  injected: z.array(z.string()).default([]),
});

const learningFeedbackPhaseShape = {
  architect: LearningFeedbackPhaseSchema.optional(),
  plan: LearningFeedbackPhaseSchema.optional(),
  implement: LearningFeedbackPhaseSchema.optional(),
  review: LearningFeedbackPhaseSchema.optional(),
  retro: LearningFeedbackPhaseSchema.optional(),
  test: LearningFeedbackPhaseSchema.optional(),
} satisfies Record<HandoffPhase, z.ZodOptional<typeof LearningFeedbackPhaseSchema>>;

export const LearningFeedbackRecordSchema = z.object({
  issueId: z.string(),
  phases: z.object(learningFeedbackPhaseShape),
  schemaVersion: z.literal(KNOWLEDGE_SCHEMA_VERSION),
  timestamp: isoTimestampSchema,
});

export type LearningFeedbackPhase = z.infer<typeof LearningFeedbackPhaseSchema>;
export type LearningFeedbackRecord = z.infer<typeof LearningFeedbackRecordSchema>;

export type ConsolidationDisposition = "accepted" | "archived" | "needs_review" | "rejected";

export interface HelpfulIssueContext {
  firstSeenAt: string;
  helpfulPaths: string[];
  issueId: string;
  phases: HandoffPhase[];
}

export interface CollectedIssueFeedback {
  issueId: string;
  records: LearningFeedbackRecord[];
  touchedPaths: string[];
}

export interface LearningAggregate {
  firstSeenAt: string;
  helpfulCount: number;
  issues: HelpfulIssueContext[];
  lastSeenAt: string;
  path: string;
  touchedCount: number;
}

export interface ClassifiedLearningAggregate extends LearningAggregate {
  disposition: ConsolidationDisposition;
  notes?: string;
}

export interface IndexMutation {
  action: "delete" | "upsert";
  path: string;
  reason: string;
}

export interface StatusMutation {
  action: "clear" | "set";
  path: string;
  reason: string;
  status?: string;
}

export interface ConsolidationReport {
  aggregates: ClassifiedLearningAggregate[];
  generatedAt: string;
  indexMutations: IndexMutation[];
  issueCount: number;
  recordCount: number;
  statusMutations: StatusMutation[];
}

export const KNOWLEDGE_PHASES = HANDOFF_PHASES;
