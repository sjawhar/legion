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

test("--write does not cut inside an inline HTML attribute value that holds a >", async () => {
  const text = 'Hover <abbr title="a>b. Second line of the tip">here</abbr> to see it.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write does not join the lines of an inline HTML attribute value that holds a >", async () => {
  const text = 'Hover <abbr title="x>y\nz">here</abbr> now.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a multi-line setext heading underlined with = is not checked for R1 or re-laid out", async () => {
  const text = "Setup. Then run\nthe tool\n===\n";
  const path = await markdownFile(text);
  const checked = run([path]);
  expect(checked.stdout).toBe("");
  expect(checked.exitCode).toBe(0);
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a setext heading underlined with - is left as it is, while a rule after a blank line is not one", async () => {
  const text = "Setup. Then run\nthe tool\n---\n\nOne. Two\n\n---\n";
  const path = await markdownFile(text);
  const checked = run([path]);
  expect(checked.stdout).toBe(`${path}:5: R1: One. Two\n`);
  expect(checked.exitCode).toBe(1);
  const result = await written(text);
  expect(result.text).toBe("Setup. Then run\nthe tool\n---\n\nOne.\nTwo\n\n---\n");
  expect(result.exitCode).toBe(0);
});

test("front matter whose opener has trailing spaces is front matter to check and --write", async () => {
  const text = "--- \ntitle: One. Two\ntags: x\n---\n\nBody. More.\n";
  const path = await markdownFile(text);
  const checked = run([path]);
  expect(checked.stdout).toBe(`${path}:6: R1: Body. More.\n`);
  expect(checked.exitCode).toBe(1);
  const result = await written(text);
  expect(result.text).toBe("--- \ntitle: One. Two\ntags: x\n---\n\nBody.\nMore.\n");
  expect(result.exitCode).toBe(0);
});

test("front matter closed by ... is front matter to check and --write", async () => {
  const text = "---\ntitle: One. Two\ndescription: x\n...\n\nBody. More.\n";
  const path = await markdownFile(text);
  const checked = run([path]);
  expect(checked.stdout).toBe(`${path}:6: R1: Body. More.\n`);
  expect(checked.exitCode).toBe(1);
  const result = await written(text);
  expect(result.text).toBe("---\ntitle: One. Two\ndescription: x\n...\n\nBody.\nMore.\n");
  expect(result.exitCode).toBe(0);
});

test("a second --write changes nothing when a paragraph holds a tag alone on its line", async () => {
  // packages/pi-envoy/extensions/askgate/askgate-request.md has this shape.
  const once = await written("Intro line:\n<tag>\nvalue\n</tag>\nMore text here.\n");
  const twice = await written(once.text);
  expect(twice.text).toBe(once.text);
});

test("--write refuses a join inside a code span that it misreads as a link destination", async () => {
  // The script reads up to `d)` as the link's destination, so the line break looks like prose;
  // mdast reads the code span `c d) e \nf`, whose content the join would change.
  const text = "See [a](b `c d) e \nf` g.\n";
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toContain(`${path}: refusing to write: the block at line 1`);
  expect(result.exitCode).toBe(2);
  expect(await readFile(path, "utf8")).toBe(text);
});

test("--write reads a < that opens no valid tag as text, so a code span after it stays masked", async () => {
  const text = 'See <K, V="x > `code" > here \nand` then more.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write reads a < that opens no valid tag as text, so a <code> element after it stays masked", async () => {
  const text = 'See <K, V="x > <code>a" > here \nand</code> then more.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a second --write changes nothing when a paragraph holds a line of only :", async () => {
  const once = await written("Intro text\n:\nmore text\n");
  expect(once.exitCode).toBe(0);
  const twice = await written(once.text);
  expect(twice.exitCode).toBe(0);
  expect(twice.text).toBe(once.text);
});

test("a second --write changes nothing when a paragraph holds a line of only -=", async () => {
  const once = await written("Intro text\n-=\nmore text\n");
  expect(once.exitCode).toBe(0);
  const twice = await written(once.text);
  expect(twice.exitCode).toBe(0);
  expect(twice.text).toBe(once.text);
});

test("--write refuses, writing nothing, a re-layout that a second re-layout would change again", async () => {
  // `[see. Next [a]:` could open a link reference definition, so the line stays apart; once the
  // sentence break moves `[a]:` to the next line, a second run joins `[see.` onto `Intro text`.
  const text = "Intro text\n[see. Next [a]: b\n";
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toBe(
    `${path}: refusing to write: a second re-layout would change it again (first difference at line 1 of the re-laid-out text)\n`
  );
  expect(result.exitCode).toBe(2);
  expect(await readFile(path, "utf8")).toBe(text);
});

/** `--write` on `text`, which it must refuse: the refusal on stderr, the exit code, the file after. */
async function refused(text: string) {
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  return {
    path,
    stderr: result.stderr.toString(),
    exitCode: result.exitCode,
    text: await readFile(path, "utf8"),
  };
}

test("--write refuses a join inside a <pre> element on a blockquote's lazy line", async () => {
  const text = "> Quote <pre>\nline one\nline two</pre>\n";
  const result = await refused(text);
  expect(result.stderr).toContain(`${result.path}: refusing to write: the block at line 1`);
  expect(result.exitCode).toBe(2);
  expect(result.text).toBe(text);
});

test("--write refuses a join inside a <pre> element in a footnote definition's text", async () => {
  const text = "[^1]: Run <pre>\nline one\nline two</pre>\n\nText[^1].\n";
  const result = await refused(text);
  expect(result.stderr).toContain(`${result.path}: refusing to write: the block at line 1`);
  expect(result.exitCode).toBe(2);
  expect(result.text).toBe(text);
});

test("--write refuses a join inside a <code> element past an escaped close tag", async () => {
  // `\</code>` is text, so the element runs to the second `</code>`, over the line break.
  const text = "Use <code>a \\</code> b\nc</code> here.\n";
  const result = await refused(text);
  expect(result.stderr).toContain(`${result.path}: refusing to write: the block at line 1`);
  expect(result.exitCode).toBe(2);
  expect(result.text).toBe(text);
});
