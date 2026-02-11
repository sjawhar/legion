import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { acquireLock } from "../storage";

let tempDir: string;

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-lock-regression-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task lock regressions (concurrency hazards)", () => {
  it("does not allow stale-lock reclaim while the original holder is still in a critical section", () => {
    const lock1 = acquireLock(tempDir);
    expect(lock1.acquired).toBe(true);
    const lockPath = path.join(tempDir, ".lock");
    const current = JSON.parse(fs.readFileSync(lockPath, "utf-8")) as {
      id: string;
      timestamp: number;
    };
    fs.writeFileSync(
      lockPath,
      JSON.stringify({ ...current, timestamp: Date.now() - 60_000 }),
      "utf-8"
    );

    const lock2 = acquireLock(tempDir);
    expect(lock2.acquired).toBe(false);

    lock1.release();
    lock2.release();
  });

  it("reclaims a stale lock when the holding process is dead", () => {
    const lock1 = acquireLock(tempDir);
    expect(lock1.acquired).toBe(true);
    const lockPath = path.join(tempDir, ".lock");

    const lockContent = JSON.parse(fs.readFileSync(lockPath, "utf-8"));
    fs.writeFileSync(
      lockPath,
      JSON.stringify({
        ...lockContent,
        timestamp: Date.now() - 60_000,
        pid: 2_147_483_647,
        startTime: "99999999999",
      }),
      "utf-8"
    );

    const lock2 = acquireLock(tempDir);
    expect(lock2.acquired).toBe(true);

    lock1.release();
    lock2.release();
  });
});
