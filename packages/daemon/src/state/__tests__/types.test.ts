/**
 * Tests for state types module.
 *
 * Ported from Python tests:
 * - tests/test_state.py (TestComputeSessionId, TestComputeControllerSessionId, TestIssueStatus, TestParsedIssueWorkerActive)
 * - tests/test_session_naming.py (session ID computation tests)
 */

import { describe, expect, it } from "bun:test";
import {
  computeControllerSessionId,
  computeSessionId,
  createParsedIssue,
  GitHubPRRef,
  IssueStatus,
} from "../types";

describe("computeSessionId", () => {
  it("returns OpenCode-format session ID", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result = computeSessionId(teamId, "ENG-21", "implement");

    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("same inputs produce same output", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result1 = computeSessionId(teamId, "ENG-21", "implement");
    const result2 = computeSessionId(teamId, "ENG-21", "implement");
    expect(result1).toBe(result2);
  });

  it("different issue produces different output", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result1 = computeSessionId(teamId, "ENG-21", "implement");
    const result2 = computeSessionId(teamId, "ENG-22", "implement");
    expect(result1).not.toBe(result2);
  });

  it("different mode produces different output", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result1 = computeSessionId(teamId, "ENG-21", "implement");
    const result2 = computeSessionId(teamId, "ENG-21", "review");
    expect(result1).not.toBe(result2);
  });

  it("accepts non-UUID team ID without throwing", () => {
    expect(() => {
      computeSessionId("not-a-valid-uuid", "ENG-21", "implement");
    }).not.toThrow();
  });
});

describe("computeControllerSessionId", () => {
  it("returns OpenCode-format session ID", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result = computeControllerSessionId(teamId);

    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("same input produces same output", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result1 = computeControllerSessionId(teamId);
    const result2 = computeControllerSessionId(teamId);
    expect(result1).toBe(result2);
  });

  it("different from worker session id", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const controllerId = computeControllerSessionId(teamId);
    const workerId = computeSessionId(teamId, "ENG-21", "implement");
    expect(controllerId).not.toBe(workerId);
  });

  it("accepts non-UUID team ID without throwing", () => {
    expect(() => {
      computeControllerSessionId("not-a-valid-uuid");
    }).not.toThrow();
  });
});

describe("computeSessionId with non-UUID team ID", () => {
  it("accepts a GitHub project ID string", () => {
    const result = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("produces deterministic known output (golden value)", () => {
    const result = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(result).toBe("ses_5f6e229e023c20L4w2B1RNa3WZ");
  });

  it("same non-UUID inputs produce same output", () => {
    const result1 = computeSessionId("sjawhar/5", "gh-42", "implement");
    const result2 = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(result1).toBe(result2);
  });

  it("different non-UUID team IDs produce different output", () => {
    const result1 = computeSessionId("sjawhar/5", "gh-42", "implement");
    const result2 = computeSessionId("sjawhar/6", "gh-42", "implement");
    expect(result1).not.toBe(result2);
  });

  it("non-UUID team ID produces different output from UUID team ID", () => {
    const uuidResult = computeSessionId(
      "7b4f0862-b775-4cb0-9a67-85400c6f44a8",
      "ENG-21",
      "implement"
    );
    const stringResult = computeSessionId("sjawhar/5", "ENG-21", "implement");
    expect(uuidResult).not.toBe(stringResult);
  });
});

describe("computeControllerSessionId with non-UUID team ID", () => {
  it("accepts a GitHub project ID string", () => {
    const result = computeControllerSessionId("sjawhar/5");
    expect(result).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
  });

  it("same non-UUID input produces same output", () => {
    const result1 = computeControllerSessionId("sjawhar/5");
    const result2 = computeControllerSessionId("sjawhar/5");
    expect(result1).toBe(result2);
  });
});

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
    expect(IssueStatus.normalize("Today")).toBe("Today");
    expect(IssueStatus.normalize("Scrapped")).toBe("Scrapped");
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

  it("needs_pr_status returns false when status not Needs Review", () => {
    const issue = createParsedIssue("ENG-21", "In Progress", ["worker-done"], {
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
});
