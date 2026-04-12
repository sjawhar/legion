import { describe, expect, it } from "bun:test";

import type { ConsolidationReportLike } from "../reporter";
import { formatConsolidationReportHuman, formatConsolidationReportJson } from "../reporter";

const report: ConsolidationReportLike = {
  apply: true,
  indexMutations: [
    {
      action: "upsert",
      key: "packages/daemon/src/state",
      learningPath: "knowledge/promoted.md",
    },
  ],
  learnings: [
    {
      disposition: "accepted",
      notes: "Frequently helpful across issues.",
      path: "knowledge/promoted.md",
    },
  ],
  legionId: "trajectory-labs/240",
  logPath: "/tmp/learning-feedback.jsonl",
  statusMutations: [],
  warnings: ["index warning"],
  workspaceRoot: "/tmp/workspace",
};

describe("formatConsolidationReportHuman", () => {
  it("formats grouped human output", () => {
    const formatted = formatConsolidationReportHuman(report);

    expect(formatted).toContain("PROMOTE (1)");
    expect(formatted).toContain("knowledge/promoted.md");
  });
});

describe("formatConsolidationReportJson", () => {
  it("formats structured JSON", () => {
    const formatted = formatConsolidationReportJson(report);
    const parsed = JSON.parse(formatted) as { legionId: string; warnings: string[] };

    expect(parsed.legionId).toBe("trajectory-labs/240");
    expect(parsed.warnings).toEqual(["index warning"]);
  });
});
