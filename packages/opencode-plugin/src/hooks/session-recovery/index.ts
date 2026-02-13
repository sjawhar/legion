import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { PluginInput } from "@opencode-ai/plugin";
import type { ErrorType } from "./types";

interface MessageInfo {
  id?: string;
  role?: string;
  sessionID?: string;
  parentID?: string;
  error?: unknown;
}

interface MessageData {
  info?: {
    id?: string;
    role?: string;
    sessionID?: string;
    parentID?: string;
    error?: unknown;
    agent?: string;
    model?: {
      providerID: string;
      modelID: string;
    };
  };
  parts?: Array<{
    type: string;
    id?: string;
    text?: string;
    thinking?: string;
    name?: string;
    input?: Record<string, unknown>;
    callID?: string;
  }>;
}

interface StoredMessageMeta {
  id: string;
  sessionID: string;
  role: "user" | "assistant";
  time?: {
    created: number;
    completed?: number;
  };
}

interface StoredPart {
  id: string;
  sessionID: string;
  messageID: string;
  type: string;
  [key: string]: unknown;
}

interface ToolUsePart {
  type: "tool_use";
  id: string;
  name: string;
  input: Record<string, unknown>;
}

const RECOVERY_RESUME_TEXT = "[session recovered - continuing previous task]";

const OPENCODE_STORAGE = join(
  process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share"),
  "opencode",
  "storage"
);
const MESSAGE_STORAGE = join(OPENCODE_STORAGE, "message");
const PART_STORAGE = join(OPENCODE_STORAGE, "part");
const THINKING_TYPES = new Set(["thinking", "redacted_thinking", "reasoning"]);

function getMessageDir(sessionID: string): string {
  if (!existsSync(MESSAGE_STORAGE)) return "";

  const directPath = join(MESSAGE_STORAGE, sessionID);
  if (existsSync(directPath)) {
    return directPath;
  }

  for (const dir of readdirSync(MESSAGE_STORAGE)) {
    const sessionPath = join(MESSAGE_STORAGE, dir, sessionID);
    if (existsSync(sessionPath)) {
      return sessionPath;
    }
  }

  return "";
}

function readMessages(sessionID: string): StoredMessageMeta[] {
  const messageDir = getMessageDir(sessionID);
  if (!messageDir || !existsSync(messageDir)) return [];

  const messages: StoredMessageMeta[] = [];
  for (const file of readdirSync(messageDir)) {
    if (!file.endsWith(".json")) continue;
    try {
      const content = readFileSync(join(messageDir, file), "utf-8");
      messages.push(JSON.parse(content));
    } catch {}
  }

  return messages.sort((a, b) => {
    const aTime = a.time?.created ?? 0;
    const bTime = b.time?.created ?? 0;
    if (aTime !== bTime) return aTime - bTime;
    return a.id.localeCompare(b.id);
  });
}

function readParts(messageID: string): StoredPart[] {
  const partDir = join(PART_STORAGE, messageID);
  if (!existsSync(partDir)) return [];

  const parts: StoredPart[] = [];
  for (const file of readdirSync(partDir)) {
    if (!file.endsWith(".json")) continue;
    try {
      const content = readFileSync(join(partDir, file), "utf-8");
      parts.push(JSON.parse(content));
    } catch {}
  }

  return parts;
}

function extractToolUseIds(parts: MessageData["parts"]): string[] {
  if (!parts) return [];
  return parts
    .filter((p): p is ToolUsePart => p.type === "tool_use" && typeof p.id === "string")
    .map((p) => p.id);
}

function findMessagesWithThinkingBlocks(sessionID: string): string[] {
  const messages = readMessages(sessionID);
  const result: string[] = [];

  for (const msg of messages) {
    if (msg.role !== "assistant") continue;
    const parts = readParts(msg.id);
    const hasThinking = parts.some((p) => THINKING_TYPES.has(p.type));
    if (hasThinking) {
      result.push(msg.id);
    }
  }

  return result;
}

function findMessagesWithOrphanThinking(sessionID: string): string[] {
  const messages = readMessages(sessionID);
  const result: string[] = [];

  for (const msg of messages) {
    if (msg.role !== "assistant") continue;
    const parts = readParts(msg.id);
    if (parts.length === 0) continue;
    const hasAnyThinking = parts.some((p) => THINKING_TYPES.has(p.type));
    if (!hasAnyThinking) continue;
    const sortedParts = [...parts].sort((a, b) => a.id.localeCompare(b.id));
    const firstPart = sortedParts[0];
    const firstIsThinking = THINKING_TYPES.has(firstPart.type);
    if (!firstIsThinking) {
      result.push(msg.id);
    }
  }

  return result;
}

