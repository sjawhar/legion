import { describe, expect, it } from "bun:test";
import {
  TaskCreateInputSchema,
  TaskSchema,
  TaskStatusSchema,
  TaskUpdateInputSchema,
} from "../types";

describe("TaskStatusSchema", () => {
  it("accepts valid statuses", () => {
    for (const status of ["pending", "in_progress", "completed", "cancelled"]) {
      expect(TaskStatusSchema.safeParse(status).success).toBe(true);
    }
  });

  it("rejects deleted (OMO legacy status)", () => {
    expect(TaskStatusSchema.safeParse("deleted").success).toBe(false);
  });

  it("rejects unknown statuses", () => {
    expect(TaskStatusSchema.safeParse("unknown").success).toBe(false);
  });
});

describe("TaskSchema", () => {
  const validTask = {
    id: "T-abc123",
    subject: "Add error handling",
    description: "Handle edge cases",
    status: "pending" as const,
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
  };

  it("accepts a valid task", () => {
    const result = TaskSchema.safeParse(validTask);
    expect(result.success).toBe(true);
  });

  it("defaults blocks and blockedBy to empty arrays", () => {
    const { blocks, blockedBy, ...rest } = validTask;
    const result = TaskSchema.parse(rest);
    expect(result.blocks).toEqual([]);
    expect(result.blockedBy).toEqual([]);
  });

  it("accepts optional fields", () => {
    const result = TaskSchema.safeParse({
      ...validTask,
      owner: "agent-1",
      metadata: { priority: "high" },
      parentID: "T-parent",
    });
    expect(result.success).toBe(true);
  });

  it("rejects missing required fields", () => {
    const { id, ...noId } = validTask;
    expect(TaskSchema.safeParse(noId).success).toBe(false);
  });

  it("rejects extra fields in strict mode", () => {
    const result = TaskSchema.safeParse({ ...validTask, extraField: "nope" });
    expect(result.success).toBe(false);
  });
});

describe("TaskCreateInputSchema", () => {
  it("requires subject", () => {
    expect(TaskCreateInputSchema.safeParse({}).success).toBe(false);
    expect(TaskCreateInputSchema.safeParse({ subject: "Do thing" }).success).toBe(true);
  });

  it("accepts optional blockedBy/blocks", () => {
    const result = TaskCreateInputSchema.safeParse({
      subject: "Do thing",
      blockedBy: ["T-1"],
      blocks: ["T-2"],
    });
    expect(result.success).toBe(true);
  });
});

describe("TaskUpdateInputSchema", () => {
  it("requires id", () => {
    expect(TaskUpdateInputSchema.safeParse({}).success).toBe(false);
    expect(TaskUpdateInputSchema.safeParse({ id: "T-1" }).success).toBe(true);
  });

  it("accepts addBlocks and addBlockedBy", () => {
    const result = TaskUpdateInputSchema.safeParse({
      id: "T-1",
      addBlocks: ["T-2"],
      addBlockedBy: ["T-3"],
    });
    expect(result.success).toBe(true);
  });
});
