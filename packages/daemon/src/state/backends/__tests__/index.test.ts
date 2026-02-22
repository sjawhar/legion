import { describe, expect, it } from "bun:test";
import { getBackend } from "../index";

describe("getBackend", () => {
  it("returns a LinearTracker for 'linear'", () => {
    const backend = getBackend("linear");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
  });

  it("returns a GitHubTracker for 'github'", () => {
    const backend = getBackend("github");
    expect(backend).toBeDefined();
    expect(typeof backend.parseIssues).toBe("function");
  });

  it("throws for unknown backend", () => {
    const invalidBackend = "jira" as "linear" | "github";
    expect(() => getBackend(invalidBackend)).toThrow("Unknown backend");
  });

  it("github backend skips non-Issue content types", () => {
    const backend = getBackend("github");
    const result = backend.parseIssues([
      { id: "PVTI_1", content: { type: "DraftIssue" }, status: "Todo", labels: [] },
    ]);
    expect(result).toEqual([]);
  });

  it("linear backend parses Linear-format issues", () => {
    const backend = getBackend("linear");
    const result = backend.parseIssues([
      { identifier: "ENG-1", state: { name: "Todo" }, labels: { nodes: [] } },
    ]);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("ENG-1");
  });
});
