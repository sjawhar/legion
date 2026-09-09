import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { acquireInstanceLock } from "../instance-lock";

// Guaranteed never to be a real pid on Linux (default kernel.pid_max is
// 4_194_304), so `process.kill` reliably reports it as not alive (ESRCH).
const DEAD_PID = 999_999_999;

describe("acquireInstanceLock", () => {
  let tempDir: string | undefined;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = undefined;
    }
  });

  it("acquires the lock when none exists, writing this process's pid", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("release() removes the lock file so a later acquisition succeeds", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));

    const lock = await acquireInstanceLock(tempDir);
    await lock.release();
    const second = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await second.release();
  });

  it("refuses a second instance while the lock's owning pid is still alive, naming the pid", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), String(process.pid), "utf8");

    await expect(acquireInstanceLock(tempDir)).rejects.toThrow(
      `Legion daemon already running for this project (pid ${process.pid}`
    );
  });

  it("removes a stale lock left by a dead pid and acquires successfully", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), String(DEAD_PID), "utf8");

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("removes a lock file with unparseable content as stale and acquires successfully", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), "not-a-pid", "utf8");

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("resolves exactly one winner when two contenders race the same stale lock", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), String(DEAD_PID), "utf8");

    // Both see the same stale (dead-pid) lock and race to reclaim it via
    // the atomic rename step; exactly one should end up holding it (the
    // other's rename loses the race, retries `wx`, and — since both
    // contenders run under this same test process's pid — correctly sees
    // the winner's live lock and refuses).
    const results = await Promise.allSettled([
      acquireInstanceLock(tempDir),
      acquireInstanceLock(tempDir),
    ]);

    const fulfilled = results.filter(
      (result): result is PromiseFulfilledResult<Awaited<ReturnType<typeof acquireInstanceLock>>> =>
        result.status === "fulfilled"
    );
    const rejected = results.filter((result) => result.status === "rejected");
    expect(fulfilled).toHaveLength(1);
    expect(rejected).toHaveLength(1);
    expect((rejected[0] as PromiseRejectedResult).reason).toBeInstanceOf(Error);
    expect((rejected[0] as PromiseRejectedResult).reason.message).toContain(`pid ${process.pid}`);
    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));

    await fulfilled[0]?.value.release();
  });

  it("release() does not remove a lock that was reclaimed by another contender in the meantime", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lock = await acquireInstanceLock(tempDir);
    // Simulate this process's lock being reclaimed as stale by another
    // daemon after some external check (e.g. a monitoring tool) mistakenly
    // decided this process was dead, and a new daemon started in its place.
    await writeFile(path.join(tempDir, "daemon.lock"), "999999998", "utf8");

    await lock.release();

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe("999999998");
  });
});
