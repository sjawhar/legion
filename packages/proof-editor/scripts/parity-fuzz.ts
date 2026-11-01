#!/usr/bin/env bun
/**
 * The old-against-new fuzz for this package's markdown engine: the four patched dependencies and
 * the block-nesting bound, each read against the unpatched packages as the oracle.
 *
 *   cd packages/proof-editor && bun run parity-fuzz
 *
 * What it compares, over every document it generates and every document it reads:
 *
 * - **The four patches** (`patches/micromark@4.0.2.patch`,
 *   `patches/micromark-core-commonmark@2.0.3.patch`,
 *   `patches/micromark-extension-gfm-footnote@2.1.0.patch`,
 *   `patches/mdast-util-from-markdown@2.0.3.patch`), which claim the same output in less time.
 *   The HTML micromark writes and the mdast tree mdast-util-from-markdown builds, positions
 *   included, must be identical to the unpatched packages', in plain, GFM and GFM + typed-block
 *   parses. Any difference is a defect in a patch.
 * - **The bound** (../src/block-nesting.ts). The bound is the one change here that may alter a
 *   reading, and only for a document the document service refuses: a list or quote opener
 *   standing past `maxBlockNesting` reads as the text it is. So the patched engine reads every
 *   document twice, once before `boundBlockNesting()` and once after, and every document whose
 *   reading changed is put to Go: `pmdoc.Parse` and `pmdoc.ParseForWrite` must both refuse it
 *   (the dump gen/differential.sh runs). Any other document the bound changed fails the run.
 *
 * The oracle is installed under `$TMPDIR`, at the versions this workspace resolves, and the
 * workspace's own `node_modules` is never written to. The run asserts that the oracle's sources
 * carry none of the four patches and that the workspace's carry all four, so a comparison of a
 * patched tree against itself cannot pass unnoticed, and that the bound, once installed, does
 * change the reading of a chain past it, so a wrap that reached no construct cannot pass either.
 *
 * The alphabet follows
 * docs/solutions/integration-patterns/replacing-a-micromark-construct-fuzzes-old-against-new-with-every-construct-its-lines-meet-pr2403.md:
 * every construct the rewritten lines meet, at several prefixes inside list items and quotes.
 *
 * Flags: `--documents <n>` how many documents the mixed family generates (default 52,000),
 * `--deep <n>` how many the deep family generates (default 8,000), `--lazy <n>` how many the
 * lazy family generates (default 12,000), `--seed <n>` the first seed,
 * `--oracle <dir>` where the unpatched packages live, `--phase patches|all` to stop after the
 * patch comparison (`patches` leaves the bound uninstalled, for a run against a mutated
 * dependency tree), `--keep` to leave the Go inputs on disk.
 */

import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { boundBlockNesting } from "../src/block-nesting.js";
import { directiveContainerSyntax } from "../src/directive-container.js";

const packageDirectory = join(import.meta.dir, "..");
const repository = join(packageDirectory, "..", "..");
const pmdoc = join(repository, "packages", "envoy", "internal", "dispatch", "pmdoc");

/** What the command line asked for. */
interface Settings {
  /** How many documents the mixed family generates. */
  documents: number;
  /** How many documents the deep family generates. */
  deep: number;
  /** How many documents the lazy family generates. */
  lazy: number;
  /** The seed of the first generated document. */
  seed: number;
  /** Where the unpatched packages are installed. */
  oracle: string;
  /** `patches` stops after the patch comparison; `all` installs the bound and reads again. */
  phase: string;
  /** Whether to leave the documents Go judged on disk. */
  keep: boolean;
}

function options(): Settings {
  const argv = process.argv.slice(2);
  const read = (flag: string, fallback: string): string => {
    const at = argv.indexOf(flag);
    return at < 0 ? fallback : (argv[at + 1] ?? fallback);
  };
  return {
    documents: Number(read("--documents", "52000")),
    deep: Number(read("--deep", "8000")),
    lazy: Number(read("--lazy", "12000")),
    seed: Number(read("--seed", "1")),
    oracle: read("--oracle", join(tmpdir(), "proof-editor-parity-oracle")),
    phase: read("--phase", "all"),
    keep: argv.includes("--keep"),
  };
}

// ---------------------------------------------------------------------------------------------
// The two dependency trees.

