/**
 * Tests for retained issue-tracker state types.
 */

import { describe, expect, it } from "bun:test";
import { createParsedIssue, GitHubPRRef, IssueStatus, LEGION_REPO_CONFIG } from "../types";

describe("IssueStatus.normalize", () => {
  it("returns direct match unchanged", () => {
    expect(IssueStatus.normalize("Todo")).toBe("Todo");
    expect(IssueStatus.normalize("In Progress")).toBe("In Progress");
  });

  it("normalizes alias to canonical form", () => {
    expect(IssueStatus.normalize("In Review")).toBe("Needs Review");
  });

  it("normalizes case-insensitive canonical match", () => {
    expect(IssueStatus.normalize("in progress")).toBe("In Progress");
    expect(IssueStatus.normalize("In progress")).toBe("In Progress");
    expect(IssueStatus.normalize("IN PROGRESS")).toBe("In Progress");
    expect(IssueStatus.normalize("todo")).toBe("Todo");
    expect(IssueStatus.normalize("BACKLOG")).toBe("Backlog");
    expect(IssueStatus.normalize("needs review")).toBe("Needs Review");
  });

  it("normalizes case-insensitive alias match", () => {
    expect(IssueStatus.normalize("in review")).toBe("Needs Review");
    expect(IssueStatus.normalize("IN REVIEW")).toBe("Needs Review");
  });

  it("returns unknown status unchanged", () => {
    expect(IssueStatus.normalize("Unknown")).toBe("Unknown");
    expect(IssueStatus.normalize("Scrapped")).toBe("Scrapped");
  });

  it("normalizes Today to Todo", () => {
    expect(IssueStatus.normalize("Today")).toBe("Todo");
    expect(IssueStatus.normalize("today")).toBe("Todo");
  });

  it("returns empty string for null", () => {
    expect(IssueStatus.normalize(null)).toBe("");
  });
});

describe("GitHubPRRef.fromUrl", () => {
  it("parses valid PR URL", () => {
    const url = "https://github.com/owner/repo/pull/123";
    const ref = GitHubPRRef.fromUrl(url);

    expect(ref).not.toBeNull();
    expect(ref?.owner).toBe("owner");
    expect(ref?.repo).toBe("repo");
    expect(ref?.number).toBe(123);
  });

  it("returns null for invalid URL format", () => {
    expect(GitHubPRRef.fromUrl("not-a-url")).toBeNull();
    expect(GitHubPRRef.fromUrl("https://github.com/owner/repo/issues/123")).toBeNull();
    expect(GitHubPRRef.fromUrl("https://gitlab.com/owner/repo/pull/123")).toBeNull();
  });

  it("returns null for unreasonably large PR number", () => {
    const url = "https://github.com/owner/repo/pull/9999999999";
    const ref = GitHubPRRef.fromUrl(url);
    expect(ref).toBeNull();
  });

  it("handles owner and repo with hyphens, underscores, dots", () => {
    const url = "https://github.com/my-org.test/my_repo-2.0/pull/456";
    const ref = GitHubPRRef.fromUrl(url);

    expect(ref).not.toBeNull();
    expect(ref?.owner).toBe("my-org.test");
    expect(ref?.repo).toBe("my_repo-2.0");
    expect(ref?.number).toBe(456);
  });
});

describe("ParsedIssue properties", () => {
  it("has_worker_done returns true when label present", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["worker-done"], null);
    expect(issue.hasWorkerDone).toBe(true);
  });

  it("has_worker_done returns false when label absent", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["other-label"], null);
    expect(issue.hasWorkerDone).toBe(false);
  });

  it("has_worker_active returns true when label present", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["worker-active"], null);
    expect(issue.hasWorkerActive).toBe(true);
  });

  it("has_worker_active returns false when label absent", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", [], null);
    expect(issue.hasWorkerActive).toBe(false);
  });

  it("has_user_feedback returns true when label present", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["user-feedback-given"], null);
    expect(issue.hasUserFeedback).toBe(true);
  });

  it("has_user_input_needed returns true when label present", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["user-input-needed"], null);
    expect(issue.hasUserInputNeeded).toBe(true);
  });

  it("has_pr returns true when pr_ref exists", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", [], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.hasPr).toBe(true);
  });

  it("has_pr returns false when pr_ref is null", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", [], null);
    expect(issue.hasPr).toBe(false);
  });

  it("needs_pr_status returns true when conditions met", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", ["worker-done"], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsPrStatus).toBe(true);
  });

  it("needs_pr_status returns false when status not Needs Review or In Progress", () => {
    const issue = createParsedIssue("ENG-21", "Testing", ["worker-done"], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsPrStatus).toBe(false);
  });

  it("needs_pr_status returns false when no worker-done label", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", [], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsPrStatus).toBe(false);
  });

  it("needs_pr_status returns false when no pr_ref", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", ["worker-done"], null);
    expect(issue.needsPrStatus).toBe(false);
  });

  it("needs_pr_status returns true for In Progress with worker-done and PR", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["worker-done"], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsPrStatus).toBe(true);
  });

  // needsCiStatus enrichment tests
  it("needsCiStatus returns true for In Progress with PR", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", [], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsCiStatus).toBe(true);
  });

  it("needsCiStatus returns true for Testing with PR", () => {
    const issue = createParsedIssue("ENG-21", "Testing", [], {
      owner: "owner",
      repo: "repo",
      number: 123,
    });
    expect(issue.needsCiStatus).toBe(true);
  });

  it("needsCiStatus returns false for In Progress without PR", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", [], null);
    expect(issue.needsCiStatus).toBe(false);
  });
});

describe("LEGION_REPO_CONFIG", () => {
  it("defines workspace config path", () => {
    expect(LEGION_REPO_CONFIG.PATH).toBe(".legion/config.yml");
  });

  it("includes documented recognized keys", () => {
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("merge.require_smoke_test");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("merge.require_reporter_approval");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("merge.auto_merge_allowed");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("testing.require_specific_task");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("testing.require_taiga_evidence");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("notifications.slack_channel");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("notifications.ping_reporter_on_pr");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("skills.required");
    expect(LEGION_REPO_CONFIG.RECOGNIZED_KEYS).toContain("phases.<mode>.*");
  });
});