function findMessageByIndexNeedingThinking(sessionID: string, targetIndex: number): string | null {
  const messages = readMessages(sessionID);
  if (targetIndex < 0 || targetIndex >= messages.length) return null;

  const targetMsg = messages[targetIndex];
  if (targetMsg.role !== "assistant") return null;

  const parts = readParts(targetMsg.id);
  if (parts.length === 0) return null;

  const hasAnyThinking = parts.some((p) => THINKING_TYPES.has(p.type));
  if (!hasAnyThinking) return null;

  const sortedParts = [...parts].sort((a, b) => a.id.localeCompare(b.id));
  const firstPart = sortedParts[0];
  const firstIsThinking = THINKING_TYPES.has(firstPart.type);
  if (!firstIsThinking) {
    return targetMsg.id;
  }

  return null;
}

function prependThinkingPart(sessionID: string, messageID: string): boolean {
  const partDir = join(PART_STORAGE, messageID);
  if (!existsSync(partDir)) {
    mkdirSync(partDir, { recursive: true });
  }

  const partId = "prt_0000000000_thinking";
  const part = {
    id: partId,
    sessionID,
    messageID,
    type: "thinking",
    thinking: "[Continuing from previous reasoning]",
    synthetic: true,
  };

  try {
    writeFileSync(join(partDir, `${partId}.json`), JSON.stringify(part, null, 2));
    return true;
  } catch {
    return false;
  }
}

function stripThinkingParts(messageID: string): boolean {
  const partDir = join(PART_STORAGE, messageID);
  if (!existsSync(partDir)) return false;

  let anyRemoved = false;
  for (const file of readdirSync(partDir)) {
    if (!file.endsWith(".json")) continue;
    try {
      const filePath = join(partDir, file);
      const content = readFileSync(filePath, "utf-8");
      const part = JSON.parse(content) as StoredPart;
      if (THINKING_TYPES.has(part.type)) {
        unlinkSync(filePath);
        anyRemoved = true;
      }
    } catch {}
  }

  return anyRemoved;
}

function findLastUserMessage(messages: MessageData[]): MessageData | undefined {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].info?.role === "user") {
      return messages[i];
    }
  }
  return undefined;
}

function getErrorMessage(error: unknown): string {
  if (!error) return "";
  if (typeof error === "string") return error.toLowerCase();

  const errorObj = error as Record<string, unknown>;
  const paths = [
    errorObj.data,
    errorObj.error,
    errorObj,
    (errorObj.data as Record<string, unknown>)?.error,
  ];

  for (const obj of paths) {
    if (obj && typeof obj === "object") {
      const msg = (obj as Record<string, unknown>).message;
      if (typeof msg === "string" && msg.length > 0) {
        return msg.toLowerCase();
      }
    }
  }

  try {
    return JSON.stringify(error).toLowerCase();
  } catch {
    return "";
  }
}

function extractMessageIndex(error: unknown): number | null {
  const message = getErrorMessage(error);
  const match = message.match(/messages\.(\d+)/);
  return match ? parseInt(match[1], 10) : null;
}

export function detectErrorType(error: unknown): ErrorType | null {
  const message = getErrorMessage(error);

  if (
    message.includes("thinking") &&
    (message.includes("first block") ||
      message.includes("must start with") ||
      message.includes("preceeding") ||
      message.includes("final block") ||
      message.includes("cannot be thinking") ||
      (message.includes("expected") && message.includes("found")))
  ) {
    return "thinking_block_order";
  }

  if (message.includes("thinking is disabled") && message.includes("cannot contain")) {
    return "thinking_disabled_violation";
  }

  if (message.includes("tool_use") && message.includes("tool_result")) {
    return "tool_result_missing";
  }

  return null;
}

async function recoverToolResultMissing(
  ctx: PluginInput,
  sessionID: string,
  failedAssistantMsg: MessageData
): Promise<boolean> {
  let parts = failedAssistantMsg.parts ?? [];
  if (parts.length === 0 && failedAssistantMsg.info?.id) {
    const storedParts = readParts(failedAssistantMsg.info.id);
    parts = storedParts.map((p) => ({
      type: p.type === "tool" ? "tool_use" : p.type,
      id: "callID" in p ? (p as { callID?: string }).callID : p.id,
      name: "tool" in p ? (p as { tool?: string }).tool : undefined,
      input:
        "state" in p
          ? (p as { state?: { input?: Record<string, unknown> } }).state?.input
          : undefined,
    }));
  }

  const toolUseIds = extractToolUseIds(parts);
  if (toolUseIds.length === 0) {
    return false;
  }

  const toolResultParts = toolUseIds.map((id) => ({
    type: "tool_result" as const,
    tool_use_id: id,
    content: "Operation cancelled by user (ESC pressed)",
  }));

  try {
    await ctx.client.session.promptAsync({
      path: { id: sessionID },
      // @ts-expect-error - SDK types may not include tool_result parts
      body: { parts: toolResultParts },
      query: { directory: ctx.directory },
    });
    return true;
  } catch {
    return false;
  }
}