/** The packages the fuzz loads, and the sources each one's patch must or must not have changed. */
const packages = [
  {
    name: "micromark",
    sources: [{ file: "lib/initialize/document.js", marker: "lazyScanFrom" }],
  },
  {
    name: "micromark-core-commonmark",
    sources: [
      { file: "lib/label-end.js", marker: "_labelEndSeen" },
      { file: "lib/list.js", marker: "markdownLineEnding" },
    ],
  },
  {
    name: "micromark-extension-gfm-footnote",
    sources: [{ file: "lib/syntax.js", marker: "_gfmFootnoteCallStop" }],
  },
  {
    name: "mdast-util-from-markdown",
    sources: [
      { file: "lib/index.js", marker: "function prepareList(events, start, length, insertions)" },
    ],
  },
] as const;

/** Where one dependency tree keeps each package the fuzz reads, by package name. */
type Tree = Record<string, string>;

/**
 * Where each package the fuzz needs stands, resolved from `from` through the package that depends
 * on it, so the walk holds in this workspace's isolated layout and in the oracle's hoisted one.
 */
function locate(from: string): Tree {
  const at: Tree = {};
  const resolve = (name: string, source: string): string => {
    at[name] = dirname(Bun.resolveSync(name, source));
    return at[name];
  };
  const remarkParse = resolve("remark-parse", from);
  const fromMarkdown = resolve("mdast-util-from-markdown", remarkParse);
  resolve("micromark", fromMarkdown);
  resolve("micromark-core-commonmark", from);
  const remarkGfm = resolve("remark-gfm", from);
  const gfm = resolve("micromark-extension-gfm", remarkGfm);
  resolve("micromark-extension-gfm-footnote", gfm);
  resolve("mdast-util-gfm", remarkGfm);
  return at;
}

/** Whether each package of a tree carries every part of its patch, by package name. */
function patchState(at: Tree): Record<string, boolean> {
  const state: Record<string, boolean> = {};
  for (const { name, sources } of packages) {
    state[name] = sources.every(({ file, marker }) =>
      readFileSync(join(at[name], file), "utf8").includes(marker)
    );
  }
  return state;
}

/**
 * Installs the unpatched packages under `$TMPDIR`, at the versions the workspace resolves, and
 * answers where they stand. A tree already there at those versions is kept.
 */
function installOracle(directory: string, at: Tree): Tree {
  const dependencies: Record<string, string> = {};
  for (const name of ["remark-parse", "remark-gfm", ...packages.map((one) => one.name)]) {
    dependencies[name] = JSON.parse(readFileSync(join(at[name], "package.json"), "utf8")).version;
  }
  // The point of this tree: the same versions as the workspace resolves, none of them patched.
  const manifest = `${JSON.stringify(
    { name: "proof-editor-parity-oracle", private: true, dependencies, overrides: dependencies },
    null,
    2
  )}\n`;
  mkdirSync(directory, { recursive: true });
  const manifestPath = join(directory, "package.json");
  const installed = join(directory, "node_modules");
  const current = existsSync(manifestPath) ? readFileSync(manifestPath, "utf8") : "";
  if (!existsSync(installed) || current !== manifest) {
    writeFileSync(manifestPath, manifest);
    const install = Bun.spawnSync(["bun", "install", "--no-summary"], {
      cwd: directory,
      stdout: "inherit",
      stderr: "inherit",
    });
    if (install.exitCode !== 0) throw new Error(`bun install in ${directory} failed`);
  }
  return locate(directory);
}

/** One dependency tree's reading of markdown: the HTML micromark writes and the mdast tree. */
interface Engine {
  micromark: (value: string, options: unknown) => string;
  fromMarkdown: (value: string, options: unknown) => unknown;
  /** GFM's micromark syntax and HTML extensions, built from this tree. */
  gfm: unknown;
  gfmHtml: unknown;
  /** GFM's mdast extensions, one fresh set per reading: they hold state while a tree is built. */
  gfmFromMarkdown: () => unknown[];
}

