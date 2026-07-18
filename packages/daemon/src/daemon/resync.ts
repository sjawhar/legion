import { IssueStatus, WorkerMode, type WorkerModeLiteral } from "../state/types";
import {
  type IssueRef,
  noPrPhaseArtifacts,
  type PhaseArtifactBatch,
  type PhaseArtifacts,
  unresolvablePhaseArtifacts,
} from "./phase-artifacts";

export interface Recommendation {
  readonly issueId: string;
  readonly mode: WorkerModeLiteral;
  readonly reason: "artifact_no_live_owner";
}

export interface LiveWorker {
  readonly sessionId?: string;
  readonly mode?: WorkerModeLiteral;
  readonly status?: string;
}

export interface SessionStatusObservation {
  readonly data?: unknown;
}

export interface ResyncError {
  readonly issueId: string;
  readonly message: string;
}

export interface ResyncPassResult {
  readonly recommendations: readonly Recommendation[];
  readonly errors: readonly ResyncError[];
}

export interface RunResyncDeps {
  readonly listNonTerminalIssues: () => IssueRef[];
  readonly fetchPhaseArtifactsBatch: (refs: readonly IssueRef[]) => Promise<PhaseArtifactBatch>;
  readonly getLiveWorkers: () => Promise<Record<string, LiveWorker>>;
  // reserved for the idle-worker advisory (U9)
  readonly getSessionStatus: (sessionId: string) => Promise<SessionStatusObservation>;
  readonly emitToController: (items: readonly Recommendation[]) => void | Promise<void>;
}

export interface ResyncInput {
  readonly issueId: string;
  readonly status: string;
  readonly artifacts: PhaseArtifacts;
  readonly hasLiveWorker: boolean;
}

function statusFallback(status: string): WorkerModeLiteral | null {
  // TODO(label-teardown): Remove this pre-artifact phase bridge when labels are torn out.
  switch (status) {
    case IssueStatus.BACKLOG:
      return WorkerMode.ARCHITECT;
    case IssueStatus.TODO:
      return WorkerMode.PLAN;
    case IssueStatus.IN_PROGRESS:
      return WorkerMode.IMPLEMENT;
    case IssueStatus.TESTING:
      return WorkerMode.TEST;
    case IssueStatus.NEEDS_REVIEW:
      return WorkerMode.REVIEW;
    case IssueStatus.RETRO:
      return WorkerMode.IMPLEMENT;
    default:
      return null;
  }
}

function ownerFromArtifacts(artifacts: PhaseArtifacts): WorkerModeLiteral | null | undefined {
  if (artifacts.resolved.merged === "resolved" && artifacts.merged) return WorkerMode.IMPLEMENT;
  if (artifacts.resolved.hasNonDraftPr !== "resolved") return null;
  if (!artifacts.hasNonDraftPr) return undefined;
  if (artifacts.resolved.testerCheckOnHead !== "resolved") return null;
  if (artifacts.testerCheckOnHead === null) return WorkerMode.TEST;
  if (artifacts.testerCheckOnHead === "failure") return WorkerMode.IMPLEMENT;
  if (artifacts.resolved.nativeReviewOnHead !== "resolved") return null;
  if (artifacts.nativeReviewOnHead === null) return WorkerMode.REVIEW;
  if (artifacts.nativeReviewOnHead === "changes_requested") return WorkerMode.IMPLEMENT;
  if (artifacts.resolved.architectCheckOnHead !== "resolved") return null;
  if (artifacts.architectCheckOnHead === null) return WorkerMode.ARCHITECT;
  if (artifacts.architectCheckOnHead === "failure") return null;
  if (artifacts.resolved.autoMergeEnabledOrMerged !== "resolved") return null;
  return artifacts.autoMergeEnabledOrMerged ? null : WorkerMode.MERGE;
}

function expectedOwner(input: ResyncInput): WorkerModeLiteral | null {
  if (input.status === IssueStatus.DONE) return null;
  const artifactOwner = ownerFromArtifacts(input.artifacts);
  return artifactOwner === undefined ? statusFallback(input.status) : artifactOwner;
}

export function computeResyncRecommendations(inputs: readonly ResyncInput[]): Recommendation[] {
  const recommendations: Recommendation[] = [];
  for (const input of inputs) {
    const mode = expectedOwner(input);
    if (mode && !input.hasLiveWorker) {
      recommendations.push({ issueId: input.issueId, mode, reason: "artifact_no_live_owner" });
    }
  }
  return recommendations;
}

function failedArtifactBatch(refs: readonly IssueRef[], message: string): PhaseArtifactBatch {
  const artifacts: Record<string, PhaseArtifacts> = {};
  const errors: ResyncError[] = [];
  for (const ref of refs) {
    artifacts[ref.issueId] = ref.prRef ? unresolvablePhaseArtifacts() : noPrPhaseArtifacts();
    if (ref.prRef) errors.push({ issueId: ref.issueId, message });
  }
  return { artifacts, errors };
}

function completeArtifactBatch(
  refs: readonly IssueRef[],
  batch: PhaseArtifactBatch
): PhaseArtifactBatch {
  const artifacts: Record<string, PhaseArtifacts> = { ...batch.artifacts };
  const errors: ResyncError[] = [...batch.errors];
  const errorsByIssue = new Set(errors.map((error) => error.issueId));
  for (const ref of refs) {
    if (artifacts[ref.issueId]) continue;
    artifacts[ref.issueId] = ref.prRef ? unresolvablePhaseArtifacts() : noPrPhaseArtifacts();
    if (!errorsByIssue.has(ref.issueId)) {
      errors.push({ issueId: ref.issueId, message: "Phase artifact batch omitted issue data" });
    }
  }
  return { artifacts, errors };
}

async function resolveArtifactBatch(
  refs: readonly IssueRef[],
  deps: RunResyncDeps
): Promise<PhaseArtifactBatch> {
  try {
    return completeArtifactBatch(refs, await deps.fetchPhaseArtifactsBatch(refs));
  } catch (error) {
    return failedArtifactBatch(
      refs,
      error instanceof Error ? error.message : "Phase artifact batch failed"
    );
  }
}

export async function runResyncPass(deps: RunResyncDeps): Promise<ResyncPassResult> {
  const refs = deps.listNonTerminalIssues();
  const [artifactBatch, liveWorkers] = await Promise.all([
    resolveArtifactBatch(refs, deps),
    deps.getLiveWorkers(),
  ]);
  const recommendations = computeResyncRecommendations(
    refs.map((ref) => ({
      issueId: ref.issueId,
      status: ref.status,
      artifacts: artifactBatch.artifacts[ref.issueId] ?? unresolvablePhaseArtifacts(),
      hasLiveWorker: liveWorkers[ref.issueId] !== undefined,
    }))
  );
  await deps.emitToController(recommendations);
  return { recommendations, errors: artifactBatch.errors };
}