async function recoverThinkingBlockOrder(sessionID: string, error: unknown): Promise<boolean> {
  const targetIndex = extractMessageIndex(error);
  if (targetIndex !== null) {
    const targetMessageID = findMessageByIndexNeedingThinking(sessionID, targetIndex);
    if (targetMessageID) {
      return prependThinkingPart(sessionID, targetMessageID);
    }
  }

  const orphanMessages = findMessagesWithOrphanThinking(sessionID);
  if (orphanMessages.length === 0) {
    return false;
  }

  let anySuccess = false;
  for (const messageID of orphanMessages) {
    if (prependThinkingPart(sessionID, messageID)) {
      anySuccess = true;
    }
  }

  return anySuccess;
}

async function recoverThinkingDisabledViolation(sessionID: string): Promise<boolean> {
  const messagesWithThinking = findMessagesWithThinkingBlocks(sessionID);
  if (messagesWithThinking.length === 0) {
    return false;
  }

  let anySuccess = false;
  for (const messageID of messagesWithThinking) {
    if (stripThinkingParts(messageID)) {
      anySuccess = true;
    }
  }

  return anySuccess;
}

async function resumeSession(
  ctx: PluginInput,
  sessionID: string,
  messages: MessageData[]
): Promise<void> {
  const lastUser = findLastUserMessage(messages);
  const agent = lastUser?.info?.agent;
  const model = lastUser?.info?.model;

  await ctx.client.session
    .promptAsync({
      path: { id: sessionID },
      body: {
        parts: [{ type: "text", text: RECOVERY_RESUME_TEXT }],
        agent,
        model,
      },
      query: { directory: ctx.directory },
    })
    .catch(() => {});
}

export interface SessionRecoveryHook {
  handleSessionRecovery: (info: MessageInfo) => Promise<boolean>;
  isRecoverableError: (error: unknown) => boolean;
  isRecovering: (sessionID: string) => boolean;
}

export function createSessionRecoveryHook(ctx: PluginInput): SessionRecoveryHook {
  const processingErrors = new Set<string>();
  const recoveringSessions = new Set<string>();

  const isRecoverableError = (error: unknown): boolean => {
    return detectErrorType(error) !== null;
  };

  const isRecovering = (sessionID: string): boolean => {
    return recoveringSessions.has(sessionID);
  };

  const handleSessionRecovery = async (info: MessageInfo): Promise<boolean> => {
    if (!info || info.role !== "assistant" || !info.error) return false;

    const errorType = detectErrorType(info.error);
    if (!errorType) return false;

    const sessionID = info.sessionID;
    const assistantMsgID = info.id;

    if (!sessionID || !assistantMsgID) return false;
    if (processingErrors.has(assistantMsgID)) return false;
    processingErrors.add(assistantMsgID);

    recoveringSessions.add(sessionID);
    try {
      await ctx.client.session
        .abort({
          path: { id: sessionID },
          query: { directory: ctx.directory },
        })
        .catch(() => {});

      const messagesResp = await ctx.client.session.messages({
        path: { id: sessionID },
        query: { directory: ctx.directory },
      });
      const msgs = (messagesResp as { data?: MessageData[] }).data ?? [];

      const failedMsg = msgs.find((m) => m.info?.id === assistantMsgID);
      if (!failedMsg) {
        return false;
      }

      let success = false;

      if (errorType === "tool_result_missing") {
        success = await recoverToolResultMissing(ctx, sessionID, failedMsg);
      } else if (errorType === "thinking_block_order") {
        success = await recoverThinkingBlockOrder(sessionID, info.error);
      } else if (errorType === "thinking_disabled_violation") {
        success = await recoverThinkingDisabledViolation(sessionID);
      }

      if (success) {
        await resumeSession(ctx, sessionID, msgs);
      }

      return success;
    } finally {
      processingErrors.delete(assistantMsgID);
      if (sessionID) recoveringSessions.delete(sessionID);
    }
  };

  return {
    handleSessionRecovery,
    isRecoverableError,
    isRecovering,
  };
}