async function load(at: Tree): Promise<Engine> {
  // Dynamic, because the specifier is the point: the same four packages are loaded twice, once
  // from this workspace and once from the unpatched tree under `$TMPDIR`, and neither path is
  // known until the trees are located. A static import would load one of them twice.
  const { micromark } = await import(join(at.micromark, "index.js"));
  const { fromMarkdown } = await import(join(at["mdast-util-from-markdown"], "index.js"));
  const { gfm, gfmHtml } = await import(join(at["micromark-extension-gfm"], "index.js"));
  const { gfmFromMarkdown } = await import(join(at["mdast-util-gfm"], "index.js"));
  return { micromark, fromMarkdown, gfm: gfm(), gfmHtml: gfmHtml(), gfmFromMarkdown };
}

/** One way of reading a document: a named parser configuration, run against both trees. */
interface Reading {
  name: string;
  html: (body: string) => string;
  tree: (body: string) => string;
}

function readings(engine: Engine): Reading[] {
  const one = (
    name: string,
    extensions: unknown[],
    mdastExtensions: unknown[],
    htmlExtensions: unknown[]
  ): Reading => ({
    name,
    html: (body) =>
      engine.micromark(body, { extensions, htmlExtensions, allowDangerousHtml: true }),
    tree: (body) => JSON.stringify(engine.fromMarkdown(body, { extensions, mdastExtensions })),
  });
  return [
    one("plain", [], [], []),
    one("gfm", [engine.gfm], engine.gfmFromMarkdown(), [engine.gfmHtml]),
    one("gfm+typed", [engine.gfm, directiveContainerSyntax], engine.gfmFromMarkdown(), [
      engine.gfmHtml,
    ]),
  ];
}

/** What a reading answers for a document, a throw read as its own answer. */
function answer(reading: Reading, key: "html" | "tree", body: string): string {
  try {
    return reading[key](body);
  } catch (error) {
    return `THROW ${String(error)}`;
  }
}

// ---------------------------------------------------------------------------------------------
// The documents.

/** A deterministic generator, so a reported document is the one a rerun generates. */
function random(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state * 1664525 + 1013904223) >>> 0;
    return state / 4294967296;
  };
}

/** One of a table's entries, for the generators below, which draw from six of them. */
const pick = <T>(next: () => number, from: readonly T[]): T =>
  from[Math.floor(next() * from.length)];

/**
 * What a generated line holds: every construct the patched scans and the bound's starts meet
 * across lines - quotes and list markers, indentation, blank lines, lazy text, brackets and
 * images, link and footnote definitions and the calls that look them up, fenced code with both
 * fences opened and left open, HTML flow opened and left open, indented code, and a typed block.
 */
const bodies = [
  "",
  "x",
  "lazy text",
  "a *b* `c` &amp; d",
  "- item",
  "* item",
  "+ item",
  "1. item",
  "3) item",
  "> quoted",
  ">",
  "two words",
  "    indented code",
  "\tx",
  "```",
  "```js",
  "~~~",
  "~~~ruby",
  "code line",
  "<div>",
  "</div>",
  "<!-- open",
  "-->",
  "[a]: /u",
  '[b]: /v "t"',
  "[^1]: note",
  "see [a] and [^1]",
  "see [b][a] and ![a]",
  "![a](u)",
  "[",
  "]",
  "![",
  "](",
  "[[[]]]",
  "[a](",
  ":::callout{}",
  ":::callout{#c}",
  ":::",
  ":::callout[",
  "]{#a}",
  "# heading",
  "---",
  "| a | b |",
  "| - | - |",
  "~~s~~ and www.a.co",
  "- [x] task",
] as const;

/** The columns a generated line opens at: inside list items and quotes, and lazily outside them. */
const prefixes = [
  "",
  "  ",
  "    ",
  "\t",
  "> ",
  ">",
  "> > ",
  "- ",
  "  - ",
  "- > ",
  "> - ",
  "1. ",
  "   ",
  "      ",
] as const;

/** A short document of lines at mixed prefixes, one in four of them left without one. */
function mixedDocument(seed: number): string {
  const next = random(seed);
  const lines = 1 + Math.floor(next() * (next() < 0.12 ? 60 : 10));
  let prefix: string = pick(next, prefixes);
  const out: string[] = [];
  for (let line = 0; line < lines; line++) {
    if (next() < 0.3) prefix = pick(next, prefixes);
    out.push((next() < 0.25 ? "" : prefix) + pick(next, bodies));
  }
  // A document that ends without a final line feed reads its last line differently.
  return out.join("\n") + (next() < 0.7 ? "\n" : "");
}

