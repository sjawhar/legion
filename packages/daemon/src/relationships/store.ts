/**
 * Persistent storage for cross-issue relationships.
 *
 * Uses the same atomic-write pattern as state-file.ts:
 * write to temp file, then rename (atomic swap).
 * Graceful recovery from missing or corrupt files.
 */

import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { EMPTY_GRAPH, type Relationship, type RelationshipGraph } from "./types";

/**
 * Read relationship graph from disk.
 * Returns empty graph on missing or corrupt file (never throws).
 */
export async function readRelationships(filePath: string): Promise<RelationshipGraph> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) {
      return EMPTY_GRAPH;
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      console.warn(`[relationships] Corrupt JSON in ${filePath}, returning empty graph`);
      return EMPTY_GRAPH;
    }

    if (!isValidGraph(parsed)) {
      console.warn(`[relationships] Invalid schema in ${filePath}, returning empty graph`);
      return EMPTY_GRAPH;
    }

    return parsed;
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") {
      return EMPTY_GRAPH;
    }
    console.warn(`[relationships] Error reading ${filePath}: ${err.message}`);
    return EMPTY_GRAPH;
  }
}

/**
 * Write relationship graph to disk atomically.
 * Creates parent directories if needed.
 */
export async function writeRelationships(
  filePath: string,
  graph: RelationshipGraph
): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });

  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  const payload = JSON.stringify(graph, null, 2);
  await writeFile(tempPath, payload, "utf-8");
  await rename(tempPath, filePath);
}

/**
 * Merge incoming relationships into existing ones, deduplicating.
 * Two relationships are considered duplicates if they have the same
 * parent, child, and type.
 */
export function mergeRelationships(
  existing: Relationship[],
  incoming: Relationship[]
): Relationship[] {
  const seen = new Set<string>();
  const result: Relationship[] = [];

  for (const rel of [...existing, ...incoming]) {
    const key = `${rel.parent}|${rel.child}|${rel.type}`;
    if (!seen.has(key)) {
      seen.add(key);
      result.push(rel);
    }
  }

  return result;
}

/**
 * Get parent and child issues for a given issue ID.
 */
export function getRelativesForIssue(
  issueId: string,
  graph: RelationshipGraph
): { parents: string[]; children: string[] } {
  const parents: string[] = [];
  const children: string[] = [];
  const normalizedId = issueId.toLowerCase();

  for (const rel of graph.relationships) {
    if (rel.child.toLowerCase() === normalizedId) {
      parents.push(rel.parent);
    }
    if (rel.parent.toLowerCase() === normalizedId) {
      children.push(rel.child);
    }
  }

  return { parents, children };
}

/**
 * Validate that a parsed value matches the RelationshipGraph schema.
 */
function isValidGraph(value: unknown): value is RelationshipGraph {
  if (typeof value !== "object" || value === null) {
    return false;
  }

  const obj = value as Record<string, unknown>;
  if (!Array.isArray(obj.relationships)) {
    return false;
  }

  return obj.relationships.every(
    (r: unknown) =>
      typeof r === "object" &&
      r !== null &&
      typeof (r as Record<string, unknown>).parent === "string" &&
      typeof (r as Record<string, unknown>).child === "string" &&
      (r as Record<string, unknown>).type === "parent-child"
  );
}
