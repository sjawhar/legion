import { describe, expect, it } from "bun:test";
import { GitHubTracker } from "../github";

const tracker = new GitHubTracker();

describe("GitHubTracker.parseIssues", () => {
  it("parses a basic GitHub project item", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: {
          number: 42,
          repository: "gh",
          url: "https://github.com/sjawhar/gh/issues/42",
          type: "Issue",
        },
        status: "In Progress",
        labels: ["worker-active"],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("GH-42");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].labels).toEqual(["worker-active"]);
    expect(result[0].hasWorkerActive).toBe(true);
    expect(result[0].prRef).toBeNull();
  });

  it("normalizes status aliases", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: {
          number: 1,
          repository: "gh",
          url: "https://github.com/o/gh/issues/1",
          type: "Issue",
        },
        status: "In Review",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].status).toBe("Needs Review");
  });

  it("skips non-issue items (DraftIssue, PullRequest)", () => {
    const items = [
      {
        id: "PVTI_draft",
        content: { title: "A draft", type: "DraftIssue" },
        status: "Todo",
        labels: [],
      },
      {
        id: "PVTI_issue",
        content: {
          number: 10,
          repository: "gh",
          url: "https://github.com/o/gh/issues/10",
          type: "Issue",
        },
        status: "Todo",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("GH-10");
  });

  it("handles items with no status", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: {
          number: 5,
          repository: "gh",
          url: "https://github.com/o/gh/issues/5",
          type: "Issue",
        },
        status: null,
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].status).toBe("");
  });

  it("handles items with no labels", () => {
    const items = [
      {
        id: "PVTI_abc",
        content: {
          number: 5,
          repository: "gh",
          url: "https://github.com/o/gh/issues/5",
          type: "Issue",
        },
        status: "Backlog",
        labels: null,
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result[0].labels).toEqual([]);
  });

  it("handles multi-repo with different repo names", () => {
    const items = [
      {
        id: "PVTI_1",
        content: {
          number: 1,
          repository: "frontend",
          url: "https://github.com/org/frontend/issues/1",
          type: "Issue",
        },
        status: "Todo",
        labels: [],
      },
      {
        id: "PVTI_2",
        content: {
          number: 1,
          repository: "backend",
          url: "https://github.com/org/backend/issues/1",
          type: "Issue",
        },
        status: "Todo",
        labels: [],
      },
    ];
    const result = tracker.parseIssues(items);
    expect(result).toHaveLength(2);
    expect(result[0].issueId).toBe("FRONTEND-1");
    expect(result[1].issueId).toBe("BACKEND-1");
  });

  it("returns empty array for empty input", () => {
    expect(tracker.parseIssues([])).toEqual([]);
  });

  it("returns empty array for null/undefined input", () => {
    expect(tracker.parseIssues(null)).toEqual([]);
    expect(tracker.parseIssues(undefined)).toEqual([]);
  });
});
