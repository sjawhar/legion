import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { CodebaseIndexManager } from "../manager";

const tempDirs: string[] = [];

async function makeTempRoot(): Promise<string> {
  const root = await mkdtemp(path.join(tmpdir(), "legion-index-manager-"));
  tempDirs.push(root);
  return root;
}

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("CodebaseIndexManager hotspots integration", () => {
  it("recomputes hotspots during rebuild and incremental update", async () => {
    const root = await makeTempRoot();
    await writeFile(path.join(root, "a.ts"), "export const value = 1;\n", "utf-8");

    let runnerCalls = 0;
    const hotspotOutput = [
      "@  qwerty user@example.com 2026-03-28 10:00:00 abcdef12",
      "│  feat: latest commit",
      "│  a.ts | 2 ++",
      "│  1 file changed, 2 insertions(+), 0 deletions(-)",
    ].join("\n");

    const manager = new CodebaseIndexManager(root, path.join(root, ".legion", "index.json"), {
      hotspotCommandRunner: async () => {
        runnerCalls += 1;
        return {
          exitCode: 0,
          stdout: hotspotOutput,
          stderr: "",
        };
      },
    });

    const rebuilt = await manager.rebuild();
    expect(rebuilt.hotspots).toEqual([
      {
        filePath: "a.ts",
        changeCount: 1,
        lastChanged: "2026-03-28T10:00:00.000Z",
      },
    ]);

    await manager.incrementalUpdate();
    expect(runnerCalls).toBe(2);
    expect(manager.getResponse()).toMatchObject({
      hotspots: [
        {
          filePath: "a.ts",
          changeCount: 1,
        },
      ],
    });
  });
});
