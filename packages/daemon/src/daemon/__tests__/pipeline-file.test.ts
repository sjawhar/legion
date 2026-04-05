import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { writeFile as fsWriteFile, mkdtemp, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { CollectedState, IssueState } from "../../state/types";
import {
  mergePipelineFromCollectedState,
  type PipelineIssueEntry,
  type PipelineState,
  patchIssueEntry,
  readPipelineFile,
  writePipelineFile,
} from "../pipeline-file";

function makeIssueState(overrides: Partial<IssueState> = {}): IssueState {
  return {
    status: "In Progress",
    labels: [],
    hasPr: true,
    prIsDraft: false,
    ciStatus: "passing",
    mergeableStatus: null,
    hasLiveWorker: true,
    workerMode: "implement",
    workerStatus: "running",
    suggestedAction: "skip",
    sessionId: "ses_test",
    hasUserFeedback: false,
    isBlocked: false,
    source: null,
    ...overrides,
  };
}

function makeEntry(overrides: Partial<PipelineIssueEntry> = {}): PipelineIssueEntry {
  return {
    status: "In Progress",
    workerSessionId: "ses_abc",
    workerMode: "implement",
    suggestedAction: "skip",
    hasPr: true,
    prIsDraft: false,
    ciStatus: "passing",
    lastAction: "dispatch_implementer",
    lastActionAt: "2026-01-01T00:00:00.000Z",
    lastProgressAt: "2026-01-01T00:00:00.000Z",
    blockedReason: null,
    blockedSince: null,
    blockedBy: [],
    staleAfterMinutes: 60,
    prUrl: null,
    enteredPipelineAt: "2025-12-01T00:00:00.000Z",
    phaseHistory: [],
    ...overrides,
  };
}

describe("pipeline-file", () => {
  let tempDir: string;
  let filePath: string;

  beforeEach(async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-pipeline-"));
    filePath = path.join(tempDir, "controller-pipeline.json");
  });

  afterEach(async () => {
    await rm(tempDir, { recursive: true, force: true });
  });

  describe("readPipelineFile", () => {
    it("returns empty state when file does not exist", async () => {
      const state = await readPipelineFile(filePath);
      expect(state.schemaVersion).toBe(1);
      expect(state.issues).toEqual({});
      expect(typeof state.updatedAt).toBe("string");
    });

    it("returns empty state for empty file", async () => {
      await fsWriteFile(filePath, "");
      const state = await readPipelineFile(filePath);
      expect(state.schemaVersion).toBe(1);
      expect(state.issues).toEqual({});
    });

    it("recovers from corrupt JSON", async () => {
      await fsWriteFile(filePath, "{invalid json!!!");
      const state = await readPipelineFile(filePath);
      expect(state.schemaVersion).toBe(1);
      expect(state.issues).toEqual({});
      const files = await readdir(tempDir);
      expect(files.some((f) => f.includes(".corrupt."))).toBe(true);
    });

    it("recovers from wrong schema version", async () => {
      await fsWriteFile(
        filePath,
        JSON.stringify({ schemaVersion: 99, updatedAt: "x", issues: {} })
      );
      const state = await readPipelineFile(filePath);
      expect(state.schemaVersion).toBe(1);
      expect(state.issues).toEqual({});
      const files = await readdir(tempDir);
      expect(files.some((f) => f.includes(".corrupt."))).toBe(true);
    });

    it("recovers from schema validation failure", async () => {
      await fsWriteFile(
        filePath,
        JSON.stringify({ schemaVersion: 1, updatedAt: "x", issues: { bad: "not_an_object" } })
      );
      const state = await readPipelineFile(filePath);
      expect(state.schemaVersion).toBe(1);
      expect(state.issues).toEqual({});
    });
  });

  describe("writePipelineFile + readPipelineFile", () => {
    it("roundtrips pipeline state", async () => {
      const state: PipelineState = {
        schemaVersion: 1,
        updatedAt: "2026-01-01T00:00:00.000Z",
        issues: {
          "test-issue-1": makeEntry({
            phaseHistory: [
              {
                phase: "implement",
                startedAt: "2026-01-01T00:00:00.000Z",
                completedAt: null,
                workerSessionId: "ses_abc",
                outcome: "in_progress",
              },
            ],
          }),
        },
      };

      await writePipelineFile(filePath, state);
      const loaded = await readPipelineFile(filePath);

      expect(loaded.schemaVersion).toBe(1);
      expect(loaded.issues["test-issue-1"].status).toBe("In Progress");
      expect(loaded.issues["test-issue-1"].workerSessionId).toBe("ses_abc");
      expect(loaded.issues["test-issue-1"].phaseHistory).toHaveLength(1);
    });

    it("writes atomically without leaving temp files", async () => {
      const state: PipelineState = {
        schemaVersion: 1,
        updatedAt: "2026-01-01T00:00:00.000Z",
        issues: {},
      };
      await writePipelineFile(filePath, state);
      const entries = await readdir(tempDir);
      expect(entries).toEqual(["controller-pipeline.json"]);
    });
  });

  describe("mergePipelineFromCollectedState", () => {
    it("creates new entries for issues not in pipeline", async () => {
      const collected: CollectedState = {
        issues: { "issue-1": makeIssueState() },
      };
      await mergePipelineFromCollectedState(filePath, collected);
      const state = await readPipelineFile(filePath);

      expect(state.issues["issue-1"]).toBeDefined();
      expect(state.issues["issue-1"].status).toBe("In Progress");
      expect(state.issues["issue-1"].workerSessionId).toBe("ses_test");
      expect(state.issues["issue-1"].lastAction).toBeNull();
      expect(state.issues["issue-1"].staleAfterMinutes).toBe(60);
    });

    it("updates daemon-owned fields and preserves controller-owned fields", async () => {
      const initialState: PipelineState = {
        schemaVersion: 1,
        updatedAt: "2026-01-01T00:00:00.000Z",
        issues: {
          "issue-1": makeEntry({
            status: "Todo",
            workerSessionId: null,
            workerMode: null,
            suggestedAction: "dispatch_implementer",
            hasPr: false,
            prIsDraft: null,
            ciStatus: null,
            lastAction: "dispatch_planner",
            staleAfterMinutes: 90,
            enteredPipelineAt: "2025-12-01T00:00:00.000Z",
            phaseHistory: [
              {
                phase: "plan",
                startedAt: "2025-12-01T00:00:00.000Z",
                completedAt: "2026-01-01T00:00:00.000Z",
                workerSessionId: "ses_old",
                outcome: "completed",
              },
            ],
          }),
        },
      };
      await writePipelineFile(filePath, initialState);

      const collected: CollectedState = {
        issues: {
          "issue-1": makeIssueState({
            status: "In Progress",
            workerMode: "implement",
            suggestedAction: "skip",
            hasPr: true,
            ciStatus: "passing",
          }),
        },
      };
      await mergePipelineFromCollectedState(filePath, collected);
      const state = await readPipelineFile(filePath);
      const entry = state.issues["issue-1"];

      // Daemon-owned: updated
      expect(entry.status).toBe("In Progress");
      expect(entry.workerMode).toBe("implement");
      expect(entry.hasPr).toBe(true);
      expect(entry.ciStatus).toBe("passing");

      // Controller-owned: preserved
      expect(entry.lastAction).toBe("dispatch_planner");
      expect(entry.staleAfterMinutes).toBe(90);
      expect(entry.enteredPipelineAt).toBe("2025-12-01T00:00:00.000Z");
      expect(entry.phaseHistory).toHaveLength(1);
    });

    it("preserves pipeline issues not in CollectedState", async () => {
      const initialState: PipelineState = {
        schemaVersion: 1,
        updatedAt: "2026-01-01T00:00:00.000Z",
        issues: { "old-issue": makeEntry({ status: "Done" }) },
      };
      await writePipelineFile(filePath, initialState);

      const collected: CollectedState = {
        issues: { "new-issue": makeIssueState() },
      };
      await mergePipelineFromCollectedState(filePath, collected);
      const state = await readPipelineFile(filePath);

      expect(state.issues["old-issue"]).toBeDefined();
      expect(state.issues["new-issue"]).toBeDefined();
    });

    it("normalizes issue IDs to lowercase", async () => {
      const collected: CollectedState = {
        issues: { "ENG-51": makeIssueState() },
      };
      await mergePipelineFromCollectedState(filePath, collected);
      const state = await readPipelineFile(filePath);

      expect(state.issues["eng-51"]).toBeDefined();
      expect(state.issues["ENG-51"]).toBeUndefined();
    });
  });

  describe("patchIssueEntry", () => {
    it("creates new entry with defaults when existing is undefined", () => {
      const result = patchIssueEntry(undefined, { lastAction: "dispatch_implementer" });
      expect(result.lastAction).toBe("dispatch_implementer");
      expect(result.status).toBe("unknown");
      expect(result.staleAfterMinutes).toBe(60);
      expect(result.phaseHistory).toEqual([]);
      expect(result.enteredPipelineAt).toBeDefined();
    });

    it("shallow-merges into existing entry preserving unpatched fields", () => {
      const existing = makeEntry();
      const result = patchIssueEntry(existing, {
        lastAction: "transition_to_testing",
        lastActionAt: "2026-01-02T00:00:00.000Z",
      });
      expect(result.lastAction).toBe("transition_to_testing");
      expect(result.lastActionAt).toBe("2026-01-02T00:00:00.000Z");
      expect(result.status).toBe("In Progress");
      expect(result.workerSessionId).toBe("ses_abc");
      expect(result.enteredPipelineAt).toBe("2025-12-01T00:00:00.000Z");
    });

    it("caps phaseHistory at 20 entries (oldest dropped)", () => {
      const existing = makeEntry();
      const bigHistory = Array.from({ length: 25 }, (_, i) => ({
        phase: "implement",
        startedAt: `2026-01-${String(i + 1).padStart(2, "0")}T00:00:00.000Z`,
        completedAt: null,
        workerSessionId: null,
        outcome: "completed" as const,
      }));
      const result = patchIssueEntry(existing, { phaseHistory: bigHistory });
      expect(result.phaseHistory).toHaveLength(20);
      expect(result.phaseHistory[0].startedAt).toBe("2026-01-06T00:00:00.000Z");
    });
  });
});
