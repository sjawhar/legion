import { describe, expect, it } from "bun:test";

import {
  KNOWLEDGE_SCHEMA_VERSION,
  LearningFeedbackPhaseSchema,
  LearningFeedbackRecordSchema,
} from "../types";

describe("LearningFeedbackPhaseSchema", () => {
  it("defaults injected and helpful arrays", () => {
    const result = LearningFeedbackPhaseSchema.parse({});

    expect(result).toEqual({
      helpful: [],
      injected: [],
    });
  });
});

describe("LearningFeedbackRecordSchema", () => {
  it("accepts partial phase maps with ISO timestamps", () => {
    const result = LearningFeedbackRecordSchema.parse({
      issueId: "ENG-240",
      phases: {
        plan: {
          injected: ["docs/solutions/knowledge/plan.md"],
        },
        test: {
          helpful: ["docs/solutions/knowledge/test.md"],
        },
      },
      schemaVersion: KNOWLEDGE_SCHEMA_VERSION,
      timestamp: "2026-04-11T12:00:00.000Z",
    });

    expect(result.phases.plan).toEqual({
      helpful: [],
      injected: ["docs/solutions/knowledge/plan.md"],
    });
    expect(result.phases.test).toEqual({
      helpful: ["docs/solutions/knowledge/test.md"],
      injected: [],
    });
    expect(result.phases.review).toBeUndefined();
  });

  it("rejects invalid timestamps", () => {
    const result = LearningFeedbackRecordSchema.safeParse({
      issueId: "ENG-240",
      phases: {
        implement: {
          helpful: ["docs/solutions/knowledge/implement.md"],
        },
      },
      schemaVersion: KNOWLEDGE_SCHEMA_VERSION,
      timestamp: "yesterday",
    });

    expect(result.success).toBe(false);
  });
});
