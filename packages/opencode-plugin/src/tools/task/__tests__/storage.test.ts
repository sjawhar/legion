import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { z } from "zod";
import {
  acquireLock,
  ensureDir,
  generateTaskId,
  getTaskDir,
  listTaskFiles,
  readJsonSafe,
  resolveTaskListId,
  sanitizePathSegment,
  writeJsonAtomic,
} from "../storage";

let tempDir: string;

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-storage-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("sanitizePathSegment", () => {
  it("passes alphanumeric with hyphens and underscores", () => {
    expect(sanitizePathSegment("my-task_123")).toBe("my-task_123");
  });

  it("replaces special characters with hyphens", () => {
    expect(sanitizePathSegment("foo/bar:baz")).toBe("foo-bar-baz");
  });

  it("returns default for empty string", () => {
    expect(sanitizePathSegment("")).toBe("default");
  });
});

describe("resolveTaskListId", () => {
  const origEnv = process.env.OPENCODE_TASK_LIST_ID;

  afterEach(() => {
    if (origEnv === undefined) {
      delete process.env.OPENCODE_TASK_LIST_ID;
    } else {
      process.env.OPENCODE_TASK_LIST_ID = origEnv;
    }
  });

  it("uses env var when set", () => {
    process.env.OPENCODE_TASK_LIST_ID = "my-list";
    expect(resolveTaskListId()).toBe("my-list");
  });

  it("falls back to sanitized cwd basename", () => {
    delete process.env.OPENCODE_TASK_LIST_ID;
    const result = resolveTaskListId();
    expect(result).toBeTruthy();
    expect(result).not.toContain("/");
  });
});

describe("getTaskDir", () => {
  it("returns path under ~/.config/opencode/tasks/", () => {
    const dir = getTaskDir("test-list");
    expect(dir).toContain("tasks");
    expect(dir).toContain("test-list");
  });
});

describe("ensureDir", () => {
  it("creates directory recursively", () => {
    const nested = path.join(tempDir, "a", "b", "c");
    ensureDir(nested);
    expect(fs.existsSync(nested)).toBe(true);
  });

  it("no-ops on existing directory", () => {
    ensureDir(tempDir);
    expect(fs.existsSync(tempDir)).toBe(true);
  });
});

describe("readJsonSafe", () => {
  const schema = z.object({ name: z.string() });

  it("returns parsed data on valid JSON", () => {
    const filePath = path.join(tempDir, "test.json");
    fs.writeFileSync(filePath, JSON.stringify({ name: "hello" }));
    expect(readJsonSafe(filePath, schema)).toEqual({ name: "hello" });
  });

  it("returns null for non-existent file", () => {
    expect(readJsonSafe(path.join(tempDir, "nope.json"), schema)).toBeNull();
  });

  it("returns null for invalid schema", () => {
    const filePath = path.join(tempDir, "bad.json");
    fs.writeFileSync(filePath, JSON.stringify({ wrong: 123 }));
    expect(readJsonSafe(filePath, schema)).toBeNull();
  });

  it("returns null for malformed JSON", () => {
    const filePath = path.join(tempDir, "broken.json");
    fs.writeFileSync(filePath, "{not json");
    expect(readJsonSafe(filePath, schema)).toBeNull();
  });
});

describe("writeJsonAtomic", () => {
  it("writes valid JSON file", () => {
    const filePath = path.join(tempDir, "out.json");
    writeJsonAtomic(filePath, { key: "value" });
    const content = JSON.parse(fs.readFileSync(filePath, "utf-8"));
    expect(content).toEqual({ key: "value" });
  });

  it("creates parent directories", () => {
    const filePath = path.join(tempDir, "nested", "deep", "out.json");
    writeJsonAtomic(filePath, { a: 1 });
    expect(fs.existsSync(filePath)).toBe(true);
  });

  it("overwrites existing file atomically", () => {
    const filePath = path.join(tempDir, "overwrite.json");
    writeJsonAtomic(filePath, { version: 1 });
    writeJsonAtomic(filePath, { version: 2 });
    const content = JSON.parse(fs.readFileSync(filePath, "utf-8"));
    expect(content).toEqual({ version: 2 });
  });

  it("cleans up temp file on error", () => {
    const readOnlyDir = path.join(tempDir, "readonly");
    fs.mkdirSync(readOnlyDir);
    const filePath = path.join(readOnlyDir, "test.json");
    writeJsonAtomic(filePath, { first: true });
    fs.chmodSync(readOnlyDir, 0o444);

    try {
      writeJsonAtomic(filePath, { second: true });
    } catch {
      // Expected
    } finally {
      fs.chmodSync(readOnlyDir, 0o755);
    }

    const tmpFiles = fs.readdirSync(readOnlyDir).filter((f) => f.includes(".tmp."));
    expect(tmpFiles.length).toBe(0);
  });
});

describe("generateTaskId", () => {
  it("starts with T-", () => {
    expect(generateTaskId()).toMatch(/^T-/);
  });

  it("generates unique IDs", () => {
    const ids = new Set(Array.from({ length: 100 }, () => generateTaskId()));
    expect(ids.size).toBe(100);
  });
});

describe("listTaskFiles", () => {
  it("returns empty array for non-existent directory", () => {
    expect(listTaskFiles(path.join(tempDir, "nope"))).toEqual([]);
  });

  it("lists only T-*.json files", () => {
    fs.writeFileSync(path.join(tempDir, "T-abc.json"), "{}");
    fs.writeFileSync(path.join(tempDir, "T-def.json"), "{}");
    fs.writeFileSync(path.join(tempDir, ".lock"), "{}");
    fs.writeFileSync(path.join(tempDir, "other.json"), "{}");

    const result = listTaskFiles(tempDir);
    expect(result.sort()).toEqual(["T-abc", "T-def"]);
  });
});

describe("acquireLock", () => {
  it("acquires lock successfully", () => {
    const lock = acquireLock(tempDir);
    expect(lock.acquired).toBe(true);
    lock.release();
  });

  it("fails to acquire when lock exists", () => {
    const lock1 = acquireLock(tempDir);
    expect(lock1.acquired).toBe(true);

    const lock2 = acquireLock(tempDir);
    expect(lock2.acquired).toBe(false);

    lock1.release();
  });

  it("reclaims stale lock", () => {
    const lockPath = path.join(tempDir, ".lock");
    fs.writeFileSync(lockPath, JSON.stringify({ id: "old", timestamp: Date.now() - 60_000 }));

    const lock = acquireLock(tempDir);
    expect(lock.acquired).toBe(true);
    lock.release();
  });

  it("release is safe to call multiple times", () => {
    const lock = acquireLock(tempDir);
    lock.release();
    lock.release();
    expect(true).toBe(true);
  });

  it("release only removes own lock", () => {
    const lock1 = acquireLock(tempDir);
    lock1.release();

    const lock2 = acquireLock(tempDir);
    expect(lock2.acquired).toBe(true);

    lock1.release();
    expect(fs.existsSync(path.join(tempDir, ".lock"))).toBe(true);

    lock2.release();
  });
});
