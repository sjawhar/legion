import { afterEach, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { fillNativesCache, linkTree } from "./test-omp-natives";

const cleanup: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const undo of cleanup.splice(0).reverse()) await undo();
});

async function scratch(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "test-omp-natives-"));
  cleanup.push(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

/** A stand-in binary under `dir`: a script carrying the embedded natives list, in the format the
 * real binary carries it, that runs `body`. */
async function standIn(dir: string, list: readonly string[], body: string): Promise<string> {
  const binary = path.join(dir, "omp");
  const header = list.map((line) => `# ${line}\n`).join("");
  await writeFile(binary, `#!/bin/sh\n${header}${body}`, { mode: 0o755 });
  return binary;
}

/** The cache's name for `binary`, as the daemon's tests name it (`testbin.OMPHome`). */
async function cacheDigest(binary: string): Promise<string> {
  const hasher = new Bun.CryptoHasher("sha256").update(await readFile(binary));
  return hasher.digest("hex").slice(0, 32);
}

async function missing(file: string): Promise<boolean> {
  return stat(file).then(
    () => false,
    (error: NodeJS.ErrnoException) => error.code === "ENOENT"
  );
}

// Of eight fills that race for one binary's cache, as the six real-binary cases and a Go test
// binary may, one runs the binary and the rest find its copy, under the name the Go tests give it;
// each HOME then holds that copy's inode rather than bytes of its own, and the copy takes no write
// through a link. The staging directory of a filler that died is gone once the cache is filled.
test("fills racing for one binary's natives cache run it once, and each HOME links the one read-only copy", async () => {
  const dir = await scratch();
  const runs = path.join(dir, "runs");
  const binary = await standIn(
    dir,
    ['{ variant: "default", filename: "pi_natives.test.node", size: 6 },'],
    `echo run >>${runs}\nmkdir -p "$HOME/.omp/natives/9.9.9"\nprintf native >"$HOME/.omp/natives/9.9.9/pi_natives.test.node"\n`
  );
  const root = path.join(dir, "cache");
  const cache = path.join(root, await cacheDigest(binary));
  const dead = `${cache}.fill-dead`;
  await mkdir(path.join(dead, "home"), { recursive: true });

  const caches = await Promise.all(Array.from({ length: 8 }, () => fillNativesCache(root, binary)));

  expect(new Set(caches)).toEqual(new Set([cache]));
  expect(await readFile(runs, "utf8")).toBe("run\n");
  expect(await missing(dead)).toBe(true);
  const cached = await stat(path.join(cache, "9.9.9", "pi_natives.test.node"));
  expect(cached.mode & 0o222).toBe(0);
  for (const home of [path.join(dir, "a"), path.join(dir, "b")]) {
    await linkTree(cache, path.join(home, ".omp", "natives"));
    const linked = await stat(path.join(home, ".omp", "natives", "9.9.9", "pi_natives.test.node"));
    expect([linked.dev, linked.ino]).toEqual([cached.dev, cached.ino]);
  }
});

// As when a full disk fails Oh My Pi's second write and it still exits 0. The stand-in embeds two
// files, writes one on its first run and both after.
test("a fill whose extraction lacks a file the binary embeds caches nothing, and the next complete one is cached", async () => {
  const dir = await scratch();
  const runs = path.join(dir, "runs");
  const binary = await standIn(
    dir,
    [
      '{ variant: "modern", filename: "pi_natives.test-modern.node", size: 6 },',
      '{ variant: "baseline", filename: "pi_natives.test-baseline.node", size: 8 },',
    ],
    `natives="$HOME/.omp/natives/9.9.9"\nmkdir -p "$natives"\nprintf modern >"$natives/pi_natives.test-modern.node"\n` +
      `[ -e ${runs} ] && printf baseline >"$natives/pi_natives.test-baseline.node"\necho run >>${runs}\n`
  );
  const root = path.join(dir, "cache");
  const cache = path.join(root, await cacheDigest(binary));

  await expect(fillNativesCache(root, binary)).rejects.toThrow(
    "embeds pi_natives.test-baseline.node, but its extraction"
  );
  expect(await missing(cache)).toBe(true);
  expect(await fillNativesCache(root, binary)).toBe(cache);
  for (const name of ["pi_natives.test-modern.node", "pi_natives.test-baseline.node"]) {
    expect(await missing(path.join(cache, "9.9.9", name))).toBe(false);
  }
});

// With nothing to check an extraction against, a partial one would be cached.
test("a binary that lists no embedded natives is refused before it runs", async () => {
  const dir = await scratch();
  const runs = path.join(dir, "runs");
  const binary = await standIn(dir, [], `echo run >>${runs}\n`);

  await expect(fillNativesCache(path.join(dir, "cache"), binary)).rejects.toThrow(
    "lists no embedded natives"
  );
  expect(await missing(runs)).toBe(true);
});
