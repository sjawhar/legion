import { afterEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { buildDependencyGraph, updateDependencyGraphIncremental } from "../graph";

async function writeFixture(rootDir: string, relativePath: string, content: string): Promise<void> {
  const absolutePath = path.join(rootDir, relativePath);
  await mkdir(path.dirname(absolutePath), { recursive: true });
  await writeFile(absolutePath, content, "utf-8");
}

describe("dependency graph test mapping", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { force: true, recursive: true });
      tempDir = null;
    }
  });

  it("maps source files to tests and tests to source files", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-index-test-map-"));

    await writeFixture(tempDir, "src/foo.ts", "export const foo = 1;\n");
    await writeFixture(tempDir, "src/bar.ts", "export const bar = 1;\n");
    await writeFixture(tempDir, "src/no-tests.ts", "export const untouched = true;\n");

    await writeFixture(tempDir, "src/foo.test.ts", "import { foo } from './foo';\nvoid foo;\n");
    await writeFixture(tempDir, "src/foo.spec.ts", "import { foo } from './foo';\nvoid foo;\n");
    await writeFixture(
      tempDir,
      "src/__tests__/bar.ts",
      "import { bar } from '../bar';\nvoid bar;\n"
    );
    await writeFixture(
      tempDir,
      "src/__tests__/integration.ts",
      "describe('integration', () => {});\n"
    );

    const index = await buildDependencyGraph(tempDir);

    expect(index.testMapping.sourceToTests).toEqual({
      "src/bar.ts": ["src/__tests__/bar.ts"],
      "src/foo.ts": ["src/foo.spec.ts", "src/foo.test.ts"],
      "src/no-tests.ts": [],
    });
    expect(index.testMapping.testToSources).toEqual({
      "src/__tests__/bar.ts": ["src/bar.ts"],
      "src/__tests__/integration.ts": [],
      "src/foo.spec.ts": ["src/foo.ts"],
      "src/foo.test.ts": ["src/foo.ts"],
    });
  });

  it("reuses unchanged test file mappings during incremental update", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-index-test-map-"));

    await writeFixture(tempDir, "src/alpha.ts", "export const alpha = 1;\n");
    await writeFixture(tempDir, "src/beta.ts", "export const beta = 1;\n");
    await writeFixture(
      tempDir,
      "src/changed.test.ts",
      "import { alpha } from './alpha';\nvoid alpha;\n"
    );
    await writeFixture(tempDir, "src/unchanged.test.ts", "import './missing';\n");

    const initialWarnings: string[] = [];
    const initial = await buildDependencyGraph(tempDir, {
      warn: (message) => initialWarnings.push(message),
    });
    expect(initialWarnings).toHaveLength(1);

    await Bun.sleep(20);
    await writeFixture(
      tempDir,
      "src/changed.test.ts",
      "import { beta } from './beta';\nvoid beta;\n"
    );

    const incrementalWarnings: string[] = [];
    const updated = await updateDependencyGraphIncremental(initial, {
      warn: (message) => incrementalWarnings.push(message),
    });

    expect(incrementalWarnings).toEqual([]);
    expect(updated.testMapping.sourceToTests).toEqual({
      "src/alpha.ts": [],
      "src/beta.ts": ["src/changed.test.ts"],
    });
    expect(updated.testMapping.testToSources).toEqual({
      "src/changed.test.ts": ["src/beta.ts"],
      "src/unchanged.test.ts": [],
    });
  });
});
