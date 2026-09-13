import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { chmod, mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { installWorkerGhShim, workerBinDir } from "../worker-bin";

let stateDir: string;

beforeEach(async () => {
  stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-worker-bin-"));
});

afterEach(async () => {
  await rm(stateDir, { recursive: true, force: true });
});

describe("installWorkerGhShim", () => {
  it("installs <stateDir>/worker-bin/gh as a 0700 script inside a 0700 directory", async () => {
    const workerBin = await installWorkerGhShim(stateDir);

    expect(workerBin).toBe(workerBinDir(stateDir));
    expect(workerBin).toBe(path.join(stateDir, "worker-bin"));
    expect((await stat(workerBin)).mode & 0o777).toBe(0o700);
    expect((await stat(path.join(workerBin, "gh"))).mode & 0o777).toBe(0o700);
  });

  it("execs `legion gh -- <argv>` with its own directory stripped from PATH", async () => {
    const workerBin = await installWorkerGhShim(stateDir);
    // A stand-in `legion` further down PATH: prints the PATH it inherited and its argv, so the
    // test observes exactly what a real `legion` would receive from the shim.
    const fakeBin = path.join(stateDir, "fake-bin");
    await mkdir(fakeBin);
    const fakeLegion = path.join(fakeBin, "legion");
    await writeFile(
      fakeLegion,
      `#!/bin/sh\nprintf 'PATH=%s\\n' "$PATH"\nprintf 'ARGS=%s\\n' "$*"\n`
    );
    await chmod(fakeLegion, 0o700);

    const proc = Bun.spawn(["sh", "-c", "gh pr view 7"], {
      env: { PATH: `${workerBin}${path.delimiter}${fakeBin}${path.delimiter}/usr/bin` },
      stdout: "pipe",
      stderr: "pipe",
    });
    const stdout = await new Response(proc.stdout).text();
    const stderr = await new Response(proc.stderr).text();
    expect(await proc.exited).toBe(0);
    expect(stderr).toBe("");
    expect(stdout).toBe(`PATH=${fakeBin}${path.delimiter}/usr/bin\nARGS=gh -- pr view 7\n`);
  });
});
