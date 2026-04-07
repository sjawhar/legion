import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { PipelineCacheSchema } from "./schemas";

/** 5 minutes in milliseconds — cache is stale after this. */
export const STALE_THRESHOLD_MS = 5 * 60 * 1000;

export interface PipelineCacheEntry {
  collectedAt: string;
  issues: Record<string, unknown>;
  stale: boolean;
}

async function moveCorruptFile(filePath: string): Promise<void> {
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  const corruptPath = `${filePath}.corrupt.${timestamp}`;
  try {
    await rename(filePath, corruptPath);
  } catch (err) {
    console.warn(`[pipeline-cache] Failed to rename corrupt file ${filePath}:`, err);
  }
}

/**
 * Read the pipeline cache from disk.
 *
 * Returns null when:
 * - File does not exist
 * - File is empty
 * - File contains invalid JSON (moved to .corrupt.{timestamp})
 * - File fails schema validation (moved to .corrupt.{timestamp})
 *
 * The `stale` field is computed at read time based on `collectedAt`.
 */
export async function readPipelineCache(filePath: string): Promise<PipelineCacheEntry | null> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return null;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[pipeline-cache] Corrupt JSON in ${filePath}, moving aside`);
      await moveCorruptFile(filePath);
      return null;
    }

    const validation = PipelineCacheSchema.safeParse(parsed);
    if (!validation.success) {
      const issues = validation.error.issues.map((i) => i.message).join(", ");
      console.warn(`[pipeline-cache] Schema validation failed for ${filePath}: ${issues}`);
      await moveCorruptFile(filePath);
      return null;
    }

    const data = validation.data;
    const collectedAtMs = new Date(data.collectedAt).getTime();
    const stale = Date.now() - collectedAtMs > STALE_THRESHOLD_MS;

    return {
      collectedAt: data.collectedAt,
      issues: data.issues as Record<string, unknown>,
      stale,
    };
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

/**
 * Write a collected state snapshot to the pipeline cache.
 *
 * Uses atomic write (temp file + rename) to avoid partial reads.
 * The `collectedAt` timestamp is added automatically.
 */
export async function writePipelineCache(
  filePath: string,
  collectedState: { issues: Record<string, unknown> }
): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });

  const cached = {
    collectedAt: new Date().toISOString(),
    issues: collectedState.issues,
  };

  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(cached, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, filePath);
}
