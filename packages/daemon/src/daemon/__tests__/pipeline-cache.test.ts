import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { readPipelineCache, STALE_THRESHOLD_MS, writePipelineCache } from "../pipeline-cache";

const sampleCollectedState = {
  issues: {
    "eng-42": {
      status: "In Progress",
      labels: ["worker-done"],
      hasPr: true,
      prIsDraft: true,
      ciStatus: "passing",
      mergeableStatus: "mergeable",
      hasLiveWorker: false,
      workerMode: "implement",
      workerStatus: "running",
      suggestedAction: "dispatch_tester",
      sessionId: "ses_test123456abcdef",
      hasUserFeedback: false,
      isBlocked: false,
      source: null,
    },
  },
};

describe("pipeline-cache", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns null when file missing", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const result = await readPipelineCache(path.join(tempDir, "pipeline-cache.json"));
    expect(result).toBeNull();
  });

  it("writes and reads cache roundtrip", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writePipelineCache(filePath, sampleCollectedState);
    const result = await readPipelineCache(filePath);

    // biome-ignore lint/style/noNonNullAssertion: result is asserted non-null above
    const r = result!;
    expect(r.issues).toEqual(sampleCollectedState.issues);
    expect(r.collectedAt).toBeDefined();
    expect(typeof r.collectedAt).toBe("string");
    // Should be a valid ISO timestamp
    expect(new Date(r.collectedAt).toISOString()).toBe(r.collectedAt);
  });

  it("includes stale boolean that is false when fresh", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writePipelineCache(filePath, sampleCollectedState);
    const result = await readPipelineCache(filePath);

    expect(result).not.toBeNull();
    expect(result?.stale).toBe(false);
  });

  it("includes stale boolean that is true when older than threshold", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    // Write cache with an old timestamp
    const oldTimestamp = new Date(Date.now() - STALE_THRESHOLD_MS - 1000).toISOString();
    const cached = {
      collectedAt: oldTimestamp,
      issues: sampleCollectedState.issues,
    };
    await writeFile(filePath, JSON.stringify(cached, null, 2), "utf-8");

    const result = await readPipelineCache(filePath);

    expect(result).not.toBeNull();
    expect(result?.stale).toBe(true);
  });

  it("writes atomically without leaving temp files", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writePipelineCache(filePath, sampleCollectedState);
    const entries = await readdir(tempDir);

    expect(entries).toEqual(["pipeline-cache.json"]);
  });

  it("recovers from corrupt JSON by moving to .corrupt file", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writeFile(filePath, "NOT VALID JSON{{{");
    const result = await readPipelineCache(filePath);

    expect(result).toBeNull();

    // Original file should be renamed for debugging
    const entries = await readdir(tempDir);
    const corruptFiles = entries.filter((e) => e.includes(".corrupt."));
    expect(corruptFiles.length).toBe(1);
  });

  it("recovers from schema-invalid JSON", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writeFile(filePath, JSON.stringify({ notTheRightShape: true }));
    const result = await readPipelineCache(filePath);

    expect(result).toBeNull();

    const entries = await readdir(tempDir);
    const corruptFiles = entries.filter((e) => e.includes(".corrupt."));
    expect(corruptFiles.length).toBe(1);
  });

  it("recovers from empty file", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    await writeFile(filePath, "");
    const result = await readPipelineCache(filePath);

    expect(result).toBeNull();
  });

  it("stays under 1MB with 50 tracked issues", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-cache-"));
    const filePath = path.join(tempDir, "pipeline-cache.json");

    // Build a synthetic state with 50 issues
    const issues: Record<string, unknown> = {};
    for (let i = 1; i <= 50; i++) {
      issues[`sjawhar-legion-${i}`] = {
        status: "In Progress",
        labels: ["worker-done", "worker-active", "test-passed", `custom-label-${i}`],
        hasPr: true,
        prIsDraft: i % 3 === 0,
        ciStatus: i % 4 === 0 ? "failing" : "passing",
        mergeableStatus: "mergeable",
        hasLiveWorker: i % 2 === 0,
        workerMode: "implement",
        workerStatus: "running",
        suggestedAction: "dispatch_tester",
        sessionId: `ses_${String(i).padStart(12, "0")}abcdefghijklmn`,
        hasUserFeedback: false,
        isBlocked: false,
        source: {
          owner: "sjawhar",
          repo: "legion",
          number: i,
          url: `https://github.com/sjawhar/legion/issues/${i}`,
        },
      };
    }

    await writePipelineCache(filePath, { issues });

    const { size } = await Bun.file(filePath).stat();
    expect(size).toBeLessThan(1_000_000); // Under 1MB
  });
});
