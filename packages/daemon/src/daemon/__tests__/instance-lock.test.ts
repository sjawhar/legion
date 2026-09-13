import { afterEach, describe, expect, it } from "bun:test";
import { readdirSync, readFileSync, readlinkSync, realpathSync } from "node:fs";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { acquireInstanceLock, type InstanceLock } from "../instance-lock";

// Guaranteed never to be a real pid on Linux (default kernel.pid_max is
// 4_194_304), so `process.kill` reliably reports it as not alive (ESRCH).
const DEAD_PID = 999_999_999;

const HOLDER_FIXTURE = path.join(import.meta.dir, "fixtures", "lock-holder.ts");

describe("acquireInstanceLock", () => {
  let tempDir: string | undefined;
  const children: Bun.Subprocess[] = [];

  afterEach(async () => {
    for (const child of children.splice(0)) {
      child.kill("SIGKILL");
      await child.exited;
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = undefined;
    }
  });

  /** Reads `child`'s stdout up to its first newline; a child that exits first is the error. */
  async function firstLine(
    child: { stdout: ReadableStream<Uint8Array>; exited: Promise<number> },
    role: string
  ): Promise<string> {
    const reader = child.stdout.getReader();
    let text = "";
    while (!text.includes("\n")) {
      const { value, done } = await reader.read();
      if (done) throw new Error(`${role} exited before reporting (exit ${await child.exited})`);
      text += new TextDecoder().decode(value);
    }
    return text.slice(0, text.indexOf("\n")).trim();
  }

  /** Starts the fixture holding the lock on `stateDir` and returns once it reports "locked". */
  async function spawnHolder(
    stateDir: string,
    ...args: string[]
  ): Promise<Bun.Subprocess<"pipe", "pipe", "inherit">> {
    const child = Bun.spawn([process.execPath, HOLDER_FIXTURE, stateDir, ...args], {
      stdin: "pipe",
      stdout: "pipe",
      stderr: "inherit",
    });
    children.push(child);
    expect(await firstLine(child, "lock holder")).toBe("locked");
    return child;
  }

  it("acquires the lock when none exists, writing this process's pid", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("release() frees the lock for a later acquisition and leaves the file in place", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");

    const lock = await acquireInstanceLock(tempDir);
    const { ino } = await stat(lockFile);
    await lock.release();
    expect((await stat(lockFile)).ino).toBe(ino);
    const second = await acquireInstanceLock(tempDir);

    expect(await readFile(lockFile, "utf8")).toBe(String(process.pid));
    expect((await stat(lockFile)).ino).toBe(ino);
    await second.release();
  });

  it("release() is idempotent", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));

    const lock = await acquireInstanceLock(tempDir);
    await lock.release();
    await lock.release();

    const again = await acquireInstanceLock(tempDir);
    await again.release();
  });

  it("refuses a second attempt inside the holding process, naming this process's pid", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");

    const lock = await acquireInstanceLock(tempDir);
    await expect(acquireInstanceLock(tempDir)).rejects.toThrow(
      `Legion daemon already running for this project (pid ${process.pid}, lock file ${lockFile})`
    );
    await lock.release();
  });

  it("takes over an unlocked file naming a dead pid, overwriting its text", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), String(DEAD_PID), "utf8");

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("takes over an unlocked file with unparseable content", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), "not-a-pid", "utf8");

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("takes over an unlocked file even when its text names a live pid: the lock is the truth, not the text", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    await writeFile(path.join(tempDir, "daemon.lock"), String(process.ppid), "utf8");

    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(path.join(tempDir, "daemon.lock"), "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("resolves exactly one winner when three contenders race a stale lock, never replacing the file", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");
    await writeFile(lockFile, String(DEAD_PID), "utf8");
    const { ino } = await stat(lockFile);

    // Every round starts from a dead daemon's leftover text in the same, unlocked file. The kernel
    // lock is per open file description, so contenders inside one process conflict exactly as
    // separate daemons would. The third contender is the one the old takeover window admitted.
    for (let round = 0; round < 25; round += 1) {
      await writeFile(lockFile, String(DEAD_PID), "utf8");
      const results = await Promise.allSettled([
        acquireInstanceLock(tempDir),
        acquireInstanceLock(tempDir),
        acquireInstanceLock(tempDir),
      ]);

      const fulfilled = results.filter(
        (result): result is PromiseFulfilledResult<InstanceLock> => result.status === "fulfilled"
      );
      const rejected = results.filter(
        (result): result is PromiseRejectedResult => result.status === "rejected"
      );
      expect(fulfilled).toHaveLength(1);
      expect(rejected).toHaveLength(2);
      for (const { reason } of rejected) {
        expect(reason).toBeInstanceOf(Error);
        expect((reason as Error).message).toBe(
          `Legion daemon already running for this project (pid ${process.pid}, lock file ${lockFile})`
        );
      }
      expect(await readFile(lockFile, "utf8")).toBe(String(process.pid));
      expect((await stat(lockFile)).ino).toBe(ino);

      await fulfilled[0]?.value.release();
      expect((await stat(lockFile)).ino).toBe(ino);
    }
  });

  it("refuses while another process holds the lock, naming its pid, and acquires once that process is killed", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");
    const holder = await spawnHolder(tempDir);

    await expect(acquireInstanceLock(tempDir)).rejects.toThrow(
      `Legion daemon already running for this project (pid ${holder.pid}, lock file ${lockFile})`
    );

    holder.kill("SIGKILL");
    await holder.exited;
    const lock = await acquireInstanceLock(tempDir);

    expect(await readFile(lockFile, "utf8")).toBe(String(process.pid));
    await lock.release();
  });

  it("waits for a holder that has not recorded its pid yet, then names it", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");
    const holder = await spawnHolder(tempDir, "--raw");

    const attempt = acquireInstanceLock(tempDir);
    await sleep(100);
    holder.stdin.write("go\n");
    await holder.stdin.flush();

    await expect(attempt).rejects.toThrow(
      `Legion daemon already running for this project (pid ${holder.pid}, lock file ${lockFile})`
    );
  });

  it("refuses naming the lock file when the holder never records its pid", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(tempDir, "daemon.lock");
    await spawnHolder(tempDir, "--raw");

    await expect(acquireInstanceLock(tempDir)).rejects.toThrow(
      `Legion daemon already running for this project (holder pid not recorded yet, lock file ${lockFile})`
    );
  });

  it("is never inherited by a spawned process: the descriptor is close-on-exec", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-lock-"));
    const lockFile = path.join(realpathSync(tempDir), "daemon.lock");

    const lock = await acquireInstanceLock(tempDir);
    if (process.platform === "linux") {
      const held = readdirSync("/proc/self/fd").filter((fd) => {
        try {
          return readlinkSync(`/proc/self/fd/${fd}`) === lockFile;
        } catch {
          return false;
        }
      });
      expect(held).toHaveLength(1);
      const [, flagsOctal = "0"] =
        /flags:\s+(\d+)/.exec(readFileSync(`/proc/self/fdinfo/${held[0]}`, "utf8")) ?? [];
      expect(Number.parseInt(flagsOctal, 8) & 0o2000000).not.toBe(0);
    }
    // vfork window: Bun.spawn returns before the child's exec has closed its inherited descriptors,
    // and flock is per open file description — `sh` printing proves its exec completed.
    const child = Bun.spawn(["sh", "-c", "echo ready; exec sleep 30"], {
      stdout: "pipe",
      stderr: "ignore",
    });
    children.push(child);
    expect(await firstLine(child, "spawned child")).toBe("ready");
    await lock.release();

    // An inherited descriptor would keep the kernel lock held while `sleep` lives.
    const again = await acquireInstanceLock(tempDir);
    await again.release();
  });
});
