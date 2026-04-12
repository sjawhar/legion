import { appendFile, mkdir, rename, stat, unlink } from "node:fs/promises";
import path from "node:path";

import { resolveLegionPaths } from "../daemon/paths";
import { readAllHandoffs, writePhaseHandoff } from "../handoff/ledger";
import { HANDOFF_PHASES } from "../handoff/schema";
import type { PhaseHandoff } from "../handoff/types";
import {
  type LearningFeedbackPhase,
  type LearningFeedbackRecord,
  LearningFeedbackRecordSchema,
} from "./types";

export function normalizePhaseFeedback(
  handoff: PhaseHandoff | null | undefined
): LearningFeedbackPhase | null {
  if (!handoff) {
    return null;
  }

  const helpful = handoff.learningsHelpful ?? [];
  const injected = handoff.learningsInjected ?? [];

  if (helpful.length === 0 && injected.length === 0) {
    return null;
  }

  return {
    helpful,
    injected,
  };
}

export function buildLearningFeedbackRecordFromHandoffs(
  issueId: string,
  handoffs: Partial<Record<PhaseHandoff["phase"], PhaseHandoff>>,
  timestamp: string
): LearningFeedbackRecord | null {
  const phases: LearningFeedbackRecord["phases"] = {};

  for (const phase of HANDOFF_PHASES) {
    const normalized = normalizePhaseFeedback(handoffs[phase]);
    if (normalized) {
      phases[phase] = normalized;
    }
  }

  if (Object.keys(phases).length === 0) {
    return null;
  }

  return LearningFeedbackRecordSchema.parse({
    issueId,
    phases,
    schemaVersion: 1,
    timestamp,
  });
}

export function deriveLegionIdFromWorkspaceDir(
  workspaceDir: string,
  env: Record<string, string | undefined>,
  homeDir: string
): string {
  const { workspacesDir } = resolveLegionPaths(env, homeDir);
  const relativeWorkspaceDir = path.relative(workspacesDir, workspaceDir);

  if (
    relativeWorkspaceDir === "" ||
    relativeWorkspaceDir.startsWith(`..${path.sep}`) ||
    relativeWorkspaceDir === ".." ||
    path.isAbsolute(relativeWorkspaceDir)
  ) {
    throw new Error(`Workspace is not inside configured workspaces dir: ${workspaceDir}`);
  }

  const parts = relativeWorkspaceDir.split(path.sep).filter(Boolean);
  if (parts.length < 2) {
    throw new Error(`Workspace path does not include legion owner and issue: ${workspaceDir}`);
  }

  return `${parts[0]}/${parts[1]}`;
}

export function getLearningFeedbackLogPath(
  legionId: string,
  env: Record<string, string | undefined>,
  homeDir: string
): string {
  const paths = resolveLegionPaths(env, homeDir);
  return path.join(paths.forLegion(legionId).legionStateDir, "learning-feedback.jsonl");
}

export class FileLearningFeedbackWriter {
  constructor(
    private readonly filePath: string,
    private readonly maxBytes: number = 50 * 1024 * 1024
  ) {}

  async append(line: string): Promise<void> {
    await mkdir(path.dirname(this.filePath), { recursive: true });

    try {
      const fileStats = await stat(this.filePath).catch(() => null);
      if (fileStats && fileStats.size >= this.maxBytes) {
        const backupPath = `${this.filePath}.1`;
        try {
          await unlink(backupPath);
        } catch {}
        await rename(this.filePath, backupPath);
      }
    } catch {}

    await appendFile(this.filePath, `${line}\n`, "utf-8");
  }
}

export interface AppendLearningFeedbackRecordOptions {
  env: Record<string, string | undefined>;
  homeDir: string;
  legionId: string;
  maxBytes?: number;
  writer?: Pick<FileLearningFeedbackWriter, "append">;
}

export async function appendLearningFeedbackRecord(
  record: LearningFeedbackRecord,
  options: AppendLearningFeedbackRecordOptions
): Promise<string> {
  const validatedRecord = LearningFeedbackRecordSchema.parse(record);
  const filePath = getLearningFeedbackLogPath(options.legionId, options.env, options.homeDir);
  const writer = options.writer ?? new FileLearningFeedbackWriter(filePath, options.maxBytes);

  await writer.append(JSON.stringify(validatedRecord));
  return filePath;
}

export interface CaptureLearningFeedbackFromWorkspaceOptions {
  env: Record<string, string | undefined>;
  homeDir: string;
  issueId: string;
  maxBytes?: number;
  timestamp?: string;
  workspaceDir: string;
}

export interface CaptureLearningFeedbackFromWorkspaceResult {
  filePath?: string;
  reason?: "no_learning_feedback";
  written: boolean;
}

export async function captureLearningFeedbackFromWorkspace(
  options: CaptureLearningFeedbackFromWorkspaceOptions
): Promise<CaptureLearningFeedbackFromWorkspaceResult> {
  const handoffs = readAllHandoffs(options.workspaceDir);
  const record = buildLearningFeedbackRecordFromHandoffs(
    options.issueId,
    handoffs,
    options.timestamp ?? new Date().toISOString()
  );

  if (!record) {
    return {
      reason: "no_learning_feedback",
      written: false,
    };
  }

  const legionId = deriveLegionIdFromWorkspaceDir(
    options.workspaceDir,
    options.env,
    options.homeDir
  );
  const filePath = await appendLearningFeedbackRecord(record, {
    env: options.env,
    homeDir: options.homeDir,
    legionId,
    maxBytes: options.maxBytes,
  });

  return {
    filePath,
    written: true,
  };
}

export interface CaptureRetroLearningFeedbackOptions {
  docsCreated?: string[];
  learningsHelpful?: string[];
  learningsInjected?: string[];
  reason?: string;
  skipped?: boolean;
  workspaceDir: string;
}

export function captureRetroLearningFeedback(options: CaptureRetroLearningFeedbackOptions): void {
  writePhaseHandoff(options.workspaceDir, "retro", {
    docsCreated: options.docsCreated,
    learningsHelpful: options.learningsHelpful,
    learningsInjected: options.learningsInjected,
    reason: options.reason,
    skipped: options.skipped,
  });
}