/** The containers a lazy document holds its blocks in. */
const containers = ["> ", "- ", "  ", "> > ", "- > ", "1. ", "> - "] as const;

/** A block a lazy document closes inside its container before it opens the next paragraph. */
const closedBlocks = [
  "a",
  "# h",
  "```\nc\n```",
  "[d]: /u",
  "<div>\nx\n</div>",
  "    code",
  "---",
  "x\n---",
  "x\n===",
  "> inner",
  "- item",
  "~~~\nc\n~~~",
  "| a |\n| - |",
] as const;

/** A line a lazy document goes on with outside its container, where micromark reads laziness. */
const lazyLines = [
  "lazy",
  "more lazy",
  "---",
  "===",
  "[e]: /v",
  "<div>",
  "    code",
  "- item",
  "> q",
  "# h",
  "```",
  "~~~",
  "<!-- c",
  "]",
  "[",
] as const;

/**
 * A document of blocks a container closed, then an open paragraph, then lines without the
 * container's marker.
 *
 * This is the shape that reads the lazy-line watermark of `patches/micromark@4.0.2.patch`: the
 * watermark only advances over the events of blocks that closed before the line, so a container
 * holding one paragraph never reaches it, however many lazy lines follow. A chain of closed
 * blocks before the open one does, and the setext underlines, definitions and HTML flow among
 * the lines are the constructs that rewrite a flow's events while it is still being written.
 */
function lazyDocument(seed: number): string {
  const next = random(seed);
  const container = pick(next, containers);
  const blank = container.trimEnd();
  const out: string[] = [];
  const rounds = 1 + Math.floor(next() * 4);
  for (let round = 0; round < rounds; round++) {
    for (const line of pick(next, closedBlocks).split("\n")) out.push(container + line);
    if (next() < 0.7) out.push(blank);
    out.push(`${container}open`);
    const lines = 1 + Math.floor(next() * 4);
    for (let line = 0; line < lines; line++) out.push(pick(next, lazyLines));
    if (next() < 0.4) out.push(blank);
  }
  return out.join("\n") + (next() < 0.7 ? "\n" : "");
}

/** The openers a deep document chains, and how many blocks the document service counts for each. */
const chains = [
  { open: "> ", blocks: 1 },
  { open: ">", blocks: 1 },
  { open: "- ", blocks: 2 },
  { open: "* ", blocks: 2 },
  { open: "1. ", blocks: 2 },
  { open: "> - ", blocks: 3 },
  { open: "- > ", blocks: 3 },
] as const;

/** What follows a chain: the lines whose continuations and lazy starts read its depth. */
const tails = [
  "x\n",
  "x\n\ntail\n",
  "x\nlazy\n",
  "x\n\n\n\ntail\n",
  "x\n- next\n",
  "x\n```\ncode\n",
  "",
] as const;

/** A list nested one level per line by indentation, as deep as asked. */
const nestedByIndentation = (levels: number): string =>
  Array.from({ length: levels }, (_, level) => `${"  ".repeat(level)}- a\n`).join("");

/** A document whose blocks chain up to, across and past the bound. */
function deepDocument(seed: number): string {
  const next = random(seed);
  const chain = pick(next, chains);
  // Levels spread around the bound, so each side of it is read on every shape.
  const levels =
    next() < 0.65
      ? Math.max(1, Math.round((86 + next() * 28) / chain.blocks))
      : 1 + Math.floor(next() * Math.ceil(120 / chain.blocks));
  if (next() < 0.25) {
    // Nested by indentation instead, one opener per line, so the bound is reached down the page.
    const lines: string[] = [];
    for (let level = 0; level < levels; level++) {
      lines.push("  ".repeat(level) + pick(next, ["- a", "* a", "1. a", "- "]));
    }
    return `${lines.join("\n")}\n${pick(next, tails)}`;
  }
  return chain.open.repeat(levels) + pick(next, tails);
}

/** A document the harness names, and the markdown it holds. */
interface Document {
  label: string;
  body: string;
}

/** The CommonMark examples, downloaded into `$TMPDIR` once. */
function specPath(): string {
  const path = join(tmpdir(), "commonmark-spec-0.31.2.json");
  if (!existsSync(path)) {
    const url = "https://spec.commonmark.org/0.31.2/spec.json";
    const fetched = Bun.spawnSync(["curl", "-sfL", "-o", path, url]);
    if (fetched.exitCode !== 0)
      throw new Error("could not download the CommonMark 0.31.2 examples");
  }
  return path;
}

