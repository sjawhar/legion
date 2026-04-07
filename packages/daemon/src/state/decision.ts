/**
 * Decision logic for Legion state collection.
 *
 * Pure functions that determine actions based on state.
 * No I/O operations - just business logic.
 */

import {
  type ActionType,
  CiStatus,
  type CiStatusLiteral,
  type CollectedState,
  computeSessionId,
  type FetchedIssueData,
  type IssueState,
  IssueStatus,
  type IssueStatusLiteral,
  MergeableStatus,
  type MergeableStatusLiteral,
  WorkerMode,
  type WorkerModeLiteral,
} from "./types";

export function suggestAction(
  status: IssueStatusLiteral | string,
  hasWorkerDone: boolean,
  hasLiveWorker: boolean,
  prIsDraft: boolean | null,
  hasPr: boolean,
  hasTestPassed: boolean,
  ciStatus: CiStatusLiteral | null = null,
  mergeableStatus: MergeableStatusLiteral | null = null
): ActionType {
  switch (status) {
    case IssueStatus.DONE:
      return "skip";

    case IssueStatus.TRIAGE:
    case IssueStatus.ICEBOX:
      return "skip";

    case IssueStatus.BACKLOG:
      if (hasWorkerDone) {
        return "transition_to_todo";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_architect";

    case IssueStatus.TODO:
      if (hasWorkerDone) {
        return "transition_to_in_progress";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_planner";

    case IssueStatus.IN_PROGRESS:
      if (hasWorkerDone) {
        if (!hasPr) {
          return "investigate_no_pr";
        }
        if (ciStatus === CiStatus.FAILING) {
          return "resume_implementer_for_ci_failure";
        }
        if (ciStatus === CiStatus.PENDING) {
          return "retry_ci_check";
        }
        return "transition_to_testing";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_implementer";

    case IssueStatus.TESTING:
      if (hasWorkerDone) {
        if (hasTestPassed) {
          if (!hasPr) {
            return "investigate_no_pr";
          }
          if (ciStatus === CiStatus.FAILING) {
            return "resume_implementer_for_ci_failure";
          }
          if (ciStatus === CiStatus.PENDING) {
            return "retry_ci_check";
          }
          return "transition_to_needs_review";
        }
        return "resume_implementer_for_test_failure";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_tester";

    case IssueStatus.NEEDS_REVIEW:
      if (hasWorkerDone) {
        if (!hasPr) {
          return "investigate_no_pr";
        }
        if (prIsDraft === null) {
          return "retry_pr_check";
        }
        if (prIsDraft) {
          return "resume_implementer_for_changes";
        }
        // Check merge conflicts before CI status.
        // Conflicts can cause CI failures, so resolve them first.
        if (mergeableStatus === MergeableStatus.UNKNOWN) {
          return "retry_pr_check";
        }
        if (mergeableStatus === MergeableStatus.CONFLICTING) {
          return "rebase_pr";
        }
        if (ciStatus === CiStatus.FAILING) {
          return "resume_implementer_for_ci_failure";
        }
        if (ciStatus === CiStatus.PENDING) {
          return "retry_ci_check";
        }
        return "transition_to_retro";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      if (hasPr && mergeableStatus === MergeableStatus.CONFLICTING) {
        return "rebase_pr";
      }
      if (hasPr && ciStatus === CiStatus.FAILING) {
        return "resume_implementer_for_ci_failure";
      }
      if (hasPr && ciStatus === CiStatus.PENDING) {
        return "retry_ci_check";
      }
      return "dispatch_reviewer";

    case IssueStatus.RETRO:
      if (hasWorkerDone) {
        return "dispatch_merger";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_implementer_for_retro";

    default:
      return "skip";
  }
}

export const ACTION_TO_MODE: Record<ActionType, WorkerModeLiteral> = {
  skip: WorkerMode.IMPLEMENT,
  investigate_no_pr: WorkerMode.IMPLEMENT,
  dispatch_architect: WorkerMode.ARCHITECT,
  dispatch_planner: WorkerMode.PLAN,
  dispatch_implementer: WorkerMode.IMPLEMENT,
  dispatch_implementer_for_retro: WorkerMode.IMPLEMENT,
  dispatch_reviewer: WorkerMode.REVIEW,
  dispatch_merger: WorkerMode.MERGE,
  resume_implementer_for_changes: WorkerMode.IMPLEMENT,
  resume_implementer_for_retro: WorkerMode.IMPLEMENT,
  transition_to_in_progress: WorkerMode.IMPLEMENT,
  transition_to_needs_review: WorkerMode.REVIEW,
  transition_to_todo: WorkerMode.PLAN,
  transition_to_retro: WorkerMode.IMPLEMENT,
  transition_to_done: WorkerMode.MERGE,
  relay_user_feedback: WorkerMode.IMPLEMENT,
  remove_worker_active_and_redispatch: WorkerMode.IMPLEMENT,
  add_needs_approval: WorkerMode.PLAN,
  retry_pr_check: WorkerMode.REVIEW,
  resume_implementer_for_ci_failure: WorkerMode.IMPLEMENT,
  retry_ci_check: WorkerMode.REVIEW,
  dispatch_tester: WorkerMode.TEST,
  transition_to_testing: WorkerMode.TEST,
  resume_implementer_for_test_failure: WorkerMode.IMPLEMENT,
  rebase_pr: WorkerMode.REVIEW,
};

/**
 * Modes that require server-side validation before dispatch.
 * Non-gated modes are always allowed (backward compatible).
 */
export const GATED_MODES: ReadonlySet<WorkerModeLiteral> = new Set<WorkerModeLiteral>([
  WorkerMode.MERGE,
]);

/**
 * Actions that represent actual worker dispatch (not transitions/resumes/skips).
 */
const DISPATCH_ACTIONS: ReadonlySet<string> = new Set([
  "dispatch_architect",
  "dispatch_planner",
  "dispatch_implementer",
  "dispatch_implementer_for_retro",
  "dispatch_tester",
  "dispatch_reviewer",
  "dispatch_merger",
]);

export type DispatchValidation =
  | { valid: true }
  | { valid: false; suggestedAction: ActionType; reason: string };

/**
 * Validate whether a worker mode can be dispatched given cached issue state.
 *
 * For gated modes (initially: merge), the cached suggestedAction must be a
 * dispatch action that maps to the requested mode. This prevents the controller
 * from bypassing lifecycle phases (e.g., skipping retro before merge).
 *
 * For non-gated modes, always returns valid (backward compatible).
 * For cache misses (undefined state), always returns valid (graceful degradation).
 */
export function canDispatchMode(
  cachedState: IssueState | undefined,
  requestedMode: WorkerModeLiteral
): DispatchValidation {
  if (!GATED_MODES.has(requestedMode)) {
    return { valid: true };
  }
  if (cachedState === undefined) {
    return { valid: true };
  }

  const { suggestedAction } = cachedState;
  const suggestedMode = ACTION_TO_MODE[suggestedAction];

  if (DISPATCH_ACTIONS.has(suggestedAction) && suggestedMode === requestedMode) {
    return { valid: true };
  }

  return {
    valid: false,
    suggestedAction,
    reason:
      `Cannot dispatch "${requestedMode}" worker: ` +
      `current suggestedAction is "${suggestedAction}" ` +
      `(maps to "${suggestedMode}" mode). ` +
      `The issue must reach the correct lifecycle state before "${requestedMode}" can be dispatched.`,
  };
}

const VALID_WORKER_MODES = new Set<string>([
  WorkerMode.ARCHITECT,
  WorkerMode.PLAN,
  WorkerMode.IMPLEMENT,
  WorkerMode.TEST,
  WorkerMode.REVIEW,
  WorkerMode.MERGE,
]);

export function buildIssueState(data: FetchedIssueData, legionId: string): IssueState {
  let action: ActionType;

  if (data.hasUserInputNeeded && data.hasUserFeedback) {
    action = "relay_user_feedback";
  } else if (data.hasUserInputNeeded) {
    action = "skip";
  } else if (data.labels.includes("worker-active") && !data.hasLiveWorker) {
    action = "remove_worker_active_and_redispatch";
  } else if (
    data.hasNeedsApproval &&
    data.hasHumanApproved &&
    (data.status === IssueStatus.BACKLOG || data.status === IssueStatus.TODO)
  ) {
    action = "transition_to_todo";
  } else if (
    data.hasNeedsApproval &&
    (data.status === IssueStatus.BACKLOG || data.status === IssueStatus.TODO)
  ) {
    action = "skip";
  } else {
    action = suggestAction(
      data.status,
      data.labels.includes("worker-done"),
      data.hasLiveWorker,
      data.prIsDraft,
      data.hasPr,
      data.hasTestPassed ?? false,
      data.ciStatus,
      data.mergeableStatus
    );
    if (action === "transition_to_todo") {
      action = "add_needs_approval";
    }
  }

  if (data.isBlocked === true && action.startsWith("dispatch_")) {
    action = "skip";
  }

  // Use actual worker mode for skip actions when available
  let mode: WorkerModeLiteral;
  if (
    action === "skip" &&
    data.hasLiveWorker &&
    data.workerMode &&
    VALID_WORKER_MODES.has(data.workerMode)
  ) {
    mode = data.workerMode as WorkerModeLiteral;
  } else {
    mode = ACTION_TO_MODE[action] ?? WorkerMode.IMPLEMENT;
  }
  const sessionId = computeSessionId(legionId, data.issueId, mode);

  return {
    title: data.title,
    status: data.status,
    labels: data.labels,
    hasPr: data.hasPr,
    prIsDraft: data.prIsDraft,
    ciStatus: data.ciStatus,
    mergeableStatus: data.mergeableStatus,
    hasLiveWorker: data.hasLiveWorker,
    workerMode: data.workerMode,
    workerStatus: data.workerStatus,
    suggestedAction: action,
    sessionId,
    hasUserFeedback: data.hasUserFeedback,
    isBlocked: data.isBlocked ?? false,
    source: data.source,
  };
}

export function buildCollectedState(
  issuesData: FetchedIssueData[],
  legionId: string
): CollectedState {
  const result: CollectedState = { issues: {} };

  for (const data of issuesData) {
    result.issues[data.issueId] = buildIssueState(data, legionId);
  }

  return result;
}
