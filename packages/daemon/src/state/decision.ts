/**
 * Decision logic for Legion state collection.
 *
 * Pure functions that determine actions based on state.
 * No I/O operations - just business logic.
 */

import {
  type ActionType,
  type CollectedState,
  computeSessionId,
  type FetchedIssueData,
  type IssueState,
  IssueStatus,
  type IssueStatusLiteral,
  WorkerMode,
  type WorkerModeLiteral,
} from "./types";

export function suggestAction(
  status: IssueStatusLiteral | string,
  hasWorkerDone: boolean,
  hasLiveWorker: boolean,
  prIsDraft: boolean | null,
  hasPr: boolean
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
        return "transition_to_needs_review";
      }
      if (hasPr && !hasLiveWorker) {
        return "skip";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_implementer";

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
        return "transition_to_retro";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_reviewer";

    case IssueStatus.RETRO:
      if (hasWorkerDone) {
        return "dispatch_merger";
      }
      return "resume_implementer_for_retro";

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
  dispatch_reviewer: WorkerMode.REVIEW,
  dispatch_merger: WorkerMode.MERGE,
  resume_implementer_for_changes: WorkerMode.IMPLEMENT,
  resume_implementer_for_retro: WorkerMode.IMPLEMENT,
  transition_to_in_progress: WorkerMode.IMPLEMENT,
  transition_to_needs_review: WorkerMode.REVIEW,
  transition_to_todo: WorkerMode.PLAN,
  transition_to_retro: WorkerMode.IMPLEMENT,
  relay_user_feedback: WorkerMode.IMPLEMENT,
  remove_worker_active_and_redispatch: WorkerMode.IMPLEMENT,
  add_needs_approval: WorkerMode.PLAN,
  retry_pr_check: WorkerMode.REVIEW,
};

export function buildIssueState(data: FetchedIssueData, teamId: string): IssueState {
  let action: ActionType;

  if (data.hasUserInputNeeded && data.hasUserFeedback) {
    action = "relay_user_feedback";
  } else if (data.hasUserInputNeeded) {
    action = "skip";
  } else if (data.labels.includes("worker-active") && !data.hasLiveWorker) {
    action = "remove_worker_active_and_redispatch";
  } else if (data.hasNeedsApproval && data.hasHumanApproved) {
    action = "transition_to_todo";
  } else if (data.hasNeedsApproval) {
    action = "skip";
  } else {
    action = suggestAction(
      data.status,
      data.labels.includes("worker-done"),
      data.hasLiveWorker,
      data.prIsDraft,
      data.hasPr
    );
    if (action === "transition_to_todo") {
      action = "add_needs_approval";
    }
  }

  const mode = ACTION_TO_MODE[action] ?? WorkerMode.IMPLEMENT;
  const sessionId = computeSessionId(teamId, data.issueId, mode);

  return {
    status: data.status,
    labels: data.labels,
    hasPr: data.hasPr,
    prIsDraft: data.prIsDraft,
    hasLiveWorker: data.hasLiveWorker,
    suggestedAction: action,
    sessionId,
    hasUserFeedback: data.hasUserFeedback,
  };
}

export function buildCollectedState(
  issuesData: FetchedIssueData[],
  teamId: string
): CollectedState {
  const result: CollectedState = { issues: {} };

  for (const data of issuesData) {
    result.issues[data.issueId] = buildIssueState(data, teamId);
  }

  return result;
}