/** The superlinear shapes of sjawhar/legion#2519, each small enough for the unpatched packages. */
function heavyShapes(): [string, string][] {
  const definitions = "[a]: /u\n[^1]: note\n\n";
  const shapes: [string, string][] = [
    ["lazy-quote", `> x\n${"x\n".repeat(1000)}`],
    ["lazy-item", `- x\n${"x\n".repeat(1000)}`],
    ["lazy-quote-blank", `> x\n${"x\n\n".repeat(400)}`],
    ["brackets", "[".repeat(1000) + "]".repeat(1000)],
    ["brackets-images", "![".repeat(700) + "](".repeat(300) + "]".repeat(700)],
    ["brackets-calls", "[a]".repeat(400) + "[^1]".repeat(400)],
    ["typed-brackets", `:::callout[${"[".repeat(1000)}${"]".repeat(989)}`],
    ["plain-list", "- x\n".repeat(1000)],
    ["plain-list-ordered", "1. x\n".repeat(1000)],
    ["nested-blank", `${nestedByIndentation(49)}${"\n".repeat(600)}`],
    ["nested-lazy", `${nestedByIndentation(49)}${"x\n".repeat(600)}`],
    ["nested-100-blank", `${nestedByIndentation(100)}${"\n".repeat(600)}`],
    ["chain-dashes", `${"- ".repeat(1000)}x`],
    ["chain-quotes", `${"> ".repeat(1000)}x`],
    ["fence-open-in-items", `${nestedByIndentation(20)}\n${"  ".repeat(20)}\`\`\`\ncode\n`],
    ["html-open-in-quote", `> <!-- open\n${"x\n".repeat(400)}`],
    ["indented-code-after-item", `- x\n\n${"    code\n".repeat(400)}`],
  ];
  // Each shape again with the definitions its calls find, so the lookups the patch skips when
  // nothing is defined do run.
  return shapes.flatMap(([name, body]): [string, string][] => [
    [name, body],
    [`${name}-defined`, definitions + body],
  ]);
}

/** Every document of the run, in a fixed order, so the two passes read the same ones. */
function* documents(settings: Settings): Generator<Document> {
  // 1. The CommonMark 0.31.2 examples.
  const spec = JSON.parse(readFileSync(specPath(), "utf8")) as {
    markdown: string;
    example: number;
  }[];
  for (const example of spec) yield { label: `spec#${example.example}`, body: example.markdown };

  // 2. The pmdoc corpus, the documents it refuses, and the fixtures generated from the corpus.
  for (const directory of ["corpus", "corpus-errors"]) {
    const where = join(pmdoc, "testdata", directory);
    for (const file of readdirSync(where).sort()) {
      if (!file.endsWith(".md")) continue;
      yield { label: `${directory}/${file}`, body: readFileSync(join(where, file), "utf8") };
    }
  }
  const fixtures = JSON.parse(readFileSync(join(pmdoc, "testdata", "fixtures.json"), "utf8")) as {
    name: string;
    markdown: string;
  }[];
  for (const one of fixtures) yield { label: `fixture/${one.name}`, body: one.markdown };

  // 3. The shapes that cost superlinear time before the patches.
  for (const [name, body] of heavyShapes()) yield { label: `heavy/${name}`, body };

  // 4. The chains that stand at each side of the bound, with each tail.
  for (const chain of chains) {
    for (let levels = 1; levels * chain.blocks <= 112; levels++) {
      for (const [index, tail] of tails.entries()) {
        yield {
          label: `chain/${chain.open.trim()}-${levels}-${index}`,
          body: chain.open.repeat(levels) + tail,
        };
      }
    }
  }
  for (let levels = 1; levels <= 56; levels++) {
    const lines = nestedByIndentation(levels);
    for (const [index, tail] of tails.entries()) {
      yield { label: `indent/${levels}-${index}`, body: lines + tail };
    }
  }

  // 5. The generated documents.
  for (let index = 0; index < settings.documents; index++) {
    const seed = settings.seed + index;
    yield { label: `mixed#${seed}`, body: mixedDocument(seed) };
  }
  for (let index = 0; index < settings.deep; index++) {
    const seed = settings.seed + 1_000_000 + index;
    yield { label: `deep#${seed}`, body: deepDocument(seed) };
  }
  for (let index = 0; index < settings.lazy; index++) {
    const seed = settings.seed + 2_000_000 + index;
    yield { label: `lazy#${seed}`, body: lazyDocument(seed) };
  }
}

