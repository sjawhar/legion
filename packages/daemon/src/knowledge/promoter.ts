import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

import { z } from "zod";

import { readLearningFrontMatter } from "./front-matter";
import type { ClassifiedLearningAggregate } from "./types";

const SOFT_CAP = 10;

const indexSchema = z.object({
  index: z.record(z.string(), z.array(z.string())),
  version: z.number(),
});

type KnowledgeIndex = z.infer<typeof indexSchema>;

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

function parseIndex(contents: string): KnowledgeIndex {
  return indexSchema.parse(JSON.parse(contents));
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
  indexPath: string,
  docsRoot: string,
  promoted: PromotableLearning[]
): Promise<PromotionResult> {
  let indexState: KnowledgeIndex = {
    index: {},
    version: 1,
  };

  try {
    const contents = await readFile(indexPath, "utf-8");
    indexState = parseIndex(contents);
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") {
      indexState = { index: {}, version: 1 };
    } else {
      return {
        mutations: [],
        warnings: [
          `Failed to read knowledge index at ${indexPath}: ${error instanceof Error ? error.message : String(error)}`,
        ],
      };
    }
  }

  const mutations: PromotionIndexMutation[] = [];

  for (const learning of promoted) {
    if (!isPromotedDisposition(learning.disposition)) {
      continue;
    }

    const prefixes = deriveIndexPrefixes(learning.touchedPaths, Object.keys(indexState.index));
    for (const prefix of prefixes) {
      const currentEntries = indexState.index[prefix] ?? [];
      const dedupedEntries = Array.from(new Set([...currentEntries, learning.path]));

      if (!currentEntries.includes(learning.path)) {
        mutations.push({
          action: "upsert",
          key: prefix,
          learningPath: learning.path,
        });
      }

      indexState.index[prefix] = await trimToSoftCap(dedupedEntries, docsRoot);
    }
  }

  await mkdir(path.dirname(indexPath), { recursive: true });
  await writeFile(indexPath, `${JSON.stringify(indexState, null, 2)}\n`);

  return {
    mutations: mutations.toSorted(
      (left, right) =>
        left.key.localeCompare(right.key) || left.learningPath.localeCompare(right.learningPath)
    ),
    warnings: [],
  };
}
