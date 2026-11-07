/**
 * Builds `proof-sdk-<commit>.tgz`, the tarball committed beside `package.json`, which installs
 * `proof-sdk-upstream` from it.
 *
 * The dependency used to be `github:sjawhar/proof-sdk#<commit>`, so every `bun install` — every
 * CI job's and every image build's — fetched 31 MB from api.github.com, and a bad minute there
 * failed jobs that read nothing else of GitHub. The tarball holds the part of that tree this
 * package resolves against: the commit's `src/`, its `package.json` and its `LICENSE`.
 *
 *   bun scripts/build-upstream-tarball.ts <commit>           # write packages/proof-editor/proof-sdk-<commit>.tgz
 *   bun scripts/build-upstream-tarball.ts <commit> <output>  # write it somewhere else
 *
 * The two halves are split because only the second one can be tested: a test that fetched those
 * 31 MB from api.github.com would put back in CI the exact network dependency this tarball
 * removes. `fetchUpstream` is the network half, and nothing tests it; `packUpstream` is a pure
 * function of a source directory, and `tests/build-upstream-tarball.test.ts` runs it over a
 * fixture tree of its own.
 *
 * The bytes are reproducible: the member list is sorted here rather than left to readdir order,
 * every entry carries the commit's author date as its mtime and `0:0` as its numeric ownership,
 * and `gzip -n` writes no timestamp. So two runs over one source tree give one file, and a
 * rebuild of a pin already committed changes nothing.
 *
 * Its scratch trees go under `node:os`'s `tmpdir()`, which reads `$TMPDIR`: the issue volume's,
 * where the environment sets one, since an upstream tarball is 31 MB and a container's own disk
 * is small.
 */
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, relative, resolve, sep } from "node:path";

const REPOSITORY = "sjawhar/proof-sdk";
const API = `https://api.github.com/repos/${REPOSITORY}`;
/** api.github.com refuses a request with no user agent. */
const HEADERS = {
  "user-agent": `${REPOSITORY} tarball builder`,
  accept: "application/vnd.github+json",
};

/** What `packUpstream` writes and where it reads it from. */
export interface PackOptions {
  /** The upstream tree: the directory holding `src/`, `package.json` and `LICENSE`. */
  sourceDir: string;
  /** The mtime of every entry, a date GNU tar parses (the commit's author date). */
  mtime: string;
  /** The `.tgz` to write. */
  outputPath: string;
}

/** The upstream tree a commit's tarball extracted to, and the date its entries are stamped with. */
interface Upstream {
  sourceDir: string;
  mtime: string;
}

function run(command: string, args: string[], input = ""): void {
  const result = spawnSync(command, args, { input, stdio: ["pipe", "inherit", "inherit"] });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} exited ${result.status}`);
}

/**
 * The members of the archive, relative to the source directory and sorted by name: every file
 * under `src/`, then `package.json` and `LICENSE`. Nothing else of the upstream tree is published
 * — its tests, its config and its own dependencies' manifests are not what this package imports.
 */
function members(sourceDir: string): string[] {
  const names: string[] = [];
  const walk = (dir: string): void => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) walk(path);
      else if (entry.isFile()) names.push(relative(sourceDir, path).split(sep).join("/"));
      else
        throw new Error(`${path} is neither a file nor a directory, and the archive holds files`);
    }
  };
  walk(join(sourceDir, "src"));
  for (const name of ["package.json", "LICENSE"]) {
    if (!statSync(join(sourceDir, name)).isFile()) {
      throw new Error(`${join(sourceDir, name)} is not a file`);
    }
    names.push(name);
  }
  return names.sort();
}

/**
 * Writes the gzipped tar: `package/src/**`, `package/package.json` and `package/LICENSE`, in that
 * sorted order and nothing else, each entry stamped with `mtime` and owned by `0:0`.
 *
 * tar is handed the member list rather than the directory, so the order is this process's sorted
 * one rather than the filesystem's, and no directory entry, no stray file beside the three and no
 * `.` entry can reach the archive. `--format=ustar` carries no atime or ctime, and `gzip -n` no
 * name or timestamp, so the only dates in the output are the ones passed in.
 */
export function packUpstream({ sourceDir, mtime, outputPath }: PackOptions): void {
  const names = members(sourceDir);
  const workDir = mkdtempSync(join(tmpdir(), "proof-sdk-pack-"));
  try {
    const tarPath = join(workDir, "upstream.tar");
    run(
      "tar",
      [
        "--create",
        `--file=${tarPath}`,
        `--directory=${sourceDir}`,
        "--no-recursion",
        "--verbatim-files-from",
        "--files-from=-",
        "--transform=s|^|package/|",
        `--mtime=${mtime}`,
        "--owner=0",
        "--group=0",
        "--numeric-owner",
        "--format=ustar",
      ],
      `${names.join("\n")}\n`
    );
    run("gzip", ["-n", "-9", tarPath]);
    copyFileSync(`${tarPath}.gz`, outputPath);
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
}

async function get(url: string): Promise<Response> {
  const response = await fetch(url, { headers: HEADERS });
  if (!response.ok) throw new Error(`GET ${url} answered ${response.status}`);
  return response;
}

/** Downloads and extracts a commit's upstream tree, and reads the date its entries are stamped with. */
async function fetchUpstream(commit: string, workDir: string): Promise<Upstream> {
  const tarball = join(workDir, "upstream.tgz");
  await Bun.write(tarball, await get(`${API}/tarball/${commit}`));
  const extracted = join(workDir, "extracted");
  mkdirSync(extracted);
  run("tar", ["--extract", `--file=${tarball}`, `--directory=${extracted}`]);
  const roots = readdirSync(extracted);
  if (roots.length !== 1) {
    throw new Error(
      `${tarball} holds ${roots.length} root entries, and a repository tarball holds one`
    );
  }
  const body = (await (await get(`${API}/commits/${commit}`)).json()) as {
    commit: { author: { date: string } };
  };
  return { sourceDir: join(extracted, roots[0]), mtime: body.commit.author.date };
}

if (import.meta.main) {
  const [commit, output] = process.argv.slice(2);
  if (!commit) {
    console.error("usage: bun scripts/build-upstream-tarball.ts <commit> [output]");
    process.exit(1);
  }
  const outputPath = resolve(output ?? join(import.meta.dir, "..", `proof-sdk-${commit}.tgz`));
  const workDir = mkdtempSync(join(tmpdir(), "proof-sdk-fetch-"));
  try {
    packUpstream({ ...(await fetchUpstream(commit, workDir)), outputPath });
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
  const bytes = readFileSync(outputPath);
  console.log(outputPath);
  console.log(`${bytes.length} bytes`);
  console.log(`sha256 ${createHash("sha256").update(bytes).digest("hex")}`);
}
