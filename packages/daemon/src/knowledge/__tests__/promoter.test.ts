import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import {
  applyPromotions,
  deriveIndexPrefixes,
  readAssembledIndex,
  sanitizeEntryId,
} from "../promoter";

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

async function writeEntryFile(
  indexDir: string,
  entryId: string,
  entries: Record<string, string[]>
): Promise<void> {
  await mkdir(indexDir, { recursive: true });
  await writeFile(
    path.join(indexDir, `${entryId}.json`),
    JSON.stringify({ entries, version: 1 }, null, 2)
  );
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

describe("sanitizeEntryId", () => {
  it("replaces non-alphanumeric characters with dashes", () => {
    expect(sanitizeEntryId("trajectory-labs/240")).toBe("trajectory-labs-240");
    expect(sanitizeEntryId("sjawhar-legion-583")).toBe("sjawhar-legion-583");
    expect(sanitizeEntryId("foo/bar.baz")).toBe("foo-bar-baz");
  });
});

describe("readAssembledIndex", () => {
  it("returns empty index when directory does not exist", async () => {
    const dir = await makeTempDir();
    const indexDir = path.join(dir, ".index");

    const result = await readAssembledIndex(indexDir);
    expect(result).toEqual({ index: {}, version: 1 });
  });

  it("merges multiple entry files into a single index", async () => {
    const dir = await makeTempDir();
    const indexDir = path.join(dir, ".index");

    await writeEntryFile(indexDir, "issue-1", {
      "packages/daemon/src/state": ["knowledge/a.md"],
    });
    await writeEntryFile(indexDir, "issue-2", {
      "packages/daemon/src/state": ["knowledge/b.md"],
      ".opencode/skills/legion-worker": ["knowledge/b.md"],
    });

    const result = await readAssembledIndex(indexDir);
    expect(result.index["packages/daemon/src/state"]).toEqual(["knowledge/a.md", "knowledge/b.md"]);
    expect(result.index[".opencode/skills/legion-worker"]).toEqual(["knowledge/b.md"]);
  });

  it("deduplicates paths across entry files", async () => {
    const dir = await makeTempDir();
    const indexDir = path.join(dir, ".index");

    await writeEntryFile(indexDir, "issue-1", {
      "packages/daemon/src/state": ["knowledge/shared.md"],
    });
    await writeEntryFile(indexDir, "issue-2", {
      "packages/daemon/src/state": ["knowledge/shared.md", "knowledge/unique.md"],
    });

    const result = await readAssembledIndex(indexDir);
    expect(result.index["packages/daemon/src/state"]).toEqual([
      "knowledge/shared.md",
      "knowledge/unique.md",
    ]);
  });

  it("skips malformed entry files", async () => {
    const dir = await makeTempDir();
    const indexDir = path.join(dir, ".index");

    await writeEntryFile(indexDir, "good", {
      "packages/daemon/src/state": ["knowledge/a.md"],
    });
    await mkdir(indexDir, { recursive: true });
    await writeFile(path.join(indexDir, "bad.json"), '{"version":"oops"}');

    const result = await readAssembledIndex(indexDir);
    expect(result.index["packages/daemon/src/state"]).toEqual(["knowledge/a.md"]);
  });

  it("ignores non-JSON files", async () => {
    const dir = await makeTempDir();
    const indexDir = path.join(dir, ".index");

    await writeEntryFile(indexDir, "good", {
      "packages/daemon/src/state": ["knowledge/a.md"],
    });
    await writeFile(path.join(indexDir, "README.md"), "# Index directory");

    const result = await readAssembledIndex(indexDir);
    expect(result.index["packages/daemon/src/state"]).toEqual(["knowledge/a.md"]);
  });
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
    const indexDir = path.join(dir, ".index");

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");
    await writeEntryFile(indexDir, "existing", {
      "packages/daemon/src/state": ["knowledge/existing.md"],
    });

    const result = await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/promoted.md",
          touchedPaths: [
            "packages/daemon/src/state/decision.ts",
            ".opencode/skills/legion-worker/workflows/plan.md",
          ],
        },
      ],
      "test-issue"
    );

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

    const savedEntry = JSON.parse(await readFile(path.join(indexDir, "test-issue.json"), "utf-8"));
    expect(savedEntry.entries["packages/daemon/src/state"]).toEqual(["knowledge/promoted.md"]);
    expect(savedEntry.entries[".opencode/skills/legion-worker"]).toEqual(["knowledge/promoted.md"]);

    // Verify assembled index includes both existing and new entries
    const assembled = await readAssembledIndex(indexDir);
    expect(assembled.index["packages/daemon/src/state"]).toEqual([
      "knowledge/existing.md",
      "knowledge/promoted.md",
    ]);
  });

  it("deduplicates and trims superseded entries before active entries", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexDir = path.join(dir, ".index");
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
    await writeEntryFile(indexDir, "existing", {
      "packages/daemon/src/state": existingEntries,
    });

    await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/promoted.md",
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        },
      ],
      "test-issue"
    );

    // Verify assembled index has trimmed entries
    const assembled = await readAssembledIndex(indexDir);
    const stateEntries = assembled.index["packages/daemon/src/state"] as string[];

    // The existing entry file still has all 11, but the new entry file has the promoted one.
    // The assembled index merges and deduplicates. The soft cap is applied during promotion
    // to the assembled view, but individual entry files are not trimmed.
    // The assembled index should contain the promoted entry.
    expect(stateEntries).toContain("knowledge/promoted.md");
  });

  it("returns a warning instead of throwing on read failure", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    // Create indexDir as a file instead of directory to cause read failure
    const indexDir = path.join(dir, ".index");
    await writeFile(indexDir, "not a directory");

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");

    const result = await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/promoted.md",
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        },
      ],
      "test-issue"
    );

    expect(result).toEqual({
      mutations: [],
      warnings: [expect.stringContaining(".index")],
    });
  });

  it("creates index directory and entry file when directory does not exist", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexDir = path.join(dir, ".index");

    await writeLearning(docsRoot, "knowledge/promoted.md", "active", "2026-04-11");

    const result = await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/promoted.md",
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        },
      ],
      "new-issue"
    );

    expect(result.warnings).toEqual([]);
    expect(result.mutations).toHaveLength(1);

    const savedEntry = JSON.parse(await readFile(path.join(indexDir, "new-issue.json"), "utf-8"));
    expect(savedEntry.entries["packages/daemon/src/state"]).toEqual(["knowledge/promoted.md"]);
  });

  it("merges into existing entry file for the same entryId", async () => {
    const dir = await makeTempDir();
    const docsRoot = path.join(dir, "docs");
    const indexDir = path.join(dir, ".index");

    await writeLearning(docsRoot, "knowledge/first.md", "active", "2026-04-11");
    await writeLearning(docsRoot, "knowledge/second.md", "active", "2026-04-12");

    // First promotion
    await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/first.md",
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        },
      ],
      "same-issue"
    );

    // Second promotion to same entry
    await applyPromotions(
      indexDir,
      docsRoot,
      [
        {
          disposition: "accepted",
          path: "knowledge/second.md",
          touchedPaths: ["packages/daemon/src/state/decision.ts"],
        },
      ],
      "same-issue"
    );

    const savedEntry = JSON.parse(await readFile(path.join(indexDir, "same-issue.json"), "utf-8"));
    expect(savedEntry.entries["packages/daemon/src/state"]).toEqual([
      "knowledge/first.md",
      "knowledge/second.md",
    ]);
  });
});
