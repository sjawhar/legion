import { IssueStatus, WorkerMode, type WorkerModeLiteral } from "../state/types";
import { computeIdleAdvisories, type IdleAdvisory } from "./idle-advisory";
import {
  type IssueRef,
  noPrPhaseArtifacts,
  type PhaseArtifactBatch,
  type PhaseArtifacts,
  unresolvablePhaseArtifacts,
} from "./phase-artifacts";

export type Recommendation =
  | {
      readonly issueId: string;
      readonly mode: WorkerModeLiteral;
      readonly reason: "artifact_no_live_owner";
    }
  | {
      readonly issueId: string;
      readonly mode: null;
      readonly reason: "architect_veto";
    }
  | IdleAdvisory;

export interface LiveWorker {
  readonly sessionId?: string;
  readonly mode?: WorkerModeLiteral;
  readonly status?: string;
}

export type LiveWorkers = Record<string, readonly LiveWorker[]>;

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

export interface ResyncControllerEvent {
  readonly type: "legion.resync";
  readonly recommendations: readonly Recommendation[];
  readonly errors: readonly ResyncError[];
}

export interface RunResyncDeps {
  readonly listNonTerminalIssues: () => IssueRef[];
  readonly fetchPhaseArtifactsBatch: (refs: readonly IssueRef[]) => Promise<PhaseArtifactBatch>;
  readonly getLiveWorkers: () => Promise<LiveWorkers>;
  // reserved for the idle-worker advisory (U9)
  readonly getSessionStatus: (sessionId: string) => Promise<SessionStatusObservation>;
  readonly emitToController: (event: ResyncControllerEvent) => void | Promise<void>;
}

export interface ResyncInput {
  readonly issueId: string;
  readonly status: string;
  readonly artifacts: PhaseArtifacts;
  readonly liveWorkerModes: readonly WorkerModeLiteral[];
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

function ownerFromArtifacts(
  artifacts: PhaseArtifacts
): WorkerModeLiteral | "architect_veto" | null | undefined {
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
  if (artifacts.architectCheckOnHead === "failure") return "architect_veto";
  if (artifacts.resolved.autoMergeEnabledOrMerged !== "resolved") return null;
  return artifacts.autoMergeEnabledOrMerged ? null : WorkerMode.MERGE;
}

function expectedOwner(input: ResyncInput): WorkerModeLiteral | "architect_veto" | null {
  if (input.status === IssueStatus.DONE) return null;
  const artifactOwner = ownerFromArtifacts(input.artifacts);
  return artifactOwner === undefined ? statusFallback(input.status) : artifactOwner;
}

function sessionStatusType(observation: SessionStatusObservation): string {
  const data = observation.data;
  if (typeof data !== "object" || data === null || !("type" in data)) return "unresolvable";
  return typeof data.type === "string" ? data.type : "unresolvable";
}

export function computeResyncRecommendations(inputs: readonly ResyncInput[]): Recommendation[] {
  const recommendations: Recommendation[] = [];
  for (const input of inputs) {
    const mode = expectedOwner(input);
    if (mode === "architect_veto") {
      recommendations.push({ issueId: input.issueId, mode: null, reason: "architect_veto" });
      continue;
    }
    if (mode && !input.liveWorkerModes.includes(mode)) {
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
  const artifactRecommendations = computeResyncRecommendations(
    refs.map((ref) => ({
      issueId: ref.issueId,
      status: ref.status,
      artifacts: artifactBatch.artifacts[ref.issueId] ?? unresolvablePhaseArtifacts(),
      liveWorkerModes: (liveWorkers[ref.issueId] ?? []).flatMap((worker) =>
        worker.mode ? [worker.mode] : []
      ),
    }))
  );
  const idleAdvisoryInputs = await Promise.all(
    refs.flatMap((ref) =>
      (liveWorkers[ref.issueId] ?? []).map(async (worker) => {
        if (!worker.sessionId || !worker.mode) return null;

        const observation = await deps.getSessionStatus(worker.sessionId);
        return {
          issueId: ref.issueId,
          mode: worker.mode,
          sessionStatusType: sessionStatusType(observation),
          artifacts: artifactBatch.artifacts[ref.issueId] ?? unresolvablePhaseArtifacts(),
        };
      })
    )
  );
  const idleAdvisories = computeIdleAdvisories(
    idleAdvisoryInputs.flatMap((input) => (input ? [input] : []))
  );
  const recommendations = [...artifactRecommendations, ...idleAdvisories];
  if (recommendations.length > 0 || artifactBatch.errors.length > 0) {
    await deps.emitToController({
      type: "legion.resync",
      recommendations,
      errors: artifactBatch.errors,
    });
  }
  return { recommendations, errors: artifactBatch.errors };
}
