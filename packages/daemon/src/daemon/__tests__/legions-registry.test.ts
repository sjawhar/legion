import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { findLegionByProjectId, removeLegionEntry, writeLegionEntry } from "../legions-registry";

const directories: string[] = [];

afterEach(async () => {
  await Promise.all(
    directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

describe("legions registry", () => {
  it("records daemon ownership without an obsolete shared-serve port", async () => {
    const directory = await mkdtemp(path.join(os.tmpdir(), "legion-registry-"));
    directories.push(directory);
    const file = path.join(directory, "legions.json");

    await writeLegionEntry(file, "acme/1", {
      port: 13370,
      pid: 12345,
      startedAt: "2026-08-24T00:00:00.000Z",
    });

    expect(await findLegionByProjectId(file, "acme/1")).toEqual({
      port: 13370,
      pid: 12345,
      startedAt: "2026-08-24T00:00:00.000Z",
    });
    await removeLegionEntry(file, "acme/1");
    expect(await findLegionByProjectId(file, "acme/1")).toBeUndefined();
  });
});
