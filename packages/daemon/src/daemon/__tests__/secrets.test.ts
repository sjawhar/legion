import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { chmod, mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  DISPATCH_TOKEN_SECRET,
  pruneSecretFiles,
  secretFilePath,
  secretsDir,
  writeSecretFile,
} from "../secrets";

let stateDir: string;

beforeEach(async () => {
  stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
});

afterEach(async () => {
  await rm(stateDir, { recursive: true, force: true });
});

describe("writeSecretFile", () => {
  it("creates <stateDir>/secrets as 0700 and the file as 0600 holding exactly the value", async () => {
    const file = await writeSecretFile(stateDir, "legion-omp-controller", "controller-secret");

    expect(file).toBe(secretFilePath(stateDir, "legion-omp-controller"));
    expect((await stat(secretsDir(stateDir))).mode & 0o777).toBe(0o700);
    expect((await stat(file)).mode & 0o777).toBe(0o600);
    expect(await readFile(file, "utf8")).toBe("controller-secret");
  });

  it("overwrites an existing file in place, restoring 0600 even if the mode had drifted", async () => {
    const file = await writeSecretFile(stateDir, "legion-omp-controller", "first");
    await writeFile(file, "tampered", { mode: 0o644 });
    await chmod(file, 0o644);

    await writeSecretFile(stateDir, "legion-omp-controller", "second");

    expect(await readFile(file, "utf8")).toBe("second");
    expect((await stat(file)).mode & 0o777).toBe(0o600);
  });
});

describe("pruneSecretFiles", () => {
  it("removes every file not in keep and reports the removed and the kept names", async () => {
    await writeSecretFile(stateDir, DISPATCH_TOKEN_SECRET, "dispatch");
    await writeSecretFile(stateDir, "legion-omp-legion-42-architect", "root");
    await writeSecretFile(stateDir, "legion-omp-legion-43-tester", "stale");

    const result = await pruneSecretFiles(
      stateDir,
      new Set([DISPATCH_TOKEN_SECRET, "legion-omp-legion-42-architect", "never-written"])
    );

    expect(result).toEqual({
      removed: ["legion-omp-legion-43-tester"],
      kept: [DISPATCH_TOKEN_SECRET, "legion-omp-legion-42-architect"],
    });
    expect((await readdir(secretsDir(stateDir))).sort()).toEqual(
      [DISPATCH_TOKEN_SECRET, "legion-omp-legion-42-architect"].sort()
    );
  });

  it("is a no-op when the secrets directory does not exist yet", async () => {
    expect(await pruneSecretFiles(stateDir, new Set())).toEqual({ removed: [], kept: [] });
  });
});
