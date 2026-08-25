import { describe, expect, it } from "bun:test";
import type { BuildDependencyGraphOptions } from "../graph";
import {
  buildChangeHotspots,
  type JjCommandResult,
  type JjCommandRunner,
  parseJjLogStatOutput,
} from "../hotspots";

describe("parseJjLogStatOutput", () => {
  it("aggregates file changes and tracks most recent change timestamp", () => {
    const output = [
      "@  qwerty user@example.com 2026-03-28 10:00:00 abcdef12",
      "│  feat: latest commit",
      "│  packages/daemon/src/index/manager.ts | 2 ++",
      "│  packages/daemon/src/index/types.ts   | 4 ++--",
      "│  2 files changed, 4 insertions(+), 2 deletions(-)",
      "◆  asdfgh user@example.com 2026-03-27 09:30:00 98765432",
      "│  feat: older commit",
      "│  packages/daemon/src/index/manager.ts  | 1 +",
      "│  packages/daemon/src/daemon/index.ts   | 3 ++-",
      "│  2 files changed, 3 insertions(+), 1 deletions(-)",
    ].join("\n");

    const hotspots = parseJjLogStatOutput(output);

    expect(hotspots).toEqual([
      {
        filePath: "packages/daemon/src/index/manager.ts",
        changeCount: 2,
        lastChanged: "2026-03-28T10:00:00.000Z",
      },
      {
        filePath: "packages/daemon/src/daemon/index.ts",
        changeCount: 1,
        lastChanged: "2026-03-27T09:30:00.000Z",
      },
      {
        filePath: "packages/daemon/src/index/types.ts",
        changeCount: 1,
        lastChanged: "2026-03-28T10:00:00.000Z",
      },
    ]);
  });

  it("ignores summary lines and lines without file stat data", () => {
    const output = [
      "@  qwerty user@example.com 2026-03-28 10:00:00 abcdef12",
      "│  feat: latest commit",
      "│  0 files changed, 0 insertions(+), 0 deletions(-)",
      "◆  asdfgh user@example.com 2026-03-27 09:30:00 98765432",
      "│  feat: older commit",
      "│  0 files changed, 0 insertions(+), 0 deletions(-)",
    ].join("\n");

    expect(parseJjLogStatOutput(output)).toEqual([]);
  });
});

describe("buildChangeHotspots", () => {
  it("returns parsed hotspots from jj log --stat output", async () => {
    const output = [
      "@  qwerty user@example.com 2026-03-28 10:00:00 abcdef12",
      "│  feat: latest commit",
      "│  packages/daemon/src/index/manager.ts | 2 ++",
      "│  1 file changed, 2 insertions(+), 0 deletions(-)",
    ].join("\n");

    const runner: JjCommandRunner = async (_rootDir: string, _args: string[]) => {
      const result: JjCommandResult = {
        exitCode: 0,
        stdout: output,
        stderr: "",
      };
      return result;
    };

    const hotspots = await buildChangeHotspots("/tmp/workspace", {
      hotspotCommandRunner: runner,
    });

    expect(hotspots).toEqual([
      {
        filePath: "packages/daemon/src/index/manager.ts",
        changeCount: 1,
        lastChanged: "2026-03-28T10:00:00.000Z",
      },
    ]);
  });

  it("returns an empty list when jj command is unavailable", async () => {
    const warns: string[] = [];
    const options: BuildDependencyGraphOptions = {
      warn: (message) => warns.push(message),
      hotspotCommandRunner: async () => {
        throw new Error("spawn ENOENT");
      },
    };

    const hotspots = await buildChangeHotspots("/tmp/workspace", options);

    expect(hotspots).toEqual([]);
    expect(warns).toHaveLength(1);
    expect(warns[0]).toContain("Failed to compute hotspots from jj log");
  });

  it("returns an empty list on non-zero jj exit", async () => {
    const warns: string[] = [];
    const options: BuildDependencyGraphOptions = {
      warn: (message) => warns.push(message),
      hotspotCommandRunner: async () => ({
        exitCode: 1,
        stdout: "",
        stderr: "Error: not inside a jj repository",
      }),
    };

    const hotspots = await buildChangeHotspots("/tmp/workspace", options);

    expect(hotspots).toEqual([]);
    expect(warns).toHaveLength(1);
    expect(warns[0]).toContain("Failed to compute hotspots from jj log");
  });
});
