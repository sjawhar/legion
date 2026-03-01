/**
 * Tests for state decision logic.
 *
 * Ported from Python tests:
 * - tests/test_state.py (TestSuggestAction, TestBuildIssueState, TestOrphanDetection, TestBuildCollectedState)
 */

import { describe, expect, it } from "bun:test";
import { ACTION_TO_MODE, buildCollectedState, buildIssueState, suggestAction } from "../decision";
import { CollectedState, computeSessionId, type FetchedIssueData, IssueStatus } from "../types";

describe("suggestAction", () => {
  it("todo_no_worker_done_no_live_worker dispatches planner", () => {
    const action = suggestAction(IssueStatus.TODO, false, false, null, false, false);
    expect(action).toBe("dispatch_planner");
  });

  it("todo_worker_done transitions to in progress", () => {
    const action = suggestAction(IssueStatus.TODO, true, false, null, false, false);
    expect(action).toBe("transition_to_in_progress");
  });

  it("in_progress_no_worker_no_done dispatches implementer", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, false, false, null, false, false);
    expect(action).toBe("dispatch_implementer");
  });

  it("in_progress_worker_done transitions to testing", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, true, false, null, false, false);
    expect(action).toBe("transition_to_testing");
  });

  it("in_progress_with_live_worker skips", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, false, true, null, false, false);
    expect(action).toBe("skip");
  });

  it("needs_review_approved transitions to retro", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, false, true, false);
    expect(action).toBe("transition_to_retro");
  });

  it("needs_review_changes_requested resumes implementer", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, true, true, false);
    expect(action).toBe("resume_implementer_for_changes");
  });

  it("needs_review_worker_done_has_pr_but_unknown_status retries pr check", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, null, true, false);
    expect(action).toBe("retry_pr_check");
  });

  it("needs_review_worker_done_no_pr investigates", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, null, false, false);
    expect(action).toBe("investigate_no_pr");
  });

  it("needs_review_no_worker_done dispatches reviewer", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, false, false, null, false, false);
    expect(action).toBe("dispatch_reviewer");
  });

  it("needs_review_with_live_worker_no_done skips", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, false, true, null, false, false);
    expect(action).toBe("skip");
  });

  it("needs_review_worker_done_ignores_live_worker", () => {
    const action = suggestAction(IssueStatus.NEEDS_REVIEW, true, true, false, true, false);
    expect(action).toBe("transition_to_retro");
  });

  it("backlog_no_worker_done dispatches architect", () => {
    const action = suggestAction(IssueStatus.BACKLOG, false, false, null, false, false);
    expect(action).toBe("dispatch_architect");
  });

  it("backlog_worker_done transitions to todo", () => {
    // suggestAction still returns transition_to_todo;
    // buildIssueState converts it to add_needs_approval
    const action = suggestAction(IssueStatus.BACKLOG, true, false, null, false, false);
    expect(action).toBe("transition_to_todo");
  });

  it("backlog_with_live_worker skips", () => {
    const action = suggestAction(IssueStatus.BACKLOG, false, true, null, false, false);
    expect(action).toBe("skip");
  });

  it("triage_skips", () => {
    const action = suggestAction(IssueStatus.TRIAGE, false, false, null, false, false);
    expect(action).toBe("skip");
  });

  it("icebox_skips", () => {
    const action = suggestAction(IssueStatus.ICEBOX, false, false, null, false, false);
    expect(action).toBe("skip");
  });

  it("retro_worker_done dispatches merger", () => {
    const action = suggestAction(IssueStatus.RETRO, true, false, null, false, false);
    expect(action).toBe("dispatch_merger");
  });

  it("retro_without_live_worker_dispatches_implementer_for_retro", () => {
    const action = suggestAction(IssueStatus.RETRO, false, false, null, false, false);
    expect(action).toBe("dispatch_implementer_for_retro");
  });

  it("retro_with_live_worker_skips", () => {
    const action = suggestAction(IssueStatus.RETRO, false, true, null, false, false);
    expect(action).toBe("skip");
  });

  it("retry_pr_check_is_distinct_from_skip", () => {
    const retry = suggestAction(IssueStatus.NEEDS_REVIEW, true, false, null, true, false);
    const skip = suggestAction(IssueStatus.DONE, false, false, null, false, false);
    expect(retry).toBe("retry_pr_check");
    expect(skip).toBe("skip");
    expect(retry).not.toBe(skip);
  });

  it("in_progress_has_pr_no_worker_done_no_live_worker_dispatches_implementer", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, false, false, null, true, false);
    expect(action).toBe("dispatch_implementer");
  });

  it("done_always_skips", () => {
    const action = suggestAction(IssueStatus.DONE, false, false, null, false, false);
    expect(action).toBe("skip");
  });

  // Testing status
  it("testing_no_worker_done_no_live_worker_dispatches_tester", () => {
    const action = suggestAction(IssueStatus.TESTING, false, false, null, false, false);
    expect(action).toBe("dispatch_tester");
  });

  it("testing_worker_done_test_passed_transitions_to_needs_review", () => {
    const action = suggestAction(IssueStatus.TESTING, true, false, null, true, true);
    expect(action).toBe("transition_to_needs_review");
  });

  it("testing_worker_done_test_failed_resumes_implementer", () => {
    const action = suggestAction(IssueStatus.TESTING, true, false, null, true, false);
    expect(action).toBe("resume_implementer_for_test_failure");
  });

  it("testing_with_live_worker_skips", () => {
    const action = suggestAction(IssueStatus.TESTING, false, true, null, false, false);
    expect(action).toBe("skip");
  });

  it("unknown_status_skips", () => {
    const action = suggestAction("SomeUnknownStatus", false, false, null, false, false);
    expect(action).toBe("skip");
  });
});

