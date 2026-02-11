import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { readStateFile, writeStateFile } from "../state-file";

const sampleState = {
  workers: {
    "eng-42-implement": {
      id: "eng-42-implement",
      port: 14444,
      pid: 9999,
      sessionId: "ses_test",
      startedAt: "2026-01-01T00:00:00.000Z",
      status: "running" as const,
      crashCount: 0,
      lastCrashAt: null,
    },
  },
  crashHistory: {
    "eng-42-implement": { crashCount: 1, lastCrashAt: "2026-01-02T00:00:00.000Z" },
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
    expect(state).toEqual({ workers: {}, crashHistory: {} });
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
