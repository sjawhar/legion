import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { afterEach, describe, expect, it } from "bun:test";

import { readLearningFrontMatter, setLearningStatus } from "../front-matter";

const tempDirs: string[] = [];

async function makeTempDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "knowledge-front-matter-"));
  tempDirs.push(dir);
  return dir;
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

describe("readLearningFrontMatter", () => {
  it("reads status and date from YAML front matter", async () => {
    const dir = await makeTempDir();
    const filePath = path.join(dir, "learning.md");

    await writeFile(
      filePath,
      ["---", "status: active", "date: 2026-04-11", "title: Example", "---", "", "# Example"].join(
        "\n"
      )
    );

    const frontMatter = await readLearningFrontMatter(filePath);

    expect(frontMatter).toEqual({
      date: "2026-04-11",
      status: "active",
    });
  });
});

describe("setLearningStatus", () => {
  it("updates the status line in place", async () => {
    const dir = await makeTempDir();
    const filePath = path.join(dir, "learning.md");

    await writeFile(
      filePath,
      ["---", "status: active", "date: 2026-04-11", "---", "", "Body"].join("\n")
    );

    const changed = await setLearningStatus(filePath, "superseded");
    const contents = await readFile(filePath, "utf-8");

    expect(changed).toBe(true);
    expect(contents).toContain("status: superseded");
  });

  it("returns false when front matter is missing", async () => {
    const dir = await makeTempDir();
    const filePath = path.join(dir, "learning.md");

    await writeFile(filePath, "# No front matter\n");

    const changed = await setLearningStatus(filePath, "superseded");

    expect(changed).toBe(false);
  });
});
