import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import type { CodebaseIndex } from "./types";
import { CodebaseIndexSchema } from "./types";

async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (error) {
    console.warn(`[index] Failed to move corrupt index file ${filePath}:`, error);
  }
}

export async function readCodebaseIndex(filePath: string): Promise<CodebaseIndex | null> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return null;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[index] Corrupt JSON in ${filePath}, moving aside and rebuilding`);
      await moveCorruptFile(filePath);
      return null;
    }

    const validation = CodebaseIndexSchema.safeParse(parsed);
    if (!validation.success) {
      const issues = validation.error.issues.map((issue) => issue.message).join(", ");
      console.warn(`[index] Schema validation failed for ${filePath}: ${issues}`);
      await moveCorruptFile(filePath);
      return null;
    }

    return validation.data;
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

export async function writeCodebaseIndex(filePath: string, index: CodebaseIndex): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });

  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  await writeFile(tempPath, JSON.stringify(index, null, 2), "utf-8");
  await rename(tempPath, filePath);
}
