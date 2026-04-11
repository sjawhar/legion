import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { afterEach, describe, expect, it } from "bun:test";

import { applyPromotions, deriveIndexPrefixes } from "../promoter";

const tempDirs: string[] = [];

async function makeTempDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "knowledge-promoter-"));
  tempDirs.push(dir);
  return dir;
}

async function writeLearning(
  docsRoot: string,
  learningPath: string,
  status: string,
  date: string
): Promise<void> {
  const fullPath = path.join(docsRoot, learningPath);
  await mkdir(path.dirname(fullPath), { recursive: true });
  await writeFile(
    fullPath,
    ["---", `status: ${status}`, `date: ${date}`, "---", "", `# ${learningPath}`].join("\n")
  );
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

describe("deriveIndexPrefixes", () => {
  it("prefers the longest existing key match before fallback", () => {
    expect(
      deriveIndexPrefixes(
        [
          "packages/daemon/src/state/decision.ts",
          ".opencode/skills/legion-worker/workflows/plan.md",
        ],
        ["packages/daemon", "packages/daemon/src/state"]
      )
    ).toEqual([".opencode/skills/legion-worker", "packages/daemon/src/state"]);
  });
});

describe("applyPromotions", () => {
  it("adds promoted learnings to matched keys and creates fallback keys", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexPath = path.join(dir, "index.json");

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");
    await writeFile(
      indexPath,
      JSON.stringify(
        {
          index: {
            "packages/daemon/src/state": ["knowledge/existing.md"],
          },
          version: 1,
        },
        null,
        2
      )
    );

    const result = await applyPromotions(indexPath, docsRoot, [
      {
        disposition: "accepted",
        path: "knowledge/promoted.md",
        touchedPaths: [
          "packages/daemon/src/state/decision.ts",
          ".opencode/skills/legion-worker/workflows/plan.md",
        ],
      },
    ]);

    expect(result.warnings).toEqual([]);
    expect(result.mutations).toEqual([
      {
        action: "upsert",
        key: ".opencode/skills/legion-worker",
        learningPath: "knowledge/promoted.md",
      },
      {
        action: "upsert",
        key: "packages/daemon/src/state",
        learningPath: "knowledge/promoted.md",
      },
    ]);

    const savedIndex = JSON.parse(await readFile(indexPath, "utf-8"));
    expect(savedIndex.index["packages/daemon/src/state"]).toEqual([
      "knowledge/existing.md",
      "knowledge/promoted.md",
    ]);
    expect(savedIndex.index[".opencode/skills/legion-worker"]).toEqual(["knowledge/promoted.md"]);
  });

  it("deduplicates and trims superseded entries before active entries", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexPath = path.join(dir, "index.json");
    const existingEntries = Array.from(
      { length: 11 },
      (_, index) => `knowledge/existing-${index}.md`
    );

    for (const [index, learningPath] of existingEntries.entries()) {
      await writeLearning(
        docsRoot,
        learningPath,
        index < 2 ? "superseded" : "active",
        `2026-01-${String(index + 1).padStart(2, "0")}`
      );
    }

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");
    await writeFile(
      indexPath,
      JSON.stringify(
        {
          index: {
            "packages/daemon/src/state": existingEntries,
          },
          version: 1,
        },
        null,
        2
      )
    );

    await applyPromotions(indexPath, docsRoot, [
      {
        disposition: "accepted",
        path: "knowledge/promoted.md",
        touchedPaths: ["packages/daemon/src/state/decision.ts"],
      },
    ]);

    const savedIndex = JSON.parse(await readFile(indexPath, "utf-8"));
    const stateEntries = savedIndex.index["packages/daemon/src/state"] as string[];

    expect(stateEntries).toHaveLength(10);
    expect(stateEntries).toContain("knowledge/promoted.md");
    expect(stateEntries).not.toContain("knowledge/existing-0.md");
    expect(stateEntries).not.toContain("knowledge/existing-1.md");
  });

  it("returns a warning instead of throwing on malformed index JSON", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexPath = path.join(dir, "index.json");

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");
    await writeFile(indexPath, '{"version":"oops"}');

    const result = await applyPromotions(indexPath, docsRoot, [
      {
        disposition: "accepted",
        path: "knowledge/promoted.md",
        touchedPaths: ["packages/daemon/src/state/decision.ts"],
      },
    ]);

    expect(result).toEqual({
      mutations: [],
      warnings: [expect.stringContaining("index")],
    });
  });
});
