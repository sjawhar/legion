import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { readJsonSafe, writeJsonAtomic } from "./storage";
import {
  type TaskIndex,
  type TaskIndexEntry,
  TaskIndexEntrySchema,
  TaskIndexSchema,
  TaskSchema,
} from "./types";

export const INDEX_FILENAME = "active-index.json";

export function indexPathFor(taskDir: string): string {
  return join(taskDir, INDEX_FILENAME);
}

export function readTaskIndex(indexPath: string): TaskIndex | null {
  try {
    const parsed = JSON.parse(readFileSync(indexPath, "utf-8"));
    const result = TaskIndexSchema.safeParse(parsed);
    return result.success ? result.data : null;
  } catch {
    return null;
  }
}

/**
 * Rebuild the active index by scanning T-*.json files on disk.
 * Called when the index is missing or corrupt to prevent hiding existing tasks.
 */
function rebuildIndexFromDisk(taskDir: string): TaskIndex {
  const entries: TaskIndexEntry[] = [];
  try {
    const files = readdirSync(taskDir).filter((f) => f.endsWith(".json") && f.startsWith("T-"));
    for (const file of files) {
      const task = readJsonSafe(join(taskDir, file), TaskSchema);
      if (task && (task.status === "pending" || task.status === "in_progress")) {
        entries.push({ id: task.id, status: task.status });
      }
    }
  } catch {
    // Directory unreadable — return empty index, readActiveTasks will
    // fall back to listTaskFiles which handles this same error
  }
  return { version: 1, entries };
}

export function writeTaskIndexAtomic(indexPath: string, data: TaskIndex): void {
  writeJsonAtomic(indexPath, data);
}

export function upsertIndexEntry(indexPath: string, entry: TaskIndexEntry): void {
  const validated = TaskIndexEntrySchema.parse(entry);
  const index = readTaskIndex(indexPath) ?? rebuildIndexFromDisk(dirname(indexPath));
  const entries = index.entries.filter((e) => e.id !== validated.id);

  if (validated.status === "pending" || validated.status === "in_progress") {
    entries.push(validated);
  }

  writeTaskIndexAtomic(indexPath, { version: 1, entries });
}
