import { describe, expect, it } from "bun:test";
import { parseIssueIdParts } from "../github";
import { LinearTracker } from "../linear";

describe("parseIssueIdParts", () => {
  it("parses simple owner-repo-number format", () => {
    const result = parseIssueIdParts("acme-backend-42");
    expect(result).toEqual({ owner: "acme", repo: "backend", number: "42" });
  });

  it("parses repo with hyphens", () => {
    const result = parseIssueIdParts("acme-my-widgets-42");
    expect(result).toEqual({ owner: "acme", repo: "my-widgets", number: "42" });
  });

  it("parses when owner has hyphens too (first segment is always owner)", () => {
    // Convention: first segment is owner, but ambiguous
    const result = parseIssueIdParts("my-org-repo-100");
    expect(result).toEqual({ owner: "my", repo: "org-repo", number: "100" });
  });

  it("throws for invalid format (no number)", () => {
    expect(() => parseIssueIdParts("acme-backend")).toThrow("Cannot parse issueId");
  });

  it("throws for single segment", () => {
    expect(() => parseIssueIdParts("42")).toThrow("Cannot parse issueId");
  });
});

describe("LinearTracker.transitionIssue", () => {
  it("throws not-implemented error", async () => {
    const tracker = new LinearTracker();
    await expect(tracker.transitionIssue({ issueId: "ENG-42" }, "In Progress")).rejects.toThrow(
      "not implemented"
    );
  });
});

describe("LinearTracker.removeLabel", () => {
  it("throws not-implemented error", async () => {
    const tracker = new LinearTracker();
    await expect(tracker.removeLabel({ issueId: "ENG-42" }, "worker-done")).rejects.toThrow(
      "not implemented"
    );
  });
});
