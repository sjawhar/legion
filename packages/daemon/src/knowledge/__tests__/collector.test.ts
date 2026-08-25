import { afterEach, describe, expect, it } from "bun:test";
import { appendFile, mkdir, mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { resolveLegionPaths } from "../../daemon/paths";
import { writePhaseHandoff } from "../../handoff/ledger";
import {
  type CollectedIssueCandidate,
  canonicalizeLearningPath,
  collectLearningFeedback,
  dedupeCollectedIssues,
} from "../collector";
import { LearningFeedbackRecordSchema } from "../types";

describe("canonicalizeLearningPath", () => {
  it("strips the docs/solutions prefix and normalizes separators", () => {
    expect(canonicalizeLearningPath("docs\\solutions\\knowledge\\collector.md")).toBe(
      "knowledge/collector.md"
    );
    expect(canonicalizeLearningPath("docs/solutions/knowledge/rules.md")).toBe(
      "knowledge/rules.md"
    );
  });

  it("leaves already canonical relative paths unchanged", () => {
    expect(canonicalizeLearningPath("knowledge/collector.md")).toBe("knowledge/collector.md");
  });
});

describe("dedupeCollectedIssues", () => {
  it("prefers log records while retaining workspace filesChanged", () => {
    const logRecord = LearningFeedbackRecordSchema.parse({
      issueId: "240",
      phases: {
        plan: {
          injected: ["docs/solutions/knowledge/log.md"],
        },
      },
      schemaVersion: 1,
      timestamp: "2026-04-11T12:00:00.000Z",
    });

    const issues: CollectedIssueCandidate[] = [
      {
        issueId: "240",
        records: [logRecord],
        source: "log",
        touchedPaths: ["packages/daemon/src/knowledge/log.ts"],
      },
      {
        issueId: "240",
        records: [],
        source: "workspace",
        touchedPaths: ["packages/daemon/src/knowledge/collector.ts"],
      },
    ];

    expect(dedupeCollectedIssues(issues)).toEqual([
      {
        issueId: "240",
        records: [
          {
            issueId: "240",
            phases: {
              plan: {
                helpful: [],
                injected: ["knowledge/log.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          },
        ],
        touchedPaths: [
          "packages/daemon/src/knowledge/collector.ts",
          "packages/daemon/src/knowledge/log.ts",
        ],
      },
    ]);
  });
});

describe("collectLearningFeedback", () => {
  let homeDir: string | null = null;

  afterEach(async () => {
    if (homeDir) {
      await rm(homeDir, { force: true, recursive: true });
      homeDir = null;
    }
  });

  it("reads JSONL and workspace handoffs while skipping malformed lines", async () => {
    homeDir = await mkdtemp(path.join(os.tmpdir(), "knowledge-collector-"));
    const env = {
      XDG_DATA_HOME: path.join(homeDir, "data"),
      XDG_STATE_HOME: path.join(homeDir, "state"),
    } satisfies Record<string, string>;
    const legionId = "sjawhar/240";
    const legionPaths = resolveLegionPaths(env, homeDir).forLegion(legionId);

    await mkdir(legionPaths.legionStateDir, { recursive: true });
    await appendFile(
      path.join(legionPaths.legionStateDir, "learning-feedback.jsonl"),
      `${JSON.stringify(
        LearningFeedbackRecordSchema.parse({
          issueId: "241",
          phases: {
            plan: {
              helpful: ["docs/solutions/knowledge/log-helpful.md"],
              injected: ["docs/solutions/knowledge/log-helpful.md"],
            },
          },
          schemaVersion: 1,
          timestamp: "2026-04-11T12:00:00.000Z",
        })
      )}\nthis is not json\n`,
      "utf-8"
    );

    const workspaceDir = path.join(legionPaths.workspacesDir, "sjawhar-legion-240");
    await mkdir(workspaceDir, { recursive: true });
    writePhaseHandoff(workspaceDir, "implement", {
      filesChanged: ["packages/daemon/src/knowledge/collector.ts"],
      learningsHelpful: ["docs/solutions/knowledge/workspace-helpful.md"],
      learningsInjected: ["docs/solutions/knowledge/workspace-helpful.md"],
    });

    const result = await collectLearningFeedback({
      env,
      homeDir,
      legionId,
    });

    expect(result.warnings).toEqual(["[knowledge] Malformed JSONL at line 2: skipped"]);
    expect(result.issues).toEqual([
      {
        issueId: "241",
        records: [
          {
            issueId: "241",
            phases: {
              plan: {
                helpful: ["knowledge/log-helpful.md"],
                injected: ["knowledge/log-helpful.md"],
              },
            },
            schemaVersion: 1,
            timestamp: "2026-04-11T12:00:00.000Z",
          },
        ],
        touchedPaths: [],
      },
      {
        issueId: "sjawhar-legion-240",
        records: [
          {
            issueId: "sjawhar-legion-240",
            phases: {
              implement: {
                helpful: ["knowledge/workspace-helpful.md"],
                injected: ["knowledge/workspace-helpful.md"],
              },
            },
            schemaVersion: 1,
            timestamp: expect.stringMatching(/^\d{4}-\d{2}-\d{2}T/),
          },
        ],
        touchedPaths: ["packages/daemon/src/knowledge/collector.ts"],
      },
    ]);
  });
});
