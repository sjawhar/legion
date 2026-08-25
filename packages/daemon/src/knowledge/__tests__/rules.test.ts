import { describe, expect, it } from "bun:test";

import { classifyLearningAggregate } from "../rules";

const baseAggregate = {
  firstSeenAt: "2026-01-01T00:00:00.000Z",
  helpfulCount: 0,
  helpfulRatio: 0,
  issues: [],
  issuesHelpful: 0,
  issuesInjected: 0,
  lastInjected: "2026-01-01T00:00:00.000Z",
  lastSeenAt: "2026-01-01T00:00:00.000Z",
  path: "knowledge/example.md",
  touchedCount: 0,
  touchedPaths: [],
};

describe("classifyLearningAggregate", () => {
  it("marks promote when ratio >= 0.70 and helpful >= 3", () => {
    expect(
      classifyLearningAggregate({
        ...baseAggregate,
        helpfulCount: 3,
        helpfulRatio: 0.75,
        issuesHelpful: 3,
        issuesInjected: 4,
      })
    ).toEqual(expect.objectContaining({ disposition: "accepted" }));
  });

  it("marks stale before review when lastInjected is older than 90 days and helpful is 0", () => {
    expect(
      classifyLearningAggregate(
        {
          ...baseAggregate,
          issuesInjected: 5,
          lastInjected: "2025-01-01T00:00:00.000Z",
          lastSeenAt: "2025-01-01T00:00:00.000Z",
        },
        new Date("2025-04-15T00:00:00.000Z")
      )
    ).toEqual(expect.objectContaining({ disposition: "archived" }));
  });

  it("marks review when injected >= 3 and ratio < 0.30", () => {
    expect(
      classifyLearningAggregate({
        ...baseAggregate,
        helpfulRatio: 0.2,
        issuesHelpful: 1,
        issuesInjected: 5,
      })
    ).toEqual(expect.objectContaining({ disposition: "needs_review" }));
  });

  it("keeps rows with insufficient data", () => {
    expect(
      classifyLearningAggregate({
        ...baseAggregate,
        helpfulRatio: 0.5,
        issuesHelpful: 1,
        issuesInjected: 2,
      })
    ).toEqual(expect.objectContaining({ disposition: "rejected" }));
  });
});
