import { describe, expect, it } from "bun:test";
import { LinearTracker } from "../linear";

const tracker = new LinearTracker();

describe("LinearTracker.parseIssues", () => {
  it("parses basic issue", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "In Progress" },
        labels: { nodes: [{ name: "worker-done" }] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].issueId).toBe("ENG-21");
    expect(result[0].status).toBe("In Progress");
    expect(result[0].hasWorkerDone).toBe(true);
  });

  it("normalizes status (In Review -> Needs Review)", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "In Review" },
        labels: { nodes: [] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result[0].status).toBe("Needs Review");
  });

  it("skips issues without identifier", () => {
    const issues = [
      { state: { name: "Todo" }, labels: { nodes: [] } },
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: { nodes: [] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
  });

  it("handles null state", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: null as unknown as undefined,
        labels: { nodes: [] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result[0].status).toBe("");
  });

  it("handles null labels", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: null as unknown as undefined,
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result[0].labels).toEqual([]);
  });

  it("returns empty array for non-array input", () => {
    expect(tracker.parseIssues(null)).toEqual([]);
    expect(tracker.parseIssues(undefined)).toEqual([]);
    expect(tracker.parseIssues("string")).toEqual([]);
    expect(tracker.parseIssues(42)).toEqual([]);
  });

  it("skips null elements in array", () => {
    const result = tracker.parseIssues([null, undefined, 42]);
    expect(result).toEqual([]);
  });

  it("extracts PR ref from valid attachment URL", () => {
    const issues = [
      {
        identifier: "ENG-1",
        state: { name: "Needs Review" },
        labels: { nodes: [{ name: "worker-done" }] },
        attachments: [{ url: "https://github.com/owner/repo/pull/123" }],
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].prRef).not.toBeNull();
    expect(result[0].prRef?.owner).toBe("owner");
    expect(result[0].prRef?.repo).toBe("repo");
    expect(result[0].prRef?.number).toBe(123);
  });
});

describe("LinearTracker.parseIssues edge cases", () => {
  it("handles deeply nested nulls", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: null as unknown as string },
        labels: { nodes: null as unknown as [] },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].status).toBe("");
    expect(result[0].labels).toEqual([]);
  });

  it("handles labels with missing name", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "Todo" },
        labels: {
          nodes: [{ name: "worker-done" }, {} as { name: string }, { name: "urgent" }],
        },
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].labels).toEqual(["worker-done", "urgent"]);
  });

  it("handles attachments with invalid URLs", () => {
    const issues = [
      {
        identifier: "ENG-21",
        state: { name: "Needs Review" },
        labels: { nodes: [] },
        attachments: [
          { url: "https://example.com/not-a-pr" },
          { url: "https://github.com/owner/repo/issues/123" },
          { url: "not-even-a-url" },
        ],
      },
    ];
    const result = tracker.parseIssues(issues);
    expect(result).toHaveLength(1);
    expect(result[0].prRef).toBeNull();
  });
});