describe("buildIssueState", () => {
  it("builds_state_with_action", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Todo",
      labels: [],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("dispatch_planner");
  });

  it("skips_when_user_input_needed", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Todo",
      labels: ["user-input-needed"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("skip");
  });

  it("relay_user_feedback_when_user_responded", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("relay_user_feedback");
  });

  it("waiting_for_feedback_skips", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["user-input-needed"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("skip");
  });

  it("feedback_without_input_needed_follows_normal_flow", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["user-feedback-given", "worker-done"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, teamId);

    expect(state.suggestedAction).toBe("transition_to_testing");
    const expectedSessionId = computeSessionId(teamId, "ENG-21", "test");
    expect(state.sessionId).toBe(expectedSessionId);
  });

  it("relay_feedback_computes_correct_session_id", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, teamId);

    const expectedSessionId = computeSessionId(teamId, "ENG-21", "implement");
    expect(state.sessionId).toBe(expectedSessionId);
    expect(state.suggestedAction).toBe("relay_user_feedback");
  });

  it("relay_feedback_in_different_statuses", () => {
    const teamId = "00000000-0000-0000-0000-000000000000";

    const dataTodo: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Todo",
      labels: ["user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };
    const stateTodo = buildIssueState(dataTodo, teamId);
    expect(stateTodo.suggestedAction).toBe("relay_user_feedback");

    const dataReview: FetchedIssueData = {
      issueId: "ENG-22",
      status: "Needs Review",
      labels: ["user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };
    const stateReview = buildIssueState(dataReview, teamId);
    expect(stateReview.suggestedAction).toBe("relay_user_feedback");
  });

  it("all_labels_present_relay_takes_precedence", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["worker-done", "user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("relay_user_feedback");
  });
});

  it("skip_with_live_worker_uses_actual_worker_mode_for_session_id", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Testing",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      hasLiveWorker: true,
      workerMode: "test",
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, teamId);
    expect(state.suggestedAction).toBe("skip");
    // sessionId should use the tester's mode, not the default implement mode
    const expectedSessionId = computeSessionId(teamId, "ENG-21", "test");
    expect(state.sessionId).toBe(expectedSessionId);
  });

  it("skip_without_worker_mode_falls_back_to_action_to_mode", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Done",
      labels: [],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, teamId);
    expect(state.suggestedAction).toBe("skip");
    // No workerMode → falls back to ACTION_TO_MODE["skip"] = implement
    const expectedSessionId = computeSessionId(teamId, "ENG-21", "implement");
    expect(state.sessionId).toBe(expectedSessionId);
  });

