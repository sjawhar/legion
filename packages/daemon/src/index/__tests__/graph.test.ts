import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { buildDependencyGraph, updateDependencyGraphIncremental } from "../graph";

const tempDirs: string[] = [];

async function makeTempRoot(): Promise<string> {
  const root = await mkdtemp(path.join(tmpdir(), "legion-index-"));
  tempDirs.push(root);
  return root;
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("dependency graph API surface integration", () => {
  it("populates apiSurface for every source file, including empty arrays", async () => {
    const root = await makeTempRoot();
    await writeFile(path.join(root, "a.ts"), "export function alpha() {}\n", "utf-8");
    await writeFile(path.join(root, "b.ts"), "const localOnly = 1;\n", "utf-8");

    const index = await buildDependencyGraph(root);

    expect(Object.keys(index.apiSurface).sort()).toEqual(["a.ts", "b.ts"]);
    expect(index.apiSurface["a.ts"]).toEqual([
      {
        name: "alpha",
        kind: "function",
        signature: "export function alpha() {}",
      },
    ]);
    expect(index.apiSurface["b.ts"]).toEqual([]);
  });

  it("re-extracts apiSurface for changed files during incremental updates", async () => {
    const root = await makeTempRoot();
    const changedFile = path.join(root, "changed.ts");
    const stableFile = path.join(root, "stable.ts");

    await writeFile(changedFile, "export const originalName = 1;\n", "utf-8");
    await writeFile(stableFile, "export const stableName = 1;\n", "utf-8");

    const initial = await buildDependencyGraph(root);
    await Bun.sleep(5);
    await writeFile(changedFile, "export const updatedName = 2;\n", "utf-8");

    const updated = await updateDependencyGraphIncremental(initial);

    expect(updated.apiSurface["changed.ts"]).toEqual([
      {
        name: "updatedName",
        kind: "const",
        signature: "export const updatedName = 2;",
      },
    ]);
    expect(updated.apiSurface["stable.ts"]).toEqual([
      {
        name: "stableName",
        kind: "const",
        signature: "export const stableName = 1;",
      },
    ]);
  });
});
