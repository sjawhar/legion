import { afterAll, describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const MEASURE = join(import.meta.dir, "..", "measure.ts");
const WINDOW = join(import.meta.dir, "..", "__fixtures__", "window.json");
const directories: string[] = [];

afterAll(async () => {
  await Promise.all(directories.map((dir) => rm(dir, { recursive: true, force: true })));
});

async function scratch(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "conflicts-measure-"));
  directories.push(dir);
  return dir;
}

/** Runs the command with `args`; its exit code and stdout. */
async function measure(args: string[]): Promise<{ code: number; stdout: string }> {
  const proc = Bun.spawn([process.execPath, MEASURE, ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, code] = await Promise.all([new Response(proc.stdout).text(), proc.exited]);
  return { code, stdout };
}

describe("measure exit codes", () => {
  test("raw records without prs are refused with 2", async () => {
    const file = join(await scratch(), "no-prs.json");
    await writeFile(
      file,
      JSON.stringify({ since: "2026-10-20", until: "2026-10-27", origin_main: "a" })
    );
    expect((await measure(["--from-json", file])).code).toBe(2);
  });

  test("a window whose since is after its until is refused with 2 before any git or gh", async () => {
    const workdir = join(await scratch(), "work");
    const { code } = await measure([
      "--since",
      "2026-10-28",
      "--until",
      "2026-10-27",
      "--workdir",
      workdir,
    ]);
    expect(code).toBe(2);
    expect(existsSync(workdir)).toBe(false);
  });

  test("a --compare-with file that is not JSON is refused with 2", async () => {
    const file = join(await scratch(), "not.json");
    await writeFile(file, "# a markdown report\n");
    expect((await measure(["--from-json", WINDOW, "--compare-with", file])).code).toBe(2);
  });

  test("valid raw records are summarised with 0", async () => {
    const { code, stdout } = await measure(["--from-json", WINDOW, "--branch-prefix", "legion/"]);
    expect(code).toBe(0);
    // window.json holds three PRs on legion/ branches, all of them read.
    expect(/^- PRs read: (\d+);/m.exec(stdout)?.[1]).toBe("3");
  });
});
