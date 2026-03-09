import { describe, expect, it } from "bun:test";
import { buildIssueState } from "../decision";
import type { FetchedIssueData } from "../types";

describe("decision regressions (known pipeline stalls)", () => {
  it("re-dispatches implementer for In Progress with dead worker and existing PR", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "In Progress",
      labels: [],
      hasPr: true,
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

  it("skips Retro when a live worker exists (prevents retro spam)", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-22",
      status: "Retro",
      labels: [],
      hasPr: true,
      prIsDraft: false,
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