describe("approval gate", () => {
  it("backlog_worker_done_adds_needs_approval", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Backlog",
      labels: ["worker-done"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("add_needs_approval");
  });

  it("backlog_needs_approval_and_human_approved_transitions_to_todo", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Backlog",
      labels: ["needs-approval", "human-approved"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: true,
      hasHumanApproved: true,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("transition_to_todo");
  });

  it("backlog_needs_approval_without_human_approved_skips", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Backlog",
      labels: ["needs-approval"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: true,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("skip");
  });

  it("needs_approval_on_non_backlog_status_follows_normal_flow", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["needs-approval", "worker-done"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: true,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    // Should follow normal In Progress flow (worker-done → transition_to_testing)
    // NOT be frozen by leaked needs-approval label
    expect(state.suggestedAction).toBe("transition_to_testing");
  });

  it("needs_approval_on_todo_still_works", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Todo",
      labels: ["needs-approval"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: true,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("skip");
  });
});

describe("orphan detection", () => {
  it("orphan_detected_when_worker_active_but_no_live_worker", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["worker-active"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("remove_worker_active_and_redispatch");
  });

  it("no_orphan_when_worker_active_and_live_worker", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["worker-active"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: true,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("skip");
  });

  it("no_orphan_when_no_worker_active_label", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: [],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("dispatch_implementer");
  });

  it("orphan_detected_in_various_statuses", () => {
    const teamId = "00000000-0000-0000-0000-000000000000";

    for (const status of ["Todo", "In Progress", "Backlog", "Needs Review"]) {
      const data: FetchedIssueData = {
        issueId: "ENG-21",
        status,
        labels: ["worker-active"],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      };

      const state = buildIssueState(data, teamId);
      expect(state.suggestedAction).toBe("remove_worker_active_and_redispatch");
    }
  });

  it("user_feedback_takes_precedence_over_orphan", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: ["worker-active", "user-input-needed", "user-feedback-given"],
      hasPr: false,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: true,
      hasUserInputNeeded: true,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("relay_user_feedback");
  });

  it("orphan_action_mode_mapping", () => {
    expect("remove_worker_active_and_redispatch" in ACTION_TO_MODE).toBe(true);
  });
});

describe("buildCollectedState", () => {
  it("builds_state_for_multiple_issues", () => {
    const issuesData: FetchedIssueData[] = [
      {
        issueId: "ENG-21",
        status: "Todo",
        labels: [],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
      {
        issueId: "ENG-22",
        status: "In Progress",
        labels: ["worker-done"],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
    ];

    const state = buildCollectedState(issuesData, "00000000-0000-0000-0000-000000000000");

    expect("ENG-21" in state.issues).toBe(true);
    expect("ENG-22" in state.issues).toBe(true);
    expect(state.issues["ENG-21"].suggestedAction).toBe("dispatch_planner");
    expect(state.issues["ENG-22"].suggestedAction).toBe("transition_to_testing");
  });

  it("to_dict_serializes_correctly", () => {
    const issuesData: FetchedIssueData[] = [
      {
        issueId: "ENG-21",
        status: "Todo",
        labels: [],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
    ];

    const state = buildCollectedState(issuesData, "00000000-0000-0000-0000-000000000000");
    const result = CollectedState.toDict(state);

    expect("issues" in result).toBe(true);
    expect("ENG-21" in result.issues).toBe(true);
    expect(result.issues["ENG-21"].suggestedAction).toBe("dispatch_planner");
  });

  it("relay_feedback_with_multiple_issues", () => {
    const teamId = "00000000-0000-0000-0000-000000000000";
    const issuesData: FetchedIssueData[] = [
      {
        issueId: "ENG-21",
        status: "Todo",
        labels: [],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
      {
        issueId: "ENG-22",
        status: "In Progress",
        labels: ["user-input-needed", "user-feedback-given"],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: true,
        hasUserInputNeeded: true,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
      {
        issueId: "ENG-23",
        status: "In Progress",
        labels: ["user-input-needed"],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: true,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        hasTestPassed: false,
        hasTestFailed: false,
        source: null,
      },
    ];

    const state = buildCollectedState(issuesData, teamId);

    expect(Object.keys(state.issues)).toHaveLength(3);
    expect(state.issues["ENG-21"].suggestedAction).toBe("dispatch_planner");
    expect(state.issues["ENG-22"].suggestedAction).toBe("relay_user_feedback");
    expect(state.issues["ENG-23"].suggestedAction).toBe("skip");
  });
});