// ---------------------------------------------------------------------------------------------
// Go's verdict on what the bound changed.

/** What Go answered for one document: the refusal of each of its two readers, or an empty string. */
interface Verdict {
  parse: string;
  write: string;
}

/**
 * Whether `pmdoc.Parse` and `pmdoc.ParseForWrite` refuse each document, read from the dump
 * `TestDifferentialDump` writes - the same binary and the same two calls gen/differential.sh puts
 * every corpus document through. The dump reads one engine line per document and renders only
 * what Go read, so a null reading leaves it with Go's own two calls, which is all this asks.
 */
function refusedByGo(changed: Document[], keep: boolean): Map<string, Verdict> {
  const out = mkdtempSync(join(tmpdir(), "parity-fuzz-go-"));
  const corpus = join(out, "corpus.jsonl");
  const engine = join(out, "engine.jsonl");
  const dump = join(out, "dump.jsonl");
  writeFileSync(
    corpus,
    changed.map(({ label, body }) => `${JSON.stringify({ id: label, md: body })}\n`).join("")
  );
  writeFileSync(
    engine,
    changed.map(({ label }) => `${JSON.stringify({ id: label, md: null })}\n`).join("")
  );
  const binary = join(out, "pmdoc.test");
  const built = Bun.spawnSync(["go", "test", "-c", "-o", binary, "."], {
    cwd: pmdoc,
    env: { ...process.env, GOWORK: "off" },
    stdout: "inherit",
    stderr: "inherit",
  });
  if (built.exitCode !== 0) throw new Error("could not build the pmdoc test binary");
  const run = Bun.spawnSync(
    [binary, "-test.run", "^TestDifferentialDump$", "-test.count=1", "-test.timeout=0"],
    {
      cwd: pmdoc,
      env: {
        ...process.env,
        PMDOC_DIFF_CORPUS: corpus,
        PMDOC_DIFF_ENGINE: engine,
        PMDOC_DIFF_OUT: dump,
      },
      stdout: "inherit",
      stderr: "inherit",
    }
  );
  if (run.exitCode !== 0) throw new Error("the pmdoc dump failed");
  const verdicts = new Map<string, Verdict>();
  for (const line of readFileSync(dump, "utf8").split("\n")) {
    if (!line.trim()) continue;
    const record = JSON.parse(line) as { id: string; parse?: string; write?: string };
    verdicts.set(record.id, { parse: record.parse ?? "", write: record.write ?? "" });
  }
  if (!keep) Bun.spawnSync(["rm", "-rf", out]);
  else console.log(`  the documents Go judged are in ${out}`);
  return verdicts;
}

// ---------------------------------------------------------------------------------------------
// The run.

/** How many documents of one family were read, and how many of them differed. */
interface Family {
  read: number;
  differences: number;
}

const settings = options();
const patchedAt = locate(packageDirectory);
const oracleAt = installOracle(settings.oracle, patchedAt);
const patched = patchState(patchedAt);
const unpatched = patchState(oracleAt);
for (const { name } of packages) {
  if (!patched[name]) throw new Error(`${name} in this workspace carries no patch`);
  if (unpatched[name]) throw new Error(`${name} in the oracle carries its patch`);
}
console.log(`patched: ${dirname(dirname(patchedAt.micromark))}`);
console.log(`oracle:  ${dirname(dirname(oracleAt.micromark))}`);
for (const { name } of packages) {
  const version = JSON.parse(readFileSync(join(patchedAt[name], "package.json"), "utf8")).version;
  console.log(`  ${name}@${version}`);
}

const oldReadings = readings(await load(oracleAt));
const newReadings = readings(await load(patchedAt));

const started = Date.now();
let read = 0;
let comparisons = 0;
let differences = 0;
const shown: string[] = [];
/** One hash per document of everything the patched engine answered, for the bound's pass. */
const unbounded: bigint[] = [];
const counts = new Map<string, Family>();

