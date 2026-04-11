import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

import { resolveLegionPaths } from "../daemon/paths";
import { readAllHandoffs } from "../handoff/ledger";
import { HANDOFF_PHASES, LEGION_DIR_NAME } from "../handoff/schema";
import type { PhaseHandoff } from "../handoff/types";
import {
  type CollectedIssueFeedback,
  type LearningFeedbackPhase,
  type LearningFeedbackRecord,
  LearningFeedbackRecordSchema,
} from "./types";

const DOCS_SOLUTIONS_PREFIX = "docs/solutions/";

export interface CollectLearningFeedbackOptions {
  env: Record<string, string | undefined>;
  homeDir: string;
  legionId: string;
}

export interface CollectedIssueCandidate extends CollectedIssueFeedback {
  source: "log" | "workspace";
}

export function canonicalizeLearningPath(rawPath: string): string {
  const normalizedPath = rawPath.replaceAll("\\", "/");
  return normalizedPath.startsWith(DOCS_SOLUTIONS_PREFIX)
    ? normalizedPath.slice(DOCS_SOLUTIONS_PREFIX.length)
    : normalizedPath;
}

function canonicalizeTouchedPath(rawPath: string): string {
  return rawPath.replaceAll("\\", "/");
}

function canonicalizePhase(
  phase: LearningFeedbackPhase | null | undefined
): LearningFeedbackPhase | null {
  if (!phase) {
    return null;
  }

  const helpful = Array.from(new Set(phase.helpful.map(canonicalizeLearningPath))).sort();
  const injected = Array.from(new Set(phase.injected.map(canonicalizeLearningPath))).sort();

  if (helpful.length === 0 && injected.length === 0) {
    return null;
  }

  return {
    helpful,
    injected,
  };
}

function canonicalizeRecord(record: LearningFeedbackRecord): LearningFeedbackRecord {
  const phases: LearningFeedbackRecord["phases"] = {};

  for (const phase of HANDOFF_PHASES) {
    const normalizedPhase = canonicalizePhase(record.phases[phase]);
    if (normalizedPhase) {
      phases[phase] = normalizedPhase;
    }
  }

  return LearningFeedbackRecordSchema.parse({
    issueId: record.issueId,
    phases,
    schemaVersion: record.schemaVersion,
    timestamp: record.timestamp,
  });
}

async function readLearningFeedbackLog(
  options: CollectLearningFeedbackOptions
): Promise<{ candidates: CollectedIssueCandidate[]; warnings: string[] }> {
  const legionPaths = resolveLegionPaths(options.env, options.homeDir).forLegion(options.legionId);
  const logPath = path.join(legionPaths.legionStateDir, "learning-feedback.jsonl");
  const warnings: string[] = [];

  let fileContents: string;
  try {
    fileContents = await readFile(logPath, "utf-8");
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      return { candidates: [], warnings };
    }
    throw error;
  }

  const candidates: CollectedIssueCandidate[] = [];

  for (const [index, line] of fileContents.split(/\r?\n/).entries()) {
    const trimmedLine = line.trim();
    if (!trimmedLine) {
      continue;
    }

    try {
      const parsedRecord = LearningFeedbackRecordSchema.parse(JSON.parse(trimmedLine));
      candidates.push({
        issueId: parsedRecord.issueId,
        records: [canonicalizeRecord(parsedRecord)],
        source: "log",
        touchedPaths: [],
      });
    } catch {
      warnings.push(`[knowledge] Malformed JSONL at line ${index + 1}: skipped`);
    }
  }

  return { candidates, warnings };
}

async function scanForWorkspaceDirs(rootDir: string): Promise<string[]> {
  try {
    const entries = await readdir(rootDir, { encoding: "utf8", withFileTypes: true });
    const workspaceDirs: string[] = [];

    for (const entry of entries) {
      if (!entry.isDirectory()) {
        continue;
      }

      const candidate = path.join(rootDir, entry.name);
      try {
        const childEntries = await readdir(candidate, { encoding: "utf8" });
        if (childEntries.includes(LEGION_DIR_NAME)) {
          workspaceDirs.push(candidate);
        }
      } catch {}
    }

    return workspaceDirs.sort();
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      return [];
    }
    throw error;
  }
}

