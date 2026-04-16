import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

import { z } from "zod";

import { readLearningFrontMatter } from "./front-matter";
import type { ClassifiedLearningAggregate } from "./types";

const SOFT_CAP = 10;

const indexSchema = z.object({
  index: z.record(z.string(), z.array(z.string())),
  version: z.number(),
});

const entrySchema = z.object({
  entries: z.record(z.string(), z.array(z.string())),
  version: z.number(),
});

type KnowledgeIndex = z.infer<typeof indexSchema>;
type IndexEntry = z.infer<typeof entrySchema>;

export interface PromotionIndexMutation {
  action: "upsert";
  detail?: string;
  key: string;
  learningPath: string;
}

export interface PromotableLearning
  extends Pick<ClassifiedLearningAggregate, "disposition" | "path"> {
  touchedPaths: string[];
}

export interface PromotionResult {
  mutations: PromotionIndexMutation[];
  warnings: string[];
}

function normalizePath(filePath: string): string {
  return filePath.replaceAll("\\", "/");
}

function isPromotedDisposition(disposition: string): boolean {
  return disposition === "accepted" || disposition === "promote";
}

function parseEntry(contents: string): IndexEntry {
  return entrySchema.parse(JSON.parse(contents));
}

export function sanitizeEntryId(entryId: string): string {
  return entryId.replaceAll(/[^a-zA-Z0-9_-]/g, "-");
}

export async function readAssembledIndex(indexDir: string): Promise<KnowledgeIndex> {
  const assembled: KnowledgeIndex = { index: {}, version: 1 };

  let files: string[];
  try {
    files = await readdir(indexDir);
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      return assembled;
    }
    throw error;
  }

  for (const file of files.filter((f) => f.endsWith(".json")).sort()) {
    try {
      const contents = await readFile(path.join(indexDir, file), "utf-8");
      const entry = parseEntry(contents);
      for (const [key, paths] of Object.entries(entry.entries)) {
        const existing = assembled.index[key] ?? [];
        assembled.index[key] = Array.from(new Set([...existing, ...paths]));
      }
    } catch {
      // Skip malformed entry files — graceful degradation
    }
  }

  return assembled;
}

export function fallbackPrefix(filePath: string): string {
  const normalizedPath = normalizePath(filePath);
  const parts = normalizedPath.split("/");

  if (parts[0] === "packages" && parts.length >= 4) {
    return parts.slice(0, 4).join("/");
  }

  if (parts[0] === ".opencode" && parts[1] === "skills" && parts.length >= 3) {
    return parts.slice(0, 3).join("/");
  }

  const dirname = path.posix.dirname(normalizedPath);
  return dirname === "." ? normalizedPath : dirname;
}

export function deriveIndexPrefixes(filesChanged: string[], existingKeys: string[]): string[] {
  const normalizedKeys = existingKeys.map(normalizePath);

  return Array.from(
    new Set(
      filesChanged.map((filePath) => {
        const normalizedFilePath = normalizePath(filePath);
        let bestMatch: string | null = null;

        for (const key of normalizedKeys) {
          if (
            (normalizedFilePath === key || normalizedFilePath.startsWith(`${key}/`)) &&
            (!bestMatch || key.length > bestMatch.length)
          ) {
            bestMatch = key;
          }
        }

        return bestMatch ?? fallbackPrefix(normalizedFilePath);
      })
    )
  ).sort();
}

export async function trimToSoftCap(entries: string[], docsRoot: string): Promise<string[]> {
  if (entries.length <= SOFT_CAP) {
    return entries;
  }

  const rankedEntries = await Promise.all(
    entries.map(async (entry) => {
      const frontMatter = await readLearningFrontMatter(path.join(docsRoot, entry)).catch(
        () => null
      );
      const parsedDate = frontMatter?.date ? Date.parse(frontMatter.date) : Number.NaN;

      return {
        entry,
        priority: frontMatter?.status === "superseded" ? 0 : 1,
        timestamp: Number.isFinite(parsedDate) ? parsedDate : Number.POSITIVE_INFINITY,
      };
    })
  );

  const entriesToRemove = new Set(
    rankedEntries
      .toSorted(
        (left, right) =>
          left.priority - right.priority ||
          left.timestamp - right.timestamp ||
          left.entry.localeCompare(right.entry)
      )
      .slice(0, entries.length - SOFT_CAP)
      .map((item) => item.entry)
  );

  return entries.filter((entry) => !entriesToRemove.has(entry));
}

export async function applyPromotions(
  indexDir: string,
  docsRoot: string,
  promoted: PromotableLearning[],
  entryId: string
): Promise<PromotionResult> {
  let assembledIndex: KnowledgeIndex;

  try {
    assembledIndex = await readAssembledIndex(indexDir);
  } catch (error) {
    return {
      mutations: [],
      warnings: [
        `Failed to read knowledge index at ${indexDir}: ${error instanceof Error ? error.message : String(error)}`,
      ],
    };
  }

  const mutations: PromotionIndexMutation[] = [];
  const newEntries: Record<string, string[]> = {};

  for (const learning of promoted) {
    if (!isPromotedDisposition(learning.disposition)) {
      continue;
    }

    const prefixes = deriveIndexPrefixes(learning.touchedPaths, Object.keys(assembledIndex.index));
    for (const prefix of prefixes) {
      const currentEntries = assembledIndex.index[prefix] ?? [];
      const dedupedEntries = Array.from(new Set([...currentEntries, learning.path]));

      if (!currentEntries.includes(learning.path)) {
        mutations.push({
          action: "upsert",
          key: prefix,
          learningPath: learning.path,
        });
      }

      assembledIndex.index[prefix] = await trimToSoftCap(dedupedEntries, docsRoot);

      // Track entries for this entry's file
      const entryPaths = newEntries[prefix] ?? [];
      if (!entryPaths.includes(learning.path)) {
        newEntries[prefix] = [...entryPaths, learning.path];
      }
    }
  }

  // Read existing entry file to merge with new entries
  const entryFileName = `${sanitizeEntryId(entryId)}.json`;
  const entryPath = path.join(indexDir, entryFileName);
  let existingEntry: IndexEntry = { entries: {}, version: 1 };
  try {
    const contents = await readFile(entryPath, "utf-8");
    existingEntry = parseEntry(contents);
  } catch {
    // No existing entry file — start fresh
  }

  // Merge new entries into existing entry
  for (const [key, paths] of Object.entries(newEntries)) {
    const existing = existingEntry.entries[key] ?? [];
    existingEntry.entries[key] = Array.from(new Set([...existing, ...paths]));
  }

  await mkdir(indexDir, { recursive: true });
  await writeFile(entryPath, `${JSON.stringify(existingEntry, null, 2)}\n`);

  return {
    mutations: mutations.toSorted(
      (left, right) =>
        left.key.localeCompare(right.key) || left.learningPath.localeCompare(right.learningPath)
    ),
    warnings: [],
  };
}
