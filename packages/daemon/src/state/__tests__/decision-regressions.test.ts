import { describe, expect, it } from "bun:test";
import { buildIssueState } from "../decision";
import type { FetchedIssueData } from "../types";

describe("decision regressions (known pipeline stalls)", () => {
  it("dispatches implementer when In Progress issue has PR but no live worker and no worker-done", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      ciStatus: null,
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

  it("skips Retro when a live worker exists (worker is already running retro)", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-22",
      status: "Retro",
      labels: [],
      hasPr: true,
      prIsDraft: false,
      ciStatus: null,
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
});

describe("CI gating regressions", () => {
  it("does not advance to retro when CI is failing on a ready PR", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-23",
      status: "Needs Review",
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: false,
      ciStatus: "failing",
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
    expect(state.suggestedAction).toBe("resume_implementer_for_ci_failure");
  });

  it("does not dispatch reviewer when CI is failing", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-24",
      status: "Needs Review",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      ciStatus: "failing",
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
    expect(state.suggestedAction).toBe("resume_implementer_for_ci_failure");
  });

  it("waits when CI is pending before dispatching reviewer", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-25",
      status: "Needs Review",
      labels: [],
      hasPr: true,
      prIsDraft: null,
      ciStatus: "pending",
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
    expect(state.suggestedAction).toBe("retry_ci_check");
  });

  it("proceeds normally when CI is null (no checks configured)", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-26",
      status: "Needs Review",
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: false,
      ciStatus: null,
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
    expect(state.suggestedAction).toBe("transition_to_retro");
  });
});