function buildWorkspacePhase(handoff: PhaseHandoff): LearningFeedbackPhase | null {
  return canonicalizePhase({
    helpful: handoff.learningsHelpful ?? [],
    injected: handoff.learningsInjected ?? [],
  });
}

function collectWorkspaceIssue(workspaceDir: string): CollectedIssueCandidate | null {
  const handoffs = readAllHandoffs(workspaceDir);
  const phases: LearningFeedbackRecord["phases"] = {};
  const timestamps: string[] = [];
  const touchedPaths = new Set<string>();

  for (const phase of HANDOFF_PHASES) {
    const handoff = handoffs[phase];
    if (!handoff) {
      continue;
    }

    const normalizedPhase = buildWorkspacePhase(handoff);
    if (normalizedPhase) {
      phases[phase] = normalizedPhase;
      timestamps.push(handoff.completed);
    }

    if (phase === "implement" && "filesChanged" in handoff && Array.isArray(handoff.filesChanged)) {
      for (const touchedPath of handoff.filesChanged) {
        touchedPaths.add(canonicalizeTouchedPath(touchedPath));
      }
    }
  }

  if (Object.keys(phases).length === 0) {
    return null;
  }

  const issueId = path.basename(workspaceDir);
  const timestamp = timestamps.sort().at(-1) ?? new Date().toISOString();

  return {
    issueId,
    records: [
      LearningFeedbackRecordSchema.parse({
        issueId,
        phases,
        schemaVersion: 1,
        timestamp,
      }),
    ],
    source: "workspace",
    touchedPaths: Array.from(touchedPaths).sort(),
  };
}

export function dedupeCollectedIssues(issues: CollectedIssueCandidate[]): CollectedIssueFeedback[] {
  const deduped = new Map<
    string,
    {
      logRecords: LearningFeedbackRecord[];
      touchedPaths: Set<string>;
      workspaceRecords: LearningFeedbackRecord[];
    }
  >();

  for (const issue of issues) {
    const entry = deduped.get(issue.issueId) ?? {
      logRecords: [],
      touchedPaths: new Set<string>(),
      workspaceRecords: [],
    };

    for (const touchedPath of issue.touchedPaths) {
      entry.touchedPaths.add(canonicalizeTouchedPath(touchedPath));
    }

    if (issue.source === "log") {
      entry.logRecords.push(...issue.records.map(canonicalizeRecord));
    } else {
      entry.workspaceRecords.push(...issue.records.map(canonicalizeRecord));
    }

    deduped.set(issue.issueId, entry);
  }

  return Array.from(deduped.entries())
    .sort(([leftIssueId], [rightIssueId]) => leftIssueId.localeCompare(rightIssueId))
    .map(([issueId, entry]) => ({
      issueId,
      records: (entry.logRecords.length > 0 ? entry.logRecords : entry.workspaceRecords).sort(
        (left, right) => left.timestamp.localeCompare(right.timestamp)
      ),
      touchedPaths: Array.from(entry.touchedPaths).sort(),
    }));
}

export async function collectLearningFeedback(
  options: CollectLearningFeedbackOptions
): Promise<{ issues: CollectedIssueFeedback[]; warnings: string[] }> {
  const legionPaths = resolveLegionPaths(options.env, options.homeDir).forLegion(options.legionId);
  const [logResult, workspaceDirs] = await Promise.all([
    readLearningFeedbackLog(options),
    scanForWorkspaceDirs(legionPaths.workspacesDir),
  ]);

  const workspaceIssues = workspaceDirs
    .map((workspaceDir) => collectWorkspaceIssue(workspaceDir))
    .filter((issue): issue is CollectedIssueCandidate => issue !== null);

  return {
    issues: dedupeCollectedIssues([...logResult.candidates, ...workspaceIssues]),
    warnings: logResult.warnings,
  };
}
