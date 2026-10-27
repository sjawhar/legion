import { afterEach, expect, test } from "bun:test";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const SCRIPT = resolve(import.meta.dir, "semantic-line-breaks.ts");
const REPO_ROOT = resolve(import.meta.dir, "..");

const scratchDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    scratchDirectories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))
  );
});

/** A scratch markdown file holding `text`; its path. */
async function markdownFile(text: string) {
  const directory = await mkdtemp(join(tmpdir(), "semantic-line-breaks-"));
  scratchDirectories.push(directory);
  const path = join(directory, "doc.md");
  await writeFile(path, text);
  return path;
}

/** Runs the script as a contributor does: `bun scripts/semantic-line-breaks.ts [--write] <files>`. */
function run(args: string[]) {
  const result = Bun.spawnSync(["bun", SCRIPT, ...args], { cwd: REPO_ROOT });
  return { exitCode: result.exitCode, stdout: result.stdout.toString() };
}

/** `--write` on `text`; the file's content afterwards and the exit code. */
async function written(text: string) {
  const path = await markdownFile(text);
  const { exitCode } = run(["--write", path]);
  return { exitCode, text: await readFile(path, "utf8") };
}

test("R1 finds a second sentence that starts with a lowercase identifier", async () => {
  const path = await markdownFile(
    "The sweeper evicts the room. ygo's default evicts it at once.\n"
  );
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:1: R1:`);
  expect(exitCode).toBe(1);
});

test("a paragraph that starts with inline HTML is checked for R2 like any other", async () => {
  const path = await markdownFile("<kbd>Ctrl</kbd> opens the menu.\n- item\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:2: R2:`);
  expect(exitCode).toBe(1);
});

test("the end of a list item closes a fence the item left open, so later text is checked", async () => {
  const path = await markdownFile("- item\n\n  ```\n  code\nPara one. Para two.\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:5: R1:`);
  expect(exitCode).toBe(1);
});

test("--write leaves a table written without leading pipes as it is", async () => {
  const text =
    "Parts, in order:\n\nPart | Content\n--- | ---\n`Summary` | The change in the reader's terms\n`Problem` | What goes wrong, for whom\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write does not join a paragraph's last line into the header row of a table below it", async () => {
  const text = "Intro text here.\nhead a | head b\n--- | ---\n1 | 2\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write keeps a link reference definition's title on its own line", async () => {
  const text = '[a]: /url\n"Title one. Two"\nSee [a] here.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write leaves the inside of a <pre> block as it is, blank lines included", async () => {
  const text = "<pre>\nline\n\nA. B\n</pre>\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});
