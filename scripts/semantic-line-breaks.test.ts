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
  return {
    exitCode: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
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
  // Input changed from `<kbd>Ctrl</kbd>` on the architect's authorization: `<kbd>` is now verbatim.
  const path = await markdownFile("<span>Ctrl</span> opens the menu.\n- item\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:2: R2:`);
  expect(exitCode).toBe(1);
});

test("a paragraph that opens a <kbd> element is verbatim: check reports nothing and --write leaves it", async () => {
  const text = "<kbd>Ctrl</kbd> opens the menu.\n- item\n";
  const path = await markdownFile(text);
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
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

/** `count` lines, each one sentence, indented by `indent`. */
function sentenceLines(count: number, indent: string, word: string) {
  return Array.from({ length: count }, (_, n) => `${indent}${word} ${n + 1}.\n`).join("");
}

test("R3 does not count a nested list toward its parent: a short parent passes", async () => {
  // The 30 nested items and the 29 blank lines between them run 59 screen rows, 61 with the
  // parent's own two lines.
  const nested = Array.from({ length: 30 }, (_, n) => `  - Nested item ${n + 1}.\n`).join("\n");
  const path = await markdownFile(`- Parent item.\n\n${nested}`);
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});

test("R3 does not count the blank lines R2 requires between nested items toward their parent", async () => {
  // The 50 nested items and the 49 blank lines between them run 99 screen rows, 101 with the
  // parent's own two lines, and `--write` could remove none of the blank lines.
  const nested = Array.from({ length: 50 }, (_, n) => `  - Child ${n + 1}.\n`).join("\n");
  const path = await markdownFile(`- Parent item.\n\n${nested}`);
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});

test("R3 reports a nested item longer than one screen at the nested item, not its parent", async () => {
  const path = await markdownFile(
    `- Parent item.\n\n  - Nested item.\n${sentenceLines(55, "    ", "Line")}`
  );
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe(`${path}:3: R3: 56 screen rows: - Nested item.\n`);
  expect(exitCode).toBe(1);
});

test("R3 counts a parent's paragraph after its nested list toward the parent", async () => {
  const path = await markdownFile(
    `- Parent item.\n\n  - Nested item.\n\n${sentenceLines(50, "  ", "Continuation")}`
  );
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe(`${path}:1: R3: 52 screen rows: - Parent item.\n`);
  expect(exitCode).toBe(1);
});

test("R3 gives a nested item the same verdict whether a sibling follows it or nothing does", async () => {
  // The blank line after the nested item counts toward no item, whether a sibling follows it or the
  // file ends: the nested item runs its marker line plus `extra` rows.
  const parent = (extra: number, after: string) =>
    `- Parent item.\n\n  - Nested item.\n${sentenceLines(extra, "    ", "Line")}\n${after}`;
  for (const after of ["", "  - Sibling item.\n"]) {
    const fits = run([await markdownFile(parent(49, after))]);
    expect(fits.stdout).toBe("");
    expect(fits.exitCode).toBe(0);
    const path = await markdownFile(parent(50, after));
    const long = run([path]);
    expect(long.stdout).toBe(`${path}:3: R3: 51 screen rows: - Nested item.\n`);
    expect(long.exitCode).toBe(1);
  }
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

test("R1 finds a second sentence that opens with a parenthesis", async () => {
  const path = await markdownFile(
    "The model turns fail, loudly, which is right for a proof. (A call's parent is a helper.)\n"
  );
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:1: R1:`);
  expect(exitCode).toBe(1);
});

test("--write cuts before a second sentence that opens with a parenthesis", async () => {
  const result = await written(
    "The model turns fail, loudly, which is right for a proof. (A call's parent is a helper.)\n"
  );
  expect(result.text).toBe(
    "The model turns fail, loudly, which is right for a proof.\n(A call's parent is a helper.)\n"
  );
  expect(result.exitCode).toBe(0);
});

test("R1 reads a parenthesis that opens with a backtick after a sentence end as a sentence start", async () => {
  const path = await markdownFile("The gate runs first. (`make check` runs it.)\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:1: R1:`);
  expect(exitCode).toBe(1);
});

test("R1 reads no sentence start at a parenthesis in the middle of a sentence", async () => {
  const path = await markdownFile("Run the tests, see (Foo) for why, and then merge.\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});

test("R1 reads no sentence start at a parenthesis after an abbreviation", async () => {
  const text = "Name a limit, e.g. (Memory) or (CPU), for each pod.\n";
  const path = await markdownFile(text);
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
  expect((await written(text)).text).toBe(text);
});

test("R1 reads no sentence start at a parenthesis that opens with a lowercase word", async () => {
  const path = await markdownFile("The pod restarts. (or not, if it is pinned.)\n");
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

test("--write leaves a <pre> element on a blockquote's lazy line as it is", async () => {
  const text = "> Quote <pre>\nline one\nline two</pre>\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves a <pre> element in a footnote definition's text as it is", async () => {
  const text = "[^1]: Run <pre>\nline one\nline two</pre>\n\nText[^1].\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves a <code> element that runs past an escaped close tag as it is", async () => {
  // `\</code>` is text, so the element runs to the second `</code>`, over the line break.
  const text = "Use <code>a \\</code> b\nc</code> here.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves a paragraph byte for byte when a nested <pre> closes before the outer one", async () => {
  const text = "Use <pre>a <pre>b</pre> c. Then more text\nd</pre> here.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves a paragraph byte for byte when a nested <code> closes before the outer one", async () => {
  const text = "Use <code>a <code>b</code> c\nd</code> here.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves the lines after a <pre> opened inside emphasis byte for byte", async () => {
  const text = "> Quote *x <pre>y*\nline one\nline two</pre>\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves every block after an unclosed <pre> byte for byte, to the end of the file", async () => {
  const text = "Use <pre>x\n\nNext b. Then c\nd.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("--write leaves the block after the one that closes a <pre> as it is: close tags are not read", async () => {
  const text = "Use <pre>a\nb</pre> here.\n\nNext one. Then two.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a <code> named inside a code span opens no element, so later blocks are still checked", async () => {
  // docs/kubernetes.md and packages/pi-envoy/AGENTS.md name `(<code>)` in a code span.
  const path = await markdownFile("Use `<code>` for code.\n\nLater one. Later two.\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toContain(`${path}:3: R1:`);
  expect(exitCode).toBe(1);
});

test("a <code> named inside a code span opens no element, so --write re-lays out later blocks", async () => {
  const result = await written("Use `<code>` for code.\n\nLater one. Later two.\n");
  expect(result.text).toBe("Use `<code>` for code.\n\nLater one.\nLater two.\n");
});

test("a </pre> inside an HTML comment closes no <pre>, so the next paragraph stays as it is", async () => {
  const text =
    "<pre>\n<!-- </pre> -->\n\nThis sentence starts on one line\nand ends on the next.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a </pre> inside an attribute value closes no <pre>, so the next paragraph stays as it is", async () => {
  const text =
    'Intro <pre> then <span title="</pre>">x</span>\n\nThis sentence starts on one line\nand ends on the next.\n';
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("an inner </pre> keeps the outer <pre> open into later paragraphs", async () => {
  const text = "Use <pre>a <pre>b</pre>\n\nNext x. Then y.\n\nz</pre>\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

test("a <pre> in backticks inside an HTML block counts, so --write leaves the next block as it is", async () => {
  // An HTML block's raw HTML is its whole text: a backtick there opens no code span.
  const text = "<span>\n`<pre>`\n</span>\n\nNext x. Then y.\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
});

// Each holds `<pre` somewhere in raw HTML that CommonMark and GitHub's HTML parser may read
// differently: the block holding it and every block after it stay byte for byte, so --write
// changes nothing and exits 0.
for (const [shape, text] of [
  ["inside a processing instruction", "Use <?a > <pre> ?> b\n\nNext x. Then y.\n"],
  ["inside CDATA", "Use <![CDATA[ > <pre> ]]> b\n\nNext x. Then y.\n"],
  ["after a comment's --!>", "Use <!-- x --!> <pre> --> b\n\nNext x. Then y.\n"],
  ["in a tag with no space between attributes", '<span>\nx <pre class="a"b>\n\nNext x. Then y.\n'],
  ["in a tag with a quote in an unquoted value", '<span>\nx <pre a=b"c>\n\nNext x. Then y.\n'],
  ["before a malformed comment", "<pre>\n<!-x </pre>\n\nNext x. Then y.\n"],
  ["before an unclosed processing instruction", "<pre>\n<?x </pre>\n\nNext x. Then y.\n"],
  ["before an unclosed tag", "<pre>\n<span a=1 </pre>\n\nNext x. Then y.\n"],
]) {
  test(`--write leaves the rest of the file as it is after a <pre> ${shape}`, async () => {
    const result = await written(text);
    expect(result.text).toBe(text);
    expect(result.exitCode).toBe(0);
  });
}

test("the guard refuses a re-layout after a <pre> only micromark reads, in a blockquote", async () => {
  // micromark reads an HTML block inside the quote; the block scanner reads the quote's text, where
  // `<pre a=b"c>` is no valid tag, so it re-lays out the paragraph below.
  const text = '> <pre a=b"c>\n\nNext x. Then y.\n';
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toContain(`${path}: refusing to write: the block at line 3`);
  expect(result.exitCode).toBe(2);
  expect(await readFile(path, "utf8")).toBe(text);
});

// GitHub's cmark-gfm still reads `source` as an HTML block tag (CommonMark 0.31 and micromark do
// not), so a line that starts with `<source` interrupts the paragraph and stays as it is.
for (const text of [
  'Some words here\n<source "x"> *a* more words.\n',
  'Intro words.\n<source "x"> <code "y">alpha\nbeta gamma</code> tail.\n',
]) {
  test(`--write leaves a line that starts with <source as it is: ${JSON.stringify(text)}`, async () => {
    const result = await written(text);
    expect(result.text).toBe(text);
    expect(result.exitCode).toBe(0);
  });
}

// VERBATIM_OPEN's names and letter case: each of the eight names, upper and mixed case, opens a
// verbatim element; a lookalike name does not.
for (const name of ["pre", "code", "tt", "kbd", "samp", "textarea", "script", "style"]) {
  for (const spelled of [name.toUpperCase(), name.charAt(0).toUpperCase() + name.slice(1)]) {
    test(`<${spelled}> opens a verbatim element`, async () => {
      const text = `Use <${spelled}>one. Two\nthree</${spelled}> here.\n`;
      const result = await written(text);
      expect(result.text).toBe(text);
      expect(result.exitCode).toBe(0);
    });
  }
}

for (const name of ["prefix", "codex"]) {
  test(`<${name}> is no verbatim element, so --write re-lays out its paragraph`, async () => {
    const result = await written(`Use <${name}>one. Two\nthree</${name}> here.\n`);
    expect(result.text).toBe(`Use <${name}>one.\nTwo three</${name}> here.\n`);
    expect(result.exitCode).toBe(0);
  });
}

// GitHub's cmark-gfm reads a vertical tab and a form feed as spaces and a carriage return as a line
// ending; --write refuses a file that holds one. Each input's first line is one the re-layout would
// split, so a refusal that still wrote the file fails the last assertion.
for (const [what, text] of [
  ["a form feed after <source", "Some words. More words.\n<source\fsrc=x> more words.\n"],
  ["a carriage return with no line feed", "Some words. More words.\n<source\rmore words.\n"],
  ["a vertical tab", "Some words. More words.\n<source\vsrc=x> more words.\n"],
  ["CRLF line endings", "Some words. More words.\nSome text. More text\r\nhere.\r\n"],
]) {
  test(`--write refuses a file that holds ${what}`, async () => {
    const path = await markdownFile(text);
    const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
    expect(result.stderr.toString()).toContain(`${path}: refusing to write: line 2 holds`);
    expect(result.exitCode).toBe(2);
    expect(await readFile(path, "utf8")).toBe(text);
  });
}

test("check mode reads <source followed by a form feed as an HTML block start", async () => {
  const path = await markdownFile("Some words here. More words\n<source\fsrc=x> one. Two.\n");
  const { stdout } = run([path]);
  expect(stdout).not.toContain(`${path}:2:`);
});

test("--write refuses a file that is not valid UTF-8, leaving its bytes as they are", async () => {
  const path = await markdownFile("");
  const bytes = Buffer.concat([
    Buffer.from("Some words. More words.\nCaf"),
    Buffer.from([0xe9]),
    Buffer.from(" words here. More words follow.\n"),
  ]);
  await writeFile(path, bytes);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toContain(
    `${path}: refusing to write: the file is not valid UTF-8`
  );
  expect(result.exitCode).toBe(2);
  expect(Buffer.compare(await readFile(path), bytes)).toBe(0);
});

test("--write keeps a byte order mark", async () => {
  const result = await written("\uFEFFSome words. More words.\n");
  expect(result.text).toBe("\uFEFFSome words.\nMore words.\n");
  expect(result.exitCode).toBe(0);
});

/** The warning the script prints for a verbatim element opened by `tag` on 1-based `line`. */
function verbatimWarning(path: string, line: number, tag: string) {
  return `${path}:${line}: warning: ${tag} opens a verbatim element; from this block to the end of the file, R1 and R2 are not checked and --write changes nothing\n`;
}

test("check warns at the verbatim tag's own line in a multi-line paragraph and keeps exit 0", async () => {
  const path = await markdownFile("A first line\nsecond line\nthird line has <PRE>x</PRE> here.\n");
  const { exitCode, stdout, stderr } = run([path]);
  expect(stdout).toBe("");
  expect(stderr).toBe(verbatimWarning(path, 3, "<pre>"));
  expect(exitCode).toBe(0);
});

test("check warns about a verbatim tag after a finding and keeps exit 1", async () => {
  const path = await markdownFile(
    "One sentence. Two sentences.\n\nIntro line\nwith <code>x</code>.\n"
  );
  const { exitCode, stdout, stderr } = run([path]);
  expect(stdout).toContain(`${path}:1: R1:`);
  expect(stderr).toBe(verbatimWarning(path, 4, "<code>"));
  expect(exitCode).toBe(1);
});

test("--write warns on a file it changes and on a file it leaves unchanged", async () => {
  const changed = await markdownFile("One sentence. Two sentences.\n\n<kbd>Ctrl</kbd> opens it.\n");
  const unchanged = await markdownFile("One sentence.\n\n<kbd>Ctrl</kbd> opens it.\n");
  const { exitCode, stderr } = run(["--write", changed, unchanged]);
  expect(stderr).toBe(
    verbatimWarning(changed, 3, "<kbd>") + verbatimWarning(unchanged, 3, "<kbd>")
  );
  expect(exitCode).toBe(0);
  expect(await readFile(changed, "utf8")).toBe(
    "One sentence.\nTwo sentences.\n\n<kbd>Ctrl</kbd> opens it.\n"
  );
});

test("a <code> in a code span or a fenced block warns nothing", async () => {
  const span = await markdownFile("Write `<code>` here.\n");
  const fenced = await markdownFile("```html\n<code>x</code>\n```\n");
  for (const args of [
    [span, fenced],
    ["--write", span, fenced],
  ]) {
    const { exitCode, stderr } = run(args);
    expect(stderr).toBe("");
    expect(exitCode).toBe(0);
  }
});

test("no rule reads a leading byte order mark as part of line 1", async () => {
  // micromark and cmark-gfm drop the mark, so `<source` opens an HTML block that holds the list.
  const text = "\uFEFF<source src=x> Some words.\n- one. two\n- three\n";
  const result = await written(text);
  expect(result.text).toBe(text);
  expect(result.exitCode).toBe(0);
  // Check mode reads the fence on line 1, so the sentences inside it are code.
  const path = await markdownFile("\uFEFF```\nOne. Two.\n```\n");
  const { exitCode, stdout } = run([path]);
  expect(stdout).toBe("");
  expect(exitCode).toBe(0);
});

test("--write refuses a file that starts with two byte order marks, leaving it as it is", async () => {
  // GitHub reads the second mark as text on line 1; trim() and micromark would drop it.
  const text = "\uFEFF\uFEFF| a | b |\n|---|---|\n| 1 | 2 |\n2. item\n";
  const path = await markdownFile(text);
  const result = Bun.spawnSync(["bun", SCRIPT, "--write", path], { cwd: REPO_ROOT });
  expect(result.stderr.toString()).toContain(`${path}: refusing to write: line 1 holds`);
  expect(result.exitCode).toBe(2);
  expect(await readFile(path, "utf8")).toBe(text);
});

test("--write adds no byte order mark to a file whose first character shares its first byte", async () => {
  // U+FF01 is EF BC 81 in UTF-8.
  const result = await written("\uFF01Some words. More words.\n");
  expect(result.text).toBe("\uFF01Some words.\nMore words.\n");
  expect(result.exitCode).toBe(0);
});
