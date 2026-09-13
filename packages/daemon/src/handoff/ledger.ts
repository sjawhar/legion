import { randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import type { HandoffMessage, HandoffPhase, PhaseHandoff } from "@legion/contracts";
import {
  describePhaseHandoffProblems,
  HANDOFF_PHASES,
  HANDOFF_SCHEMA_VERSION,
  LEGION_DIR_NAME,
  MESSAGES_DIR_NAME,
  PHASE_FILE_NAMES,
  validateHandoffMessage,
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

function getMessagesDir(workspaceDir: string): string {
  return path.join(getLegionDir(workspaceDir), MESSAGES_DIR_NAME);
}

function getPhaseFilePath(workspaceDir: string, phase: HandoffPhase): string {
  return path.join(getLegionDir(workspaceDir), PHASE_FILE_NAMES[phase]);
}

function generateMessageId(): string {
  const now = new Date();
  const ts = now.toISOString().replace(/[-:T]/g, "").slice(0, 14);
  const rand = randomBytes(4).toString("hex");
  return `${ts}-${rand}`;
}

export function getLegionDir(workspaceDir: string): string {
  return path.join(workspaceDir, LEGION_DIR_NAME);
}

export function ensureLegionDir(workspaceDir: string): void {
  ensureDir(getLegionDir(workspaceDir));
  ensureDir(getMessagesDir(workspaceDir));
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
  const problems = describePhaseHandoffProblems(payload);
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

export function writeMessage(workspaceDir: string, msg: Omit<HandoffMessage, "timestamp">): void {
  ensureLegionDir(workspaceDir);
  const messagesDir = getMessagesDir(workspaceDir);
  const msgId = generateMessageId();
  const fileName = `${msgId}-${msg.from}-to-${msg.to}.json`;
  const payload: HandoffMessage = {
    ...msg,
    timestamp: new Date().toISOString(),
  };
  atomicWriteJson(path.join(messagesDir, fileName), payload);
}

export function readMessages(workspaceDir: string): HandoffMessage[] {
  try {
    const messagesDir = getMessagesDir(workspaceDir);
    if (!existsSync(messagesDir)) {
      return [];
    }

    const entries = readdirSync(messagesDir)
      .filter((entry) => entry.endsWith(".json"))
      .sort((a, b) => a.localeCompare(b));

    const messages: HandoffMessage[] = [];
    for (const entry of entries) {
      const filePath = path.join(messagesDir, entry);

      try {
        if (!statSync(filePath).isFile()) {
          continue;
        }

        const parsed = JSON.parse(readFileSync(filePath, "utf-8")) as unknown;
        const message = validateHandoffMessage(parsed);
        if (message) {
          messages.push(message);
        }
      } catch (e) {
        console.error(`[handoff] Failed to read message file:`, e);
      }
    }

    return messages;
  } catch (e) {
    console.error("[handoff] Failed to read messages directory:", e);
    return [];
  }
}
