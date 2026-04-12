import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { writePhaseHandoff } from "../../handoff/ledger";
import type { HandoffPhase, PhaseHandoff } from "../../handoff/types";
import {
  buildLearningFeedbackRecordFromHandoffs,
  captureLearningFeedbackFromWorkspace,
  deriveLegionIdFromWorkspaceDir,
} from "../feedback-logger";
import { LearningFeedbackRecordSchema } from "../types";

describe("deriveLegionIdFromWorkspaceDir", () => {
  it("extracts owner and number from configured workspaces dir", async () => {
    const homeDir = await mkdtemp(path.join(os.tmpdir(), "knowledge-home-"));
    const env = {
      XDG_DATA_HOME: path.join(homeDir, "data"),
      XDG_STATE_HOME: path.join(homeDir, "state"),
    } satisfies Record<string, string>;
    const workspaceDir = path.join(
      env.XDG_DATA_HOME,
      "legion",
      "workspaces",
      "sjawhar",
      "240",
      "worker"
    );

    await mkdir(workspaceDir, { recursive: true });

    expect(deriveLegionIdFromWorkspaceDir(workspaceDir, env, homeDir)).toBe("sjawhar/240");

    await rm(homeDir, { force: true, recursive: true });
  });
});

describe("buildLearningFeedbackRecordFromHandoffs", () => {
  it("extracts only populated learning arrays", () => {
    const handoffs = {
      architect: {
        completed: "2026-04-11T12:00:00.000Z",
        phase: "architect",
        schemaVersion: 1,
      },
      plan: {
        completed: "2026-04-11T12:01:00.000Z",
        learningsHelpful: ["docs/solutions/knowledge/plan.md"],
        learningsInjected: ["docs/solutions/knowledge/shared.md"],
        phase: "plan",
        schemaVersion: 1,
      },
      review: {
        completed: "2026-04-11T12:02:00.000Z",
        learningsHelpful: [],
        learningsInjected: [],
        phase: "review",
        schemaVersion: 1,
      },
    } satisfies Partial<Record<HandoffPhase, PhaseHandoff>>;

    const result = buildLearningFeedbackRecordFromHandoffs(
      "ENG-240",
      handoffs,
      "2026-04-11T13:00:00.000Z"
    );

    expect(result).toEqual({
      issueId: "ENG-240",
      phases: {
        plan: {
          helpful: ["docs/solutions/knowledge/plan.md"],
          injected: ["docs/solutions/knowledge/shared.md"],
        },
      },
      schemaVersion: 1,
      timestamp: "2026-04-11T13:00:00.000Z",
    });
  });
});

describe("captureLearningFeedbackFromWorkspace", () => {
  let homeDir: string | null = null;

  afterEach(async () => {
    if (homeDir) {
      await rm(homeDir, { force: true, recursive: true });
      homeDir = null;
    }
  });

  it("appends a JSONL line", async () => {
    homeDir = await mkdtemp(path.join(os.tmpdir(), "knowledge-home-"));
    const env = {
      XDG_DATA_HOME: path.join(homeDir, "data"),
      XDG_STATE_HOME: path.join(homeDir, "state"),
    } satisfies Record<string, string>;
    const workspaceDir = path.join(
      env.XDG_DATA_HOME,
      "legion",
      "workspaces",
      "sjawhar",
      "240",
      "worker"
    );

    await mkdir(workspaceDir, { recursive: true });
    writePhaseHandoff(workspaceDir, "plan", {
      learningsHelpful: ["docs/solutions/knowledge/plan.md"],
      learningsInjected: ["docs/solutions/knowledge/shared.md"],
    });

    const result = await captureLearningFeedbackFromWorkspace({
      env,
      homeDir,
      issueId: "ENG-240",
      timestamp: "2026-04-11T13:00:00.000Z",
      workspaceDir,
    });

    expect(result.written).toBe(true);
    expect(result.filePath).toBeDefined();

    const fileContents = await readFile(result.filePath as string, "utf-8");
    const parsed = LearningFeedbackRecordSchema.parse(JSON.parse(fileContents.trim()));
    expect(parsed).toEqual({
      issueId: "ENG-240",
      phases: {
        plan: {
          helpful: ["docs/solutions/knowledge/plan.md"],
          injected: ["docs/solutions/knowledge/shared.md"],
        },
      },
      schemaVersion: 1,
      timestamp: "2026-04-11T13:00:00.000Z",
    });
  });

  it("skips when no handoff carries learning feedback", async () => {
    homeDir = await mkdtemp(path.join(os.tmpdir(), "knowledge-home-"));
    const env = {
      XDG_DATA_HOME: path.join(homeDir, "data"),
      XDG_STATE_HOME: path.join(homeDir, "state"),
    } satisfies Record<string, string>;
    const workspaceDir = path.join(
      env.XDG_DATA_HOME,
      "legion",
      "workspaces",
      "sjawhar",
      "240",
      "worker"
    );

    await mkdir(workspaceDir, { recursive: true });
    writePhaseHandoff(workspaceDir, "implement", {
      filesChanged: ["packages/daemon/src/knowledge/types.ts"],
      trickyParts: ["none"],
    });

    const result = await captureLearningFeedbackFromWorkspace({
      env,
      homeDir,
      issueId: "ENG-240",
      timestamp: "2026-04-11T13:00:00.000Z",
      workspaceDir,
    });

    expect(result).toEqual({
      reason: "no_learning_feedback",
      written: false,
    });
  });
});
