import { describe, expect, it } from "bun:test";
import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

async function sourceFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { recursive: true });
  return entries
    .filter((entry) => entry.endsWith(".ts"))
    .map((entry) => path.join(directory, entry));
}

describe("real tmux E2E isolation", () => {
  it("never tears down a tmux server across the daemon source tree", async () => {
    const sourceRoot = path.resolve(import.meta.dir, "..", "..");
    const serverTeardown = new RegExp(`["']kill${"-"}server["']`);
    const matches = (
      await Promise.all(
        (
          await sourceFiles(sourceRoot)
        ).map(async (file) =>
          serverTeardown.test(await readFile(file, "utf8"))
            ? path.relative(sourceRoot, file)
            : undefined
        )
      )
    ).filter((file): file is string => file !== undefined);

    expect(matches).toEqual([]);
  });
});
