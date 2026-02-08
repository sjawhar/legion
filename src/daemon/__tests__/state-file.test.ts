import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { readStateFile, writeStateFile } from "../state-file";

const sampleState = {
  "ENG-42-implement": {
    id: "ENG-42-implement",
    port: 14444,
    pid: 9999,
    sessionId: "ses_test",
    startedAt: "2026-01-01T00:00:00.000Z",
    status: "running" as const,
  },
};

describe("state-file", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns empty state when file missing", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const state = await readStateFile(path.join(tempDir, "workers.json"));
    expect(state).toEqual({});
  });

  it("writes and reads state file roundtrip", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    await writeStateFile(filePath, sampleState);
    const state = await readStateFile(filePath);

    expect(state).toEqual(sampleState);
  });

  it("writes atomically without leaving temp files", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    const filePath = path.join(tempDir, "workers.json");

    await writeStateFile(filePath, sampleState);
    const entries = await readdir(tempDir);

    expect(entries).toEqual(["workers.json"]);
  });
});
