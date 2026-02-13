import { describe, expect, it } from "bun:test";
import { detectCycle } from "../graph";

describe("detectCycle", () => {
  it("returns null when no cycle exists", () => {
    const tasks = new Map([
      ["T-1", { blocks: ["T-2"], blockedBy: [] }],
      ["T-2", { blocks: [], blockedBy: ["T-1"] }],
    ]);

    const result = detectCycle("T-3", ["T-2"], (id) => tasks.get(id) ?? null);
    expect(result).toBeNull();
  });

  it("detects a direct cycle (A blocks B, B wants to block A)", () => {
    const tasks = new Map([
      ["T-1", { blocks: ["T-2"], blockedBy: [] }],
      ["T-2", { blocks: [], blockedBy: ["T-1"] }],
    ]);

    const result = detectCycle("T-1", ["T-2"], (id) => tasks.get(id) ?? null);
    expect(result).not.toBeNull();
    expect(result).toContain("T-1");
    expect(result).toContain("T-2");
  });

  it("detects a transitive cycle (A→B→C, C wants to depend on A)", () => {
    const tasks = new Map([
      ["T-1", { blocks: [], blockedBy: [] }],
      ["T-2", { blocks: [], blockedBy: ["T-1"] }],
      ["T-3", { blocks: [], blockedBy: ["T-2"] }],
    ]);

    const result = detectCycle("T-1", ["T-3"], (id) => tasks.get(id) ?? null);
    expect(result).not.toBeNull();
    expect(result).toContain("T-1");
    expect(result).toContain("T-3");
  });

  it("returns null for missing tasks in the graph", () => {
    const result = detectCycle("T-1", ["T-nonexistent"], () => null);
    expect(result).toBeNull();
  });

  it("handles self-reference", () => {
    const tasks = new Map([["T-1", { blocks: [], blockedBy: [] }]]);

    const result = detectCycle("T-1", ["T-1"], (id) => tasks.get(id) ?? null);
    expect(result).not.toBeNull();
  });

  it("handles multiple proposed dependencies with one cycle", () => {
    const tasks = new Map([
      ["T-1", { blocks: [], blockedBy: [] }],
      ["T-2", { blocks: [], blockedBy: ["T-1"] }],
    ]);

    const result = detectCycle("T-1", ["T-1", "T-2"], (id) => tasks.get(id) ?? null);
    expect(result).not.toBeNull();
  });

  it("handles deep chains without cycles", () => {
    const tasks = new Map<string, { blocks: string[]; blockedBy: string[] }>();
    for (let i = 1; i <= 10; i++) {
      tasks.set(`T-${i}`, {
        blocks: [],
        blockedBy: i > 1 ? [`T-${i - 1}`] : [],
      });
    }

    const result = detectCycle("T-11", ["T-10"], (id) => tasks.get(id) ?? null);
    expect(result).toBeNull();
  });
});
