import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { z } from "zod";
import type { CollectedState, IssueState } from "../state/types";

// =============================================================================
// Zod Schemas
// =============================================================================

export const PhaseHistoryEntrySchema = z.object({
  phase: z.string(),
  startedAt: z.string(),
  completedAt: z.string().nullable(),
  workerSessionId: z.string().nullable(),
  outcome: z.enum(["completed", "failed", "blocked", "in_progress"]),
});

export const PipelineIssueEntrySchema = z.object({
  // Daemon-owned (auto-merged from CollectedState)
  status: z.string(),
  workerSessionId: z.string().nullable(),
  workerMode: z.string().nullable(),
  suggestedAction: z.string(),
  hasPr: z.boolean(),
  prIsDraft: z.boolean().nullable(),
  ciStatus: z.string().nullable(),

  // Controller-owned
  lastAction: z.string().nullable(),
  lastActionAt: z.string().nullable(),
  lastProgressAt: z.string(),
  blockedReason: z.string().nullable(),
  blockedSince: z.string().nullable(),
  blockedBy: z.array(z.string()),
  staleAfterMinutes: z.number(),
  prUrl: z.string().nullable(),
  enteredPipelineAt: z.string(),
  phaseHistory: z.array(PhaseHistoryEntrySchema),
});

export const PipelineStateSchema = z
  .object({
    schemaVersion: z.literal(1),
    updatedAt: z.string(),
    issues: z.record(z.string(), PipelineIssueEntrySchema),
  })
  .passthrough();

// =============================================================================
// Types
// =============================================================================

export type PhaseHistoryEntry = z.infer<typeof PhaseHistoryEntrySchema>;
export type PipelineIssueEntry = z.infer<typeof PipelineIssueEntrySchema>;
export type PipelineState = z.infer<typeof PipelineStateSchema>;

// =============================================================================
// Constants
// =============================================================================

const MAX_PHASE_HISTORY = 20;

// =============================================================================
// File I/O
// =============================================================================

function emptyPipelineState(): PipelineState {
  return {
    schemaVersion: 1,
    updatedAt: new Date().toISOString(),
    issues: {},
  };
}

async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (err) {
    console.warn(`[pipeline-file] Failed to rename corrupt file ${filePath}:`, err);
  }
}

export async function readPipelineFile(filePath: string): Promise<PipelineState> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return emptyPipelineState();
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[pipeline-file] Corrupt JSON in ${filePath}, recovering to empty state`);
      await moveCorruptFile(filePath);
      return emptyPipelineState();
    }

    // Schema version check — treat mismatch as corrupt
    if (
      typeof parsed === "object" &&
      parsed !== null &&
      "schemaVersion" in parsed &&
      (parsed as Record<string, unknown>).schemaVersion !== 1
    ) {
      console.warn(
        `[pipeline-file] Unknown schema version in ${filePath}, recovering to empty state`
      );
      await moveCorruptFile(filePath);
      return emptyPipelineState();
    }

    const validation = PipelineStateSchema.safeParse(parsed);
    if (!validation.success) {
      const issues = validation.error.issues.map((i) => i.message).join(", ");
      console.warn(`[pipeline-file] Schema validation failed for ${filePath}: ${issues}`);
      await moveCorruptFile(filePath);
      return emptyPipelineState();
    }

    return validation.data;
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return emptyPipelineState();
    }
    throw error;
  }
}

export async function writePipelineFile(filePath: string, state: PipelineState): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });

  const toWrite: PipelineState = { ...state, updatedAt: new Date().toISOString() };
  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(toWrite, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, filePath);
}

// =============================================================================
// Merge Logic
// =============================================================================

function makeDefaultEntry(issueState: IssueState, now: string): PipelineIssueEntry {
  return {
    status: issueState.status,
    workerSessionId: issueState.sessionId || null,
    workerMode: issueState.workerMode,
    suggestedAction: issueState.suggestedAction,
    hasPr: issueState.hasPr,
    prIsDraft: issueState.prIsDraft,
    ciStatus: issueState.ciStatus,
    lastAction: null,
    lastActionAt: null,
    lastProgressAt: now,
    blockedReason: null,
    blockedSince: null,
    blockedBy: [],
    staleAfterMinutes: 60,
    prUrl: null,
    enteredPipelineAt: now,
    phaseHistory: [],
  };
}

function mergeDaemonFields(
  existing: PipelineIssueEntry,
  issueState: IssueState
): PipelineIssueEntry {
  return {
    ...existing,
    status: issueState.status,
    workerSessionId: issueState.sessionId || null,
    workerMode: issueState.workerMode,
    suggestedAction: issueState.suggestedAction,
    hasPr: issueState.hasPr,
    prIsDraft: issueState.prIsDraft,
    ciStatus: issueState.ciStatus,
  };
}

/**
 * Merge CollectedState into the pipeline file.
 * - Existing issues: daemon-owned fields updated, controller-owned fields preserved.
 * - New issues: created with defaults.
 * - Issues in pipeline but NOT in CollectedState: left unchanged.
 */
export async function mergePipelineFromCollectedState(
  pipelineFilePath: string,
  collectedState: CollectedState
): Promise<void> {
  const pipeline = await readPipelineFile(pipelineFilePath);
  const now = new Date().toISOString();

  for (const [rawIssueId, issueState] of Object.entries(collectedState.issues)) {
    const issueId = rawIssueId.toLowerCase();
    const existing = pipeline.issues[issueId];
    if (existing) {
      pipeline.issues[issueId] = mergeDaemonFields(existing, issueState);
    } else {
      pipeline.issues[issueId] = makeDefaultEntry(issueState, now);
    }
  }

  await writePipelineFile(pipelineFilePath, pipeline);
}

/**
 * Shallow-merge a partial update into an existing pipeline issue entry.
 * Creates a new entry with defaults if issueId is not tracked (per spec).
 * Caps phaseHistory at MAX_PHASE_HISTORY entries (oldest dropped).
 */
export function patchIssueEntry(
  existing: PipelineIssueEntry | undefined,
  patch: Record<string, unknown>
): PipelineIssueEntry {
  const now = new Date().toISOString();
  const base: PipelineIssueEntry = existing ?? {
    status: "unknown",
    workerSessionId: null,
    workerMode: null,
    suggestedAction: "skip",
    hasPr: false,
    prIsDraft: null,
    ciStatus: null,
    lastAction: null,
    lastActionAt: null,
    lastProgressAt: now,
    blockedReason: null,
    blockedSince: null,
    blockedBy: [],
    staleAfterMinutes: 60,
    prUrl: null,
    enteredPipelineAt: now,
    phaseHistory: [],
  };

  const merged = { ...base, ...patch };

  // Cap phaseHistory at 20 entries (oldest dropped)
  if (Array.isArray(merged.phaseHistory) && merged.phaseHistory.length > MAX_PHASE_HISTORY) {
    merged.phaseHistory = merged.phaseHistory.slice(-MAX_PHASE_HISTORY);
  }

  return merged as PipelineIssueEntry;
}
