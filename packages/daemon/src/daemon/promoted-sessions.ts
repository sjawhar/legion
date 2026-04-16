import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { PromotedSessionsFileSchema } from "./schemas";

export interface PromotedSession {
  sessionId: string;
  role: string;
  repo?: string;
  promotedAt: string;
}

export interface PromotedSessionsFile {
  sessions: Record<string, PromotedSession>;
}

function emptyFile(): PromotedSessionsFile {
  return { sessions: {} };
}

async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (err) {
    console.warn(`[promoted-sessions] Failed to rename corrupt file ${filePath}:`, err);
  }
}

export async function readPromotedSessions(filePath: string): Promise<PromotedSessionsFile> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return emptyFile();
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[promoted-sessions] Corrupt JSON in ${filePath}, moving aside`);
      await moveCorruptFile(filePath);
      return emptyFile();
    }

    const result = PromotedSessionsFileSchema.safeParse(parsed);
    if (!result.success) {
      console.warn(`[promoted-sessions] Invalid schema in ${filePath}, moving aside`);
      await moveCorruptFile(filePath);
      return emptyFile();
    }

    return result.data;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return emptyFile();
    }
    throw error;
  }
}

export async function writePromotedSessions(
  filePath: string,
  data: PromotedSessionsFile
): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });

  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(data, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, filePath);
}

export async function promoteSession(
  filePath: string,
  sessionId: string,
  role: string,
  repo?: string
): Promise<PromotedSession> {
  const data = await readPromotedSessions(filePath);

  const session: PromotedSession = {
    sessionId,
    role,
    ...(repo ? { repo } : {}),
    promotedAt: new Date().toISOString(),
  };

  data.sessions[sessionId] = session;
  await writePromotedSessions(filePath, data);
  return session;
}

export async function demoteSession(filePath: string, sessionId: string): Promise<boolean> {
  const data = await readPromotedSessions(filePath);

  if (!data.sessions[sessionId]) {
    return false;
  }

  delete data.sessions[sessionId];
  await writePromotedSessions(filePath, data);
  return true;
}

export function listPromotedSessions(data: PromotedSessionsFile): PromotedSession[] {
  return Object.values(data.sessions);
}
