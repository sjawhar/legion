/**
 * Tests for state types module.
 * 
 * Ported from Python tests:
 * - tests/test_state.py (TestComputeSessionId, TestComputeControllerSessionId, TestIssueStatus, TestParsedIssueWorkerActive)
 * - tests/test_session_naming.py (session ID computation tests)
 */

import { describe, it, expect } from "bun:test";
import { validate as validateUuid } from "uuid";
import {
  IssueStatus,
  computeSessionId,
  computeControllerSessionId,
  GitHubPRRef,
  createParsedIssue,
} from "../types";

describe("computeSessionId", () => {
  it("returns uuid string with ses_ prefix", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result = computeSessionId(teamId, "ENG-21", "implement");
    
    // Should have ses_ prefix
    expect(result).toMatch(/^ses_/);
    
    // After removing prefix, should be valid UUID
    const uuidPart = result.slice(4);
    expect(validateUuid(uuidPart)).toBe(true);
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

  it("throws error for invalid team id", () => {
    expect(() => {
      computeSessionId("not-a-valid-uuid", "ENG-21", "implement");
    }).toThrow();
  });
});

describe("computeControllerSessionId", () => {
  it("returns uuid string with ses_ prefix", () => {
    const teamId = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const result = computeControllerSessionId(teamId);
    
    // Should have ses_ prefix
    expect(result).toMatch(/^ses_/);
    
    // After removing prefix, should be valid UUID
    const uuidPart = result.slice(4);
    expect(validateUuid(uuidPart)).toBe(true);
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

  it("throws error for invalid team id", () => {
    expect(() => {
      computeControllerSessionId("not-a-valid-uuid");
    }).toThrow();
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

  it("returns unknown status unchanged", () => {
    expect(IssueStatus.normalize("Unknown")).toBe("Unknown");
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
    const issue = createParsedIssue(
      "ENG-21",
      "Needs Review",
      [],
      { owner: "owner", repo: "repo", number: 123 }
    );
    expect(issue.hasPr).toBe(true);
  });

  it("has_pr returns false when pr_ref is null", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", [], null);
    expect(issue.hasPr).toBe(false);
  });

  it("needs_pr_status returns true when conditions met", () => {
    const issue = createParsedIssue(
      "ENG-21",
      "Needs Review",
      ["worker-done"],
      { owner: "owner", repo: "repo", number: 123 }
    );
    expect(issue.needsPrStatus).toBe(true);
  });

  it("needs_pr_status returns false when status not Needs Review", () => {
    const issue = createParsedIssue(
      "ENG-21",
      "In Progress",
      ["worker-done"],
      { owner: "owner", repo: "repo", number: 123 }
    );
    expect(issue.needsPrStatus).toBe(false);
  });

  it("needs_pr_status returns false when no worker-done label", () => {
    const issue = createParsedIssue(
      "ENG-21",
      "Needs Review",
      [],
      { owner: "owner", repo: "repo", number: 123 }
    );
    expect(issue.needsPrStatus).toBe(false);
  });

  it("needs_pr_status returns false when no pr_ref", () => {
    const issue = createParsedIssue("ENG-21", "Needs Review", ["worker-done"], null);
    expect(issue.needsPrStatus).toBe(false);
  });
});