for (const { label, body } of documents(settings)) {
  const name = label.split(/[#/]/)[0];
  const group = counts.get(name) ?? { read: 0, differences: 0 };
  counts.set(name, group);
  group.read++;
  read++;
  let answers = "";
  for (const [index, reading] of newReadings.entries()) {
    for (const key of ["html", "tree"] as const) {
      const mine = answer(reading, key, body);
      const theirs = answer(oldReadings[index], key, body);
      answers += `\u0000${mine}`;
      comparisons++;
      if (mine === theirs) continue;
      differences++;
      group.differences++;
      if (shown.length < 10) {
        shown.push(
          `${label} [${reading.name}/${key}]\n  in:  ${JSON.stringify(body.slice(0, 300))}\n` +
            `  old: ${JSON.stringify(theirs.slice(0, 300))}\n` +
            `  new: ${JSON.stringify(mine.slice(0, 300))}`
        );
      }
    }
  }
  unbounded.push(Bun.hash(answers) as bigint);
}

console.log(`\npatched against unpatched, ${((Date.now() - started) / 1000).toFixed(0)}s`);
for (const [name, group] of [...counts].sort()) {
  console.log(`  ${name.padEnd(14)} ${group.read} documents, ${group.differences} differences`);
}
console.log(
  `  ${"total".padEnd(14)} ${read} documents, ${comparisons} comparisons, ${differences} differences`
);
for (const line of shown) console.log(`----\n${line}`);

let failed = differences > 0;
if (settings.phase !== "patches") {
  // The bound rewrites the constructs the patched engine already holds, so it is installed only
  // now, after every document has been read without it. Each sentinel is a document the bound
  // must read differently, one shape of chain per way of writing one, so a wrap that reached no
  // construct, or one that reads a line's depth from the wrong place, cannot pass unnoticed.
  const sentinels = [
    ["a chain of list markers on one line", `${"- ".repeat(101)}x\n`],
    ["a chain of quote markers on one line", `${"> ".repeat(101)}x\n`],
    ["a list nested one level per line", `${nestedByIndentation(51)}tail\n`],
    ["a chain of quote markers in a list item", `- ${"> ".repeat(101)}x\n`],
  ] as const;
  const beforeBound = sentinels.map(([, body]) => newReadings[0].html(body));
  boundBlockNesting();
  for (const [index, [what, body]] of sentinels.entries()) {
    if (newReadings[0].html(body) !== beforeBound[index]) continue;
    console.log(`\nthe bound left ${what} as it was: it reached no construct there`);
    failed = true;
  }

  const bound = Date.now();
  let boundRead = 0;
  let boundComparisons = 0;
  const changed: Document[] = [];
  for (const { label, body } of documents(settings)) {
    let answers = "";
    for (const reading of newReadings) {
      for (const key of ["html", "tree"] as const) {
        answers += `\u0000${answer(reading, key, body)}`;
        boundComparisons++;
      }
    }
    if (Bun.hash(answers) !== unbounded[boundRead]) changed.push({ label, body });
    boundRead++;
  }

  console.log(`\nbounded against unbounded, ${((Date.now() - bound) / 1000).toFixed(0)}s`);
  console.log(
    `  ${boundRead} documents, ${boundComparisons} comparisons, ` +
      `${changed.length} documents read differently`
  );
  if (changed.length > 0) {
    const verdicts = refusedByGo(changed, settings.keep);
    const stored = changed.filter(({ label }) => (verdicts.get(label)?.parse ?? "") === "");
    const written = changed.filter(({ label }) => (verdicts.get(label)?.write ?? "") === "");
    console.log(`  pmdoc.Parse refuses ${changed.length - stored.length} of ${changed.length}`);
    console.log(
      `  pmdoc.ParseForWrite refuses ${changed.length - written.length} of ${changed.length}`
    );
    const first = changed[0];
    console.log(`  the first of them, ${first.label}: ${verdicts.get(first.label)?.parse}`);
    for (const { label, body } of stored.slice(0, 10)) {
      console.log(
        `----\n${label} reads differently and pmdoc stores it\n  in: ${JSON.stringify(body.slice(0, 300))}`
      );
    }
    failed ||= stored.length > 0 || written.length > 0;
  }
}

console.log(`\n${failed ? "FAIL" : "ok"} in ${((Date.now() - started) / 1000).toFixed(0)}s`);
process.exit(failed ? 1 : 0);
