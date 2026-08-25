import { describe, expect, it } from "bun:test";

import { aggregateLearningFeedback } from "../aggregator";
import { LearningFeedbackRecordSchema } from "../types";

describe("aggregateLearningFeedback", () => {
  it("counts distinct issue IDs once even when injected in multiple phases", () => {
    const issues = [
      {
        issueId: "240",
        records: [
          LearningFeedbackRecordSchema.parse({
            issueId: "240",
            phases: {
              plan: {
                injected: ["knowledge/shared.md"],
              },
              test: {
                injected: ["knowledge/shared.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          }),
        ],
        touchedPaths: [],
      },
    ];

    expect(aggregateLearningFeedback(issues)).toEqual([
      expect.objectContaining({
        helpfulRatio: 0,
        issuesHelpful: 0,
        issuesInjected: 1,
        path: "knowledge/shared.md",
      }),
    ]);
  });

  it("ignores architect helpful marks for helpful counts", () => {
    const issues = [
      {
        issueId: "240",
        records: [
          LearningFeedbackRecordSchema.parse({
            issueId: "240",
            phases: {
              architect: {
                helpful: ["knowledge/plan.md"],
                injected: ["knowledge/plan.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          }),
        ],
        touchedPaths: ["packages/daemon/src/knowledge/collector.ts"],
      },
    ];

    expect(aggregateLearningFeedback(issues)).toEqual([
      expect.objectContaining({
        helpfulRatio: 0,
        issues: [],
        issuesHelpful: 0,
        issuesInjected: 1,
        path: "knowledge/plan.md",
      }),
    ]);
  });

  it("carries filesChanged for helpful issues into promotion context", () => {
    const issues = [
      {
        issueId: "240",
        records: [
          LearningFeedbackRecordSchema.parse({
            issueId: "240",
            phases: {
              review: {
                helpful: ["knowledge/review.md"],
                injected: ["knowledge/review.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          }),
        ],
        touchedPaths: [
          "packages/daemon/src/knowledge/aggregator.ts",
          "packages/daemon/src/knowledge/rules.ts",
        ],
      },
    ];

    const aggregate = aggregateLearningFeedback(issues)[0];

    expect(aggregate.issues[0]).toEqual(
      expect.objectContaining({
        issueId: "240",
        touchedPaths: [
          "packages/daemon/src/knowledge/aggregator.ts",
          "packages/daemon/src/knowledge/rules.ts",
        ],
      })
    );
  });

  it("tracks the latest injection timestamp", () => {
    const issues = [
      {
        issueId: "240",
        records: [
          LearningFeedbackRecordSchema.parse({
            issueId: "240",
            phases: {
              plan: {
                injected: ["knowledge/latest.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          }),
        ],
        touchedPaths: [],
      },
      {
        issueId: "241",
        records: [
          LearningFeedbackRecordSchema.parse({
            issueId: "241",
            phases: {
              review: {
                injected: ["knowledge/latest.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-12T12:00:00.000Z",
          }),
        ],
        touchedPaths: [],
      },
    ];

    expect(aggregateLearningFeedback(issues)).toEqual([
      expect.objectContaining({
        lastInjected: "2026-04-12T12:00:00.000Z",
        lastSeenAt: "2026-04-12T12:00:00.000Z",
        path: "knowledge/latest.md",
      }),
    ]);
  });
});
