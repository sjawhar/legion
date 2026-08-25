import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { consolidateKnowledge } from "../consolidate";
import { readAssembledIndex } from "../promoter";
import { type CollectedIssueFeedback, LearningFeedbackRecordSchema } from "../types";

const tempDirs: string[] = [];

async function makeTempDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "knowledge-consolidate-"));
  tempDirs.push(dir);
  return dir;
}

function makeIssue(
  issueId: string,
  learningPath: string,
  options: {
    helpful?: boolean;
    touchedPaths?: string[];
    timestamp?: string;
  } = {}
): CollectedIssueFeedback {
  const helpful = options.helpful ?? false;

  return {
    issueId,
    records: [
      LearningFeedbackRecordSchema.parse({
        issueId,
        phases: {
          review: {
            helpful: helpful ? [learningPath] : [],
            injected: [learningPath],
          },
        },
        schemaVersion: 1,
        timestamp: options.timestamp ?? "2026-04-11T12:00:00.000Z",
      }),
    ],
    touchedPaths: options.touchedPaths ?? [],
  };
}

async function writeLearningFile(
  repoRoot: string,
  learningPath: string,
  status = "active"
): Promise<string> {
  const fullPath = path.join(repoRoot, "docs", "solutions", learningPath);
  await mkdir(path.dirname(fullPath), { recursive: true });
  await writeFile(
    fullPath,
    [
      "---",
      `title: ${learningPath}`,
      "date: 2026-04-11",
      `status: ${status}`,
      "---",
      "",
      "# Learning",
    ].join("\n")
  );
  return fullPath;
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

describe("consolidateKnowledge", () => {
  it("returns categorized dry-run stats for promotable learnings", async () => {
    const workspaceRoot = await makeTempDir();
    const report = await consolidateKnowledge({
      legionId: "trajectory-labs/240",
      workspaceRoot,
      apply: false,
      preCollectedIssues: [
        makeIssue("240", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
          timestamp: "2026-04-11T12:00:00.000Z",
        }),
        makeIssue("241", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
          timestamp: "2026-04-12T12:00:00.000Z",
        }),
        makeIssue("242", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
          timestamp: "2026-04-13T12:00:00.000Z",
        }),
      ],
      env: {},
      homeDir: workspaceRoot,
      now: new Date("2026-04-14T00:00:00.000Z"),
    });

    expect(report.apply).toBe(false);
    expect(report.issueCount).toBe(3);
    expect(report.recordCount).toBe(3);
    expect(report.indexMutations).toEqual([]);
    expect(report.statusMutations).toEqual([]);
    expect(report.warnings).toEqual([]);
    expect(report.learnings).toEqual([
      expect.objectContaining({
        disposition: "accepted",
        path: "knowledge/promoted.md",
      }),
    ]);
  });

  it("applies status mutations when apply is enabled for review learnings", async () => {
    const repoRoot = await makeTempDir();
    const learningPath = await writeLearningFile(repoRoot, "knowledge/review.md");

    const report = await consolidateKnowledge({
      legionId: "trajectory-labs/240",
      workspaceRoot: repoRoot,
      repoRoot,
      apply: true,
      preCollectedIssues: [
        makeIssue("240", "knowledge/review.md"),
        makeIssue("241", "knowledge/review.md"),
        makeIssue("242", "knowledge/review.md"),
      ],
      env: {},
      homeDir: repoRoot,
      now: new Date("2026-04-14T00:00:00.000Z"),
    });

    expect(report.statusMutations).toEqual([
      {
        action: "set",
        learningPath: "knowledge/review.md",
        reason: "Review this learning because it is frequently injected but rarely helpful.",
        status: "needs-review",
      },
    ]);
    expect(report.indexMutations).toEqual([]);

    const contents = await readFile(learningPath, "utf-8");
    expect(contents).toContain("status: needs-review");
  });

  it("stays idempotent across repeated apply runs when the learning is already indexed", async () => {
    const repoRoot = await makeTempDir();
    await writeLearningFile(repoRoot, "knowledge/promoted.md");
    const indexDir = path.join(repoRoot, "docs", "solutions", ".index");
    await mkdir(indexDir, { recursive: true });
    await writeFile(
      path.join(indexDir, "existing.json"),
      JSON.stringify(
        {
          entries: {
            "packages/daemon/src/state": ["knowledge/promoted.md"],
          },
          version: 1,
        },
        null,
        2
      )
    );

    const preCollectedIssues = [
      makeIssue("240", "knowledge/promoted.md", {
        helpful: true,
        touchedPaths: ["packages/daemon/src/state/decision.ts"],
      }),
      makeIssue("241", "knowledge/promoted.md", {
        helpful: true,
        touchedPaths: ["packages/daemon/src/state/decision.ts"],
      }),
      makeIssue("242", "knowledge/promoted.md", {
        helpful: true,
        touchedPaths: ["packages/daemon/src/state/decision.ts"],
      }),
    ];

    const firstReport = await consolidateKnowledge({
      legionId: "trajectory-labs/240",
      workspaceRoot: repoRoot,
      repoRoot,
      apply: true,
      preCollectedIssues,
      env: {},
      homeDir: repoRoot,
      now: new Date("2026-04-14T00:00:00.000Z"),
    });
    const secondReport = await consolidateKnowledge({
      legionId: "trajectory-labs/240",
      workspaceRoot: repoRoot,
      repoRoot,
      apply: true,
      preCollectedIssues,
      env: {},
      homeDir: repoRoot,
      now: new Date("2026-04-14T00:00:00.000Z"),
    });

    expect(firstReport.indexMutations).toEqual([]);
    expect(secondReport.indexMutations).toEqual([]);

    const assembled = await readAssembledIndex(indexDir);
    expect(assembled.index["packages/daemon/src/state"]).toEqual(["knowledge/promoted.md"]);
  });

  it("skips malformed entry files without crashing", async () => {
    const repoRoot = await makeTempDir();
    await writeLearningFile(repoRoot, "knowledge/promoted.md");
    const indexDir = path.join(repoRoot, "docs", "solutions", ".index");
    await mkdir(indexDir, { recursive: true });
    await writeFile(path.join(indexDir, "bad.json"), '{"version":"oops"}');

    const report = await consolidateKnowledge({
      legionId: "trajectory-labs/240",
      workspaceRoot: repoRoot,
      repoRoot,
      apply: true,
      preCollectedIssues: [
        makeIssue("240", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        }),
        makeIssue("241", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        }),
        makeIssue("242", "knowledge/promoted.md", {
          helpful: true,
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        }),
      ],
      env: {},
      homeDir: repoRoot,
      now: new Date("2026-04-14T00:00:00.000Z"),
    });

    // Malformed entry files are skipped silently — promotions still succeed
    expect(report.indexMutations).toHaveLength(1);
    expect(report.indexMutations[0]).toEqual({
      action: "upsert",
      key: "packages/daemon/src/state",
      learningPath: "knowledge/promoted.md",
    });
    expect(report.warnings).toEqual([]);
  });
});
