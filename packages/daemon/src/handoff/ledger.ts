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
import {
  HANDOFF_PHASES,
  HANDOFF_SCHEMA_VERSION,
  LEGION_DIR_NAME,
  MESSAGES_DIR_NAME,
  PHASE_FILE_NAMES,
  validateHandoffMessage,
  validatePhaseHandoff,
} from "./schema";
import type { HandoffMessage, HandoffPhase, PhaseHandoff } from "./types";

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
  try {
    ensureDir(getLegionDir(workspaceDir));
    ensureDir(getMessagesDir(workspaceDir));
  } catch (e) {
    console.error("[handoff] Failed to create .legion/ directory:", e);
  }
}

export function writePhaseHandoff<T extends object>(
  workspaceDir: string,
  phase: HandoffPhase,
  data: T
): void {
  try {
    ensureLegionDir(workspaceDir);
    const payload = {
      ...data,
      schemaVersion: HANDOFF_SCHEMA_VERSION,
      phase,
      completed: new Date().toISOString(),
    };
    atomicWriteJson(getPhaseFilePath(workspaceDir, phase), payload);
  } catch (e) {
    console.error(`[handoff] Failed to write ${phase} handoff:`, e);
  }
}

export function readPhaseHandoff(workspaceDir: string, phase: HandoffPhase): PhaseHandoff | null {
  try {
    const filePath = getPhaseFilePath(workspaceDir, phase);
    if (!existsSync(filePath)) {
      return null;
    }

    const parsed = JSON.parse(readFileSync(filePath, "utf-8")) as unknown;
    const handoff = validatePhaseHandoff(parsed);
    if (!handoff || handoff.phase !== phase) {
      return null;
    }
    return handoff;
  } catch {
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
  try {
    ensureLegionDir(workspaceDir);
    const messagesDir = getMessagesDir(workspaceDir);
    const msgId = generateMessageId();
    const fileName = `${msgId}-${msg.from}-to-${msg.to}.json`;
    const payload: HandoffMessage = {
      ...msg,
      timestamp: new Date().toISOString(),
    };
    atomicWriteJson(path.join(messagesDir, fileName), payload);
  } catch (e) {
    console.error(`[handoff] Failed to write message ${msg.from}->${msg.to}:`, e);
  }
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
      } catch {}
    }

    return messages;
  } catch {
    return [];
  }
}
