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

test("--write leaves a fenced block as it is when a fence line inside it is indented four spaces", async () => {
  const text = "```yaml\na: 1\n    ```\nb: x. Next one\nc: y\n```\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write leaves a code block indented with a tab as it is", async () => {
  const text = "Para.\n\n\tcode line one\n\tcode. Two\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("R2 finds a list item that opens with a fence and has no blank line before it", async () => {
  const path = await markdownFile("Steps:\n- ```sh\n  make\n  ```\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:2: R2:`);
  expect(exitCode).toBe(1);
});

test("an ordered marker whose number is not 1 does not interrupt a paragraph, so R2 does not apply", async () => {
  const path = await markdownFile("The release line is\n2. That is the build number.\n");
  const { stdout } = run([path]);
  expect(stdout).not.toContain("R2");
});

test("--write does not join a 01. item into the paragraph above it: 01. starts at 1, so it interrupts", async () => {
  const result = await written("A paragraph line\n01. An item.\n");
  expect(result.text.split("\n")).toContain("01. An item.");
});

test("--write keeps a footnote definition that interrupts a paragraph on its own line", async () => {
  const result = await written("Text[^n] here,\nmore text\n[^n]: The note.\n");
  expect(result.text.split("\n")).toContain("[^n]: The note.");
});

test("--write keeps the content of an empty list item's next line inside that item", async () => {
  const text = "Next.\n\n-\n  1.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write leaves an HTML block opened by a custom element named script-something as it is", async () => {
  const text = "Intro.\n\n<script-runner>\nfoo. Bar\nbaz\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("R1 does not end a sentence at the abbreviation incl.", async () => {
  const path = await markdownFile("Every phase worker (incl. sub-architect) resumes.\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});

test("--write refuses, writing nothing, a re-layout that would change how the file renders", async () => {
  // R2's blank line before the nested item would make its `-` an empty item, not text: whitespace
  // only, yet another rendering.
  const text = "- a\n  - -\n";
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toContain(`${path}: refusing to write: the block at line 1`);
  expect(result.exitCode).toBe(2);
  expect(await readFile(path, "utf8")).toBe(text);
});

test("--write leaves YAML front matter as it is", async () => {
  const text = "---\nname: example\ndescription: Bar. Baz\n---\n\n# Title\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write does not join a task marker with the text on the next line", async () => {
  const result = await written("- [x]\n  done\n");
  expect(result.text).not.toContain("[x] done");
});

test("--write keeps the spaces of a code span that a line break follows", async () => {
  const text = "Run `foo   bar \nbaz` now.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write keeps the line break inside inline <pre> HTML", async () => {
  const text = "Output follows <pre>line one\nline two</pre> in the doc.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});

test("--write keeps the line break inside inline <code> HTML, which GitHub shows", async () => {
  // github-markdown-css sets `code { white-space: break-spaces }`, so the break is visible.
  const text = "Type <code>a\nb</code> now.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
});
