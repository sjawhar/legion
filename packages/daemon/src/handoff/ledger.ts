import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import type { HandoffPhase, PhaseHandoff } from "@legion/contracts";
import {
  describePhaseHandoffProblems,
  describePhaseHandoffWriteProblems,
  HANDOFF_PHASES,
  HANDOFF_SCHEMA_VERSION,
  LEGION_DIR_NAME,
  PHASE_FILE_NAMES,
  validatePhaseHandoff,
} from "@legion/contracts";

function ensureDir(dir: string): void {
  mkdirSync(dir, { recursive: true });
}

function atomicWriteJson(filePath: string, data: unknown): void {
  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  writeFileSync(tempPath, JSON.stringify(data, null, 2), "utf-8");
  try {
    renameSync(tempPath, filePath);
  } catch (e) {
    try {
      unlinkSync(tempPath);
    } catch {}
    throw e;
  }
}

function getPhaseFilePath(workspaceDir: string, phase: HandoffPhase): string {
  return path.join(getLegionDir(workspaceDir), PHASE_FILE_NAMES[phase]);
}

export function getLegionDir(workspaceDir: string): string {
  return path.join(workspaceDir, LEGION_DIR_NAME);
}

export function ensureLegionDir(workspaceDir: string): void {
  ensureDir(getLegionDir(workspaceDir));
}

export function writePhaseHandoff<T extends object>(
  workspaceDir: string,
  phase: HandoffPhase,
  data: T
): void {
  const payload = {
    ...data,
    schemaVersion: HANDOFF_SCHEMA_VERSION,
    phase,
    completed: new Date().toISOString(),
  };
  // The one path every phase handoff is written through: a payload this phase's schema rejects
  // never reaches the branch, and the refusal names the field (LEGION-53).
  const problems = describePhaseHandoffWriteProblems(payload);
  if (problems.length > 0) {
    throw new Error(`Invalid ${phase} handoff: ${problems.join("; ")}`);
  }
  ensureLegionDir(workspaceDir);
  atomicWriteJson(getPhaseFilePath(workspaceDir, phase), payload);
}

export function readPhaseHandoff(workspaceDir: string, phase: HandoffPhase): PhaseHandoff | null {
  try {
    const filePath = getPhaseFilePath(workspaceDir, phase);
    if (!existsSync(filePath)) {
      return null;
    }

    const parsed = JSON.parse(readFileSync(filePath, "utf-8")) as unknown;
    const handoff = validatePhaseHandoff(parsed);
    if (!handoff) {
      // Failing open is the contract (the phase reads as missing), but never silently: the
      // next worker has to be able to see which field cost it the predecessor's handoff.
      console.error(
        `[handoff] Ignoring ${filePath}: ${describePhaseHandoffProblems(parsed).join("; ")}`
      );
      return null;
    }
    if (handoff.phase !== phase) {
      console.error(
        `[handoff] Ignoring ${filePath}: phase is "${handoff.phase}", expected "${phase}"`
      );
      return null;
    }
    return handoff;
  } catch (e) {
    console.error(`[handoff] Failed to read ${phase} handoff:`, e);
    return null;
  }
}

export function readAllHandoffs(workspaceDir: string): Partial<Record<HandoffPhase, PhaseHandoff>> {
  const result: Partial<Record<HandoffPhase, PhaseHandoff>> = {};

  for (const phase of HANDOFF_PHASES) {
    const handoff = readPhaseHandoff(workspaceDir, phase);
    if (handoff) {
      result[phase] = handoff;
    }
  }

  return result;
}
