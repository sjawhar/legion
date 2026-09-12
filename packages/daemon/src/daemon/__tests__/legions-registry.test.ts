import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, stat } from "node:fs/promises";
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

  it("creates the registry directory on first use and serializes concurrent writers into it", async () => {
    // `legion start` on a fresh XDG_STATE_HOME: `<state>/legion/` does not exist yet, and two
    // daemons for different projects may register at the same moment.
    const directory = await mkdtemp(path.join(os.tmpdir(), "legion-registry-"));
    directories.push(directory);
    const file = path.join(directory, "fresh", "legion", "legions.json");
    const entry = (port: number) => ({ port, pid: 12345, startedAt: "2026-08-24T00:00:00.000Z" });

    await Promise.all([
      writeLegionEntry(file, "acme/1", entry(13370)),
      writeLegionEntry(file, "acme/2", entry(13380)),
    ]);

    expect(await stat(file)).toBeTruthy();
    expect(await findLegionByProjectId(file, "acme/1")).toEqual(entry(13370));
    expect(await findLegionByProjectId(file, "acme/2")).toEqual(entry(13380));
    // The lock was released: a follow-up write neither waits out the 3 s acquisition window nor fails.
    await removeLegionEntry(file, "acme/1");
    expect(await findLegionByProjectId(file, "acme/1")).toBeUndefined();
  });
});
