export type HandoffPhase = "architect" | "plan" | "implement" | "test" | "review" | "retro";

export interface RoutingHints {
  skipTest?: boolean;
  skipRetro?: boolean;
  skipArchitect?: boolean;
  complexity?: "trivial" | "small" | "medium" | "large";
  estimatedImplementers?: number;
}

export interface BaseHandoff {
  schemaVersion: 1;
  phase: HandoffPhase;
  completed: string;
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
  learningsUsed?: string[];
  workflowRecommendation?: string;
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

export interface RetroHandoff extends BaseHandoff {
  phase: "retro";
  skipped?: boolean;
  reason?: string;
  docsCreated?: string[];
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
  | ReviewHandoff
  | RetroHandoff;
