import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  getRelativesForIssue,
  mergeRelationships,
  readRelationships,
  writeRelationships,
} from "../store";
import type { Relationship, RelationshipGraph } from "../types";

describe("readRelationships", () => {
  let tmpDir: string | null = null;

  afterEach(async () => {
    if (tmpDir) {
      await rm(tmpDir, { force: true, recursive: true });
      tmpDir = null;
    }
  });

  it("returns empty graph for missing file", async () => {
    tmpDir = (await Bun.file("/dev/null").exists())
      ? path.join(os.tmpdir(), `legion-rel-${Date.now()}`)
      : os.tmpdir();
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    const graph = await readRelationships(filePath);

    expect(graph).toEqual({ relationships: [] });
  });

  it("reads valid relationship graph", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    const expected: RelationshipGraph = {
      relationships: [
        { parent: "sjawhar-legion-277", child: "sjawhar-legion-317", type: "parent-child" },
      ],
    };
    await writeFile(filePath, JSON.stringify(expected), "utf-8");

    const graph = await readRelationships(filePath);
    expect(graph).toEqual(expected);
  });

  it("returns empty graph for corrupt JSON", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    await writeFile(filePath, "not-valid-json{{{", "utf-8");

    const graph = await readRelationships(filePath);
    expect(graph).toEqual({ relationships: [] });
  });

  it("returns empty graph for invalid schema", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    await writeFile(filePath, JSON.stringify({ wrong: "shape" }), "utf-8");

    const graph = await readRelationships(filePath);
    expect(graph).toEqual({ relationships: [] });
  });

  it("returns empty graph for invalid relationship entries", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    await writeFile(
      filePath,
      JSON.stringify({
        relationships: [{ parent: 123, child: "issue", type: "parent-child" }],
      }),
      "utf-8"
    );

    const graph = await readRelationships(filePath);
    expect(graph).toEqual({ relationships: [] });
  });

  it("returns empty graph for empty file", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);
    await mkdir(tmpDir, { recursive: true });

    const filePath = path.join(tmpDir, "relationships.json");
    await writeFile(filePath, "", "utf-8");

    const graph = await readRelationships(filePath);
    expect(graph).toEqual({ relationships: [] });
  });
});

describe("writeRelationships", () => {
  let tmpDir: string | null = null;

  afterEach(async () => {
    if (tmpDir) {
      await rm(tmpDir, { force: true, recursive: true });
      tmpDir = null;
    }
  });

  it("writes and reads back a relationship graph", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);

    const filePath = path.join(tmpDir, "nested", "relationships.json");
    const graph: RelationshipGraph = {
      relationships: [
        { parent: "sjawhar-legion-277", child: "sjawhar-legion-317", type: "parent-child" },
        { parent: "sjawhar-legion-277", child: "sjawhar-legion-318", type: "parent-child" },
      ],
    };

    await writeRelationships(filePath, graph);
    const result = await readRelationships(filePath);

    expect(result).toEqual(graph);
  });

  it("creates parent directories if they don't exist", async () => {
    tmpDir = path.join(os.tmpdir(), `legion-rel-${Date.now()}`);

    const filePath = path.join(tmpDir, "deep", "nested", "dir", "relationships.json");
    await writeRelationships(filePath, { relationships: [] });

    const result = await readRelationships(filePath);
    expect(result).toEqual({ relationships: [] });
  });
});

describe("mergeRelationships", () => {
  it("merges two arrays and deduplicates", () => {
    const existing: Relationship[] = [
      { parent: "a", child: "b", type: "parent-child" },
      { parent: "a", child: "c", type: "parent-child" },
    ];
    const incoming: Relationship[] = [
      { parent: "a", child: "b", type: "parent-child" }, // duplicate
      { parent: "a", child: "d", type: "parent-child" }, // new
    ];

    const result = mergeRelationships(existing, incoming);

    expect(result).toHaveLength(3);
    expect(result).toEqual([
      { parent: "a", child: "b", type: "parent-child" },
      { parent: "a", child: "c", type: "parent-child" },
      { parent: "a", child: "d", type: "parent-child" },
    ]);
  });

  it("returns empty array when both inputs are empty", () => {
    expect(mergeRelationships([], [])).toEqual([]);
  });

  it("returns existing when incoming is empty", () => {
    const existing: Relationship[] = [{ parent: "a", child: "b", type: "parent-child" }];
    expect(mergeRelationships(existing, [])).toEqual(existing);
  });

  it("returns incoming when existing is empty", () => {
    const incoming: Relationship[] = [{ parent: "a", child: "b", type: "parent-child" }];
    expect(mergeRelationships([], incoming)).toEqual(incoming);
  });

  it("preserves order: existing first, then new from incoming", () => {
    const existing: Relationship[] = [{ parent: "x", child: "y", type: "parent-child" }];
    const incoming: Relationship[] = [{ parent: "a", child: "b", type: "parent-child" }];

    const result = mergeRelationships(existing, incoming);
    expect(result[0]?.parent).toBe("x");
    expect(result[1]?.parent).toBe("a");
  });
});

describe("getRelativesForIssue", () => {
  const graph: RelationshipGraph = {
    relationships: [
      { parent: "sjawhar-legion-277", child: "sjawhar-legion-317", type: "parent-child" },
      { parent: "sjawhar-legion-277", child: "sjawhar-legion-318", type: "parent-child" },
      { parent: "sjawhar-legion-277", child: "sjawhar-legion-319", type: "parent-child" },
      { parent: "sjawhar-legion-100", child: "sjawhar-legion-317", type: "parent-child" },
    ],
  };

  it("returns parents for a child issue", () => {
    const { parents, children } = getRelativesForIssue("sjawhar-legion-317", graph);

    expect(parents).toEqual(["sjawhar-legion-277", "sjawhar-legion-100"]);
    expect(children).toEqual([]);
  });

  it("returns children for a parent issue", () => {
    const { parents, children } = getRelativesForIssue("sjawhar-legion-277", graph);

    expect(parents).toEqual([]);
    expect(children).toEqual(["sjawhar-legion-317", "sjawhar-legion-318", "sjawhar-legion-319"]);
  });

  it("returns empty arrays for unrelated issue", () => {
    const { parents, children } = getRelativesForIssue("sjawhar-legion-999", graph);

    expect(parents).toEqual([]);
    expect(children).toEqual([]);
  });

  it("is case-insensitive for issue ID matching", () => {
    const { parents } = getRelativesForIssue("SJAWHAR-LEGION-317", graph);
    expect(parents).toEqual(["sjawhar-legion-277", "sjawhar-legion-100"]);
  });

  it("handles empty graph", () => {
    const { parents, children } = getRelativesForIssue("any-issue", { relationships: [] });

    expect(parents).toEqual([]);
    expect(children).toEqual([]);
  });
});
