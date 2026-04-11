import { describe, expect, it } from "bun:test";
import type { IssueStateDict } from "../../state/types";
import { formatPollOutput } from "../poll-formatter";

function makeIssue(overrides: Partial<IssueStateDict>): IssueStateDict {
  return {
    status: "In Progress",
    labels: [],
    hasPr: false,
    prIsDraft: null,
    ciStatus: null,
    mergeableStatus: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    suggestedAction: "skip",
    sessionId: "ses_test",
    hasUserFeedback: false,
    isBlocked: false,
    source: null,
    ...overrides,
  };
}

describe("formatPollOutput", () => {
  it("renders actionable issues grouped by action with titles", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_implementer",
        status: "In Progress",
        source: { owner: "acme", repo: "repo", number: 42, url: "" },
      }),
      "acme-repo-43": makeIssue({
        suggestedAction: "dispatch_implementer",
        status: "In Progress",
        source: { owner: "acme", repo: "repo", number: 43, url: "" },
      }),
      "acme-repo-44": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
        source: { owner: "acme", repo: "repo", number: 44, url: "" },
      }),
    };
    const titles: Record<string, string> = {
      "acme-repo-42": "Fix widget alignment",
      "acme-repo-43": "Add dark mode",
      "acme-repo-44": "Redesign settings page",
    };

    const output = formatPollOutput(issues, titles);
    expect(output).toContain("ACTIONABLE (3):");
    expect(output).toContain("dispatch_implementer:");
    expect(output).toContain('#42  In Progress  "Fix widget alignment"');
    expect(output).toContain("dispatch_planner:");
    expect(output).toContain('#44  Todo  "Redesign settings page"');
  });

  it("renders blocked issues (user-input-needed, stale worker-active)", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-50": makeIssue({
        // Real state machine: user-input-needed issues may have relay_user_feedback action
        suggestedAction: "relay_user_feedback",
        status: "In Progress",
        labels: ["user-input-needed"],
        source: { owner: "acme", repo: "repo", number: 50, url: "" },
      }),
      "acme-repo-51": makeIssue({
        // Real state machine: stale worker-active gets remove_worker_active_and_redispatch
        suggestedAction: "remove_worker_active_and_redispatch",
        status: "In Progress",
        labels: ["worker-active"],
        hasLiveWorker: false,
        source: { owner: "acme", repo: "repo", number: 51, url: "" },
      }),
    };
    const titles: Record<string, string> = {
      "acme-repo-50": "Zendesk ticketing",
      "acme-repo-51": "Profile pic storage",
    };

    const output = formatPollOutput(issues, titles);
    expect(output).toContain("BLOCKED (2):");
    expect(output).toContain('#50  user-input-needed  "Zendesk ticketing"');
    expect(output).toContain('#51  worker-active (stale)  "Profile pic storage"');
    // These should NOT appear in ACTIONABLE
    expect(output).not.toContain("ACTIONABLE");
  });

  it("renders summary counts for non-actionable skip issues", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-100": makeIssue({ suggestedAction: "skip", status: "Done" }),
      "acme-repo-101": makeIssue({ suggestedAction: "skip", status: "Done" }),
      "acme-repo-102": makeIssue({ suggestedAction: "skip", status: "Icebox" }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).toContain("SUMMARY:");
    expect(output).toContain("Done: 2");
    expect(output).toContain("Icebox: 1");
  });

  it("omits empty sections", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
      }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).not.toContain("BLOCKED");
    expect(output).not.toContain("SUMMARY");
  });

  it("falls back to issueId when source is null", () => {
    const issues: Record<string, IssueStateDict> = {
      "acme-repo-42": makeIssue({
        suggestedAction: "dispatch_planner",
        status: "Todo",
        source: null,
      }),
    };
    const output = formatPollOutput(issues, {});
    expect(output).toContain("acme-repo-42  Todo");
  });
});
