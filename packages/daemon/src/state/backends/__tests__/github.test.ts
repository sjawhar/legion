import { describe, expect, it } from "bun:test";
import { GitHubTracker } from "../github";

const tracker = new GitHubTracker();

function makeItem(overrides: Record<string, unknown> = {}) {
  return {
    id: "PVTI_abc",
    content: {
      number: 42,
      repository: "acme/widgets",
      url: "https://github.com/acme/widgets/issues/42",
      type: "Issue",
    },
    status: "In Progress",
    labels: [],
    ...overrides,
  };
}

describe("GitHubTracker.parseIssues", () => {
  it("parses a basic item from gh project item-list --format json", () => {
    const raw = { items: [makeItem({ labels: ["worker-active"] })] };
    const result = tracker.parseIssues(raw);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("acme-widgets-42");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].labels).toEqual(["worker-active"]);
    expect(result[0].hasWorkerActive).toBe(true);
    expect(result[0].source).toEqual({
      owner: "acme",
      repo: "widgets",
      number: 42,
      url: "https://github.com/acme/widgets/issues/42",
    });
  });

  it("unwraps items envelope from gh CLI output", () => {
    const raw = { items: [makeItem()], totalCount: 100 };
    const result = tracker.parseIssues(raw);
    expect(result).toHaveLength(1);
  });

  it("also accepts a bare array", () => {
    const result = tracker.parseIssues([makeItem()]);
    expect(result).toHaveLength(1);
  });

  it("normalizes status aliases", () => {
    const raw = { items: [makeItem({ status: "In Review" })] };
    const result = tracker.parseIssues(raw);
    expect(result[0].status).toBe("Needs Review");
  });

  it("skips non-issue items (DraftIssue, PullRequest)", () => {
    const raw = {
      items: [
        { id: "PVTI_draft", content: { title: "A draft", type: "DraftIssue" }, status: "Todo" },
        {
          id: "PVTI_pr",
          content: { number: 65, repository: "acme/auth", type: "PullRequest" },
          status: "Done",
        },
        makeItem(),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("acme-widgets-42");
  });

  it("handles items with no status", () => {
    const raw = { items: [makeItem({ status: null })] };
    const result = tracker.parseIssues(raw);
    expect(result[0].status).toBe("");
  });

  it("handles items with no labels", () => {
    const raw = { items: [makeItem({ labels: null })] };
    const result = tracker.parseIssues(raw);
    expect(result[0].labels).toEqual([]);
  });

  it("handles multi-repo multi-owner project", () => {
    const raw = {
      items: [
        makeItem({
          content: {
            number: 1,
            repository: "acme/widgets",
            url: "https://github.com/acme/widgets/issues/1",
            type: "Issue",
          },
        }),
        makeItem({
          content: {
            number: 1,
            repository: "acme/backend",
            url: "https://github.com/acme/backend/issues/1",
            type: "Issue",
          },
        }),
        makeItem({
          content: {
            number: 99,
            repository: "other-org/shared-lib",
            url: "https://github.com/other-org/shared-lib/issues/99",
            type: "Issue",
          },
        }),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result).toHaveLength(3);
    expect(result[0].issueId).toBe("acme-widgets-1");
    expect(result[1].issueId).toBe("acme-backend-1");
    expect(result[2].issueId).toBe("other-org-shared-lib-99");
    expect(result[2].source?.owner).toBe("other-org");
    expect(result[2].source?.repo).toBe("shared-lib");
  });

  it("extracts PR refs from linked pull requests", () => {
    const raw = {
      items: [
        makeItem({
          "linked pull requests": ["https://github.com/acme/widgets/pull/479"],
        }),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result[0].prRef).not.toBeNull();
    expect(result[0].prRef?.owner).toBe("acme");
    expect(result[0].prRef?.repo).toBe("widgets");
    expect(result[0].prRef?.number).toBe(479);
    expect(result[0].hasPr).toBe(true);
  });

  it("extracts PR refs with case-variant key names", () => {
    const variants = ["Linked pull requests", "Linked Pull Requests", "linked pull request"];
    for (const key of variants) {
      const raw = {
        items: [
          makeItem({
            [key]: ["https://github.com/acme/widgets/pull/100"],
          }),
        ],
      };
      const result = tracker.parseIssues(raw);
      expect(result[0].prRef).not.toBeNull();
      expect(result[0].prRef?.number).toBe(100);
    }
  });

  it("handles items without linked pull requests", () => {
    const raw = { items: [makeItem()] };
    const result = tracker.parseIssues(raw);
    expect(result[0].prRef).toBeNull();
    expect(result[0].hasPr).toBe(false);
  });

  it("skips items with null elements in array", () => {
    const result = tracker.parseIssues([null, undefined, 42, "string"]);
    expect(result).toEqual([]);
  });

  it("skips items with missing content field", () => {
    const result = tracker.parseIssues({ items: [{ id: "PVTI_abc", status: "Todo" }] });
    expect(result).toEqual([]);
  });

  it("skips items with repository missing owner", () => {
    const raw = {
      items: [
        makeItem({
          content: { number: 1, repository: "just-repo-name", type: "Issue" },
        }),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result).toEqual([]);
  });

  it("skips items with empty repository", () => {
    const raw = {
      items: [
        makeItem({
          content: { number: 1, repository: "", type: "Issue" },
        }),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result).toEqual([]);
  });

  it("sanitizes special characters in owner/repo for issue ID", () => {
    const raw = {
      items: [
        makeItem({
          content: {
            number: 1,
            repository: "org.test/my_repo",
            url: "https://github.com/org.test/my_repo/issues/1",
            type: "Issue",
          },
        }),
      ],
    };
    const result = tracker.parseIssues(raw);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("org-test-my-repo-1");
    expect(result[0].source?.owner).toBe("org.test");
    expect(result[0].source?.repo).toBe("my_repo");
  });

  it("returns empty array for null/undefined/object-without-items input", () => {
    expect(tracker.parseIssues(null)).toEqual([]);
    expect(tracker.parseIssues(undefined)).toEqual([]);
    expect(tracker.parseIssues({})).toEqual([]);
    expect(tracker.parseIssues({ items: "not-array" })).toEqual([]);
  });

  it("returns empty array for empty items", () => {
    expect(tracker.parseIssues({ items: [] })).toEqual([]);
    expect(tracker.parseIssues([])).toEqual([]);
  });
});
