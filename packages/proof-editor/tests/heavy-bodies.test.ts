/**
 * The bodies that froze a reader's browser tab (sjawhar/legion#2519): a long list, a list or a
 * quote followed by lines carrying no marker of their own, a run of brackets, and lists nested
 * deeply. Each parses here in time a reader can wait through, and the blocks of each nest no
 * deeper than the document service accepts, so a body the service stores reads the same in the
 * browser and a body it refuses reads as the text it is instead of freezing or overflowing the
 * stack.
 *
 * Three things are asserted:
 *
 * 1. Cost. Every shape, at the longest body a caller may write, parses in the read-only engine and
 *    in the default mode in under five seconds. The bound is a regression lock with room for any
 *    runner, not a target: the lazy-line and bracket shapes take 10-20 s without the dependency
 *    patches, and about half a second with them.
 * 2. The nesting bound, at the boundaries the document service draws them at
 *    (`maxNesting`, packages/envoy/internal/dispatch/pmdoc/nesting.go: a quote, a list, a list
 *    item, a typed block and a footnote definition each count one). A list opener opens a list and
 *    its first item, so it takes two of the hundred; a quote takes one; a `:::` opener takes one.
 *    Every line past the bound is kept as the text it is, never dropped.
 * 3. That the bound reaches a bare `unified().use(remarkParse)` processor, which is the path
 *    Milkdown and the headless engine both parse through. The bound is a rewrite of the `list` and
 *    `blockQuote` constructs `micromark-core-commonmark` exports, so a second copy of that package
 *    in the tree would leave one of them unbounded, and this test is what says so.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import remarkParse from "remark-parse";
import { unified } from "unified";
import { createHeadlessProof, type HeadlessProofEditor } from "../src/lib-headless.js";
import { serverBlockSchema } from "./block-schema-fixture";

/** The longest comment, message or broadcast body a caller may write, in characters. */
const bodyCap = 32_000;

/** The most a parse of a body at that cap may take, on any runner. */
const parseBudgetMs = 5_000;

/**
 * How many blocks a document may open inside one another: `maxNesting` in
 * packages/envoy/internal/dispatch/pmdoc/nesting.go, which refuses a write that opens one more.
 */
const maxBlockNesting = 100;

/** How deeply list markers alone may nest: each opens a list and its first item. */
const maxListNesting = maxBlockNesting / 2;

/** How deeply quotes alone may nest: each opens one block. */
const maxQuoteNesting = maxBlockNesting;

/** How deeply a quote and a list alternating may nest: the two open three blocks a level. */
const maxAlternatingNesting = Math.floor(maxBlockNesting / 3);

/** `depth` list items nested by indentation, each line opening a list and its item. */
function nestedItems(depth: number): string {
  const lines: string[] = [];
  for (let level = 0; level < depth; level++) lines.push(`${"  ".repeat(level)}- a`);
  return `${lines.join("\n")}\n`;
}

/** The two nested shapes, whose blank lines every open block above them reads. */
const nest49 = nestedItems(maxListNesting - 1);
const nest100 = nestedItems(maxBlockNesting);

/** The shapes, each at the body cap, as a reader is sent them. */
const shapes: { name: string; body: string }[] = [
  { name: "a plain list of 8,000 items", body: "- x\n".repeat(8_000) },
  { name: "a quote then lines carrying no marker", body: `> x\n${"x\n".repeat(15_998)}` },
  { name: "a list item then lines carrying no marker", body: `- x\n${"x\n".repeat(15_998)}` },
  {
    name: "a run of 15,000 brackets and as many closers",
    body: "[".repeat(15_000) + "]".repeat(15_000),
  },
  {
    name: "49 items nested by indentation then blank lines",
    body: nest49 + "\n".repeat(bodyCap - nest49.length),
  },
  {
    name: "100 items nested by indentation then blank lines",
    body: nest100 + "\n".repeat(bodyCap - nest100.length),
  },
  { name: "a chain of 15,999 list markers on one line", body: `${"- ".repeat(15_999)}x` },
  {
    name: "a typed fence whose label opens a run of brackets",
    body: `:::callout[${"[".repeat(15_000)}${"]".repeat(14_989)}`,
  },
];

/**
 * The two engines, each built once: building one costs about as much as a small parse, and a parse
 * that throws drops only the parser state it opened (`parseMarkdown`, src/lib-headless.ts), so the
 * tests below share them.
 */
let readOnly: Promise<HeadlessProofEditor> | undefined;
let editor: Promise<HeadlessProofEditor> | undefined;

function readOnlyProof(): Promise<HeadlessProofEditor> {
  readOnly ??= createHeadlessProof({ blockSchema: serverBlockSchema, readOnly: true });
  return readOnly;
}

function defaultProof(): Promise<HeadlessProofEditor> {
  editor ??= createHeadlessProof({ blockSchema: serverBlockSchema });
  return editor;
}

/** How long `parse` takes, in milliseconds; a refusal it throws still counts its time. */
function timed(parse: () => unknown): number {
  const started = performance.now();
  try {
    parse();
  } catch {
    // A shape the nesting bound refuses is refused in the default mode; what is measured is how
    // long it takes to.
  }
  return performance.now() - started;
}

/** How many blocks of type `type` stand inside one another on `node`'s deepest path. */
function depthOf(node: ProseMirrorNode, type: string): number {
  let deepest = 0;
  node.forEach((child) => {
    deepest = Math.max(deepest, depthOf(child, type));
  });
  return node.type.name === type ? deepest + 1 : deepest;
}

for (const shape of shapes) {
  test(`the read-only engine parses ${shape.name} in under 5 s`, async () => {
    const proof = await readOnlyProof();
    const took = timed(() => proof.parseMarkdown(shape.body));
    expect(took, `the parse took ${Math.round(took)} ms`).toBeLessThan(parseBudgetMs);
  }, 300_000);

  test(`the default mode parses ${shape.name} in under 5 s`, async () => {
    const proof = await defaultProof();
    const took = timed(() => proof.parseMarkdown(shape.body));
    expect(took, `the parse took ${Math.round(took)} ms`).toBeLessThan(parseBudgetMs);
  }, 300_000);
}

/** The two modes, which read a body's blocks the same way: only their directives differ. */
const modes = [
  ["the read-only engine", readOnlyProof],
  ["the default mode", defaultProof],
] as const;

for (const [mode, proofOf] of modes) {
  test(`${mode} nests 50 list markers and reads the 51st as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"- ".repeat(maxListNesting)}x`);
    expect(depthOf(deepest, "bullet_list")).toBe(maxListNesting);
    expect(deepest.textContent).toBe("x");

    const over = proof.parseMarkdown(`${"- ".repeat(maxListNesting + 1)}x`);
    expect(depthOf(over, "bullet_list")).toBe(maxListNesting);
    expect(over.textContent).toBe("- x");
  }, 300_000);

  test(`${mode} nests 100 quotes and reads the 101st as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"> ".repeat(maxQuoteNesting)}x`);
    expect(depthOf(deepest, "blockquote")).toBe(maxQuoteNesting);
    expect(deepest.textContent).toBe("x");

    const over = proof.parseMarkdown(`${"> ".repeat(maxQuoteNesting + 1)}x`);
    expect(depthOf(over, "blockquote")).toBe(maxQuoteNesting);
    expect(over.textContent).toBe("> x");
  }, 300_000);

  test(`${mode} nests 33 quote-and-list levels and reads the 34th list as text`, async () => {
    const proof = await proofOf();
    const deepest = proof.parseMarkdown(`${"> - ".repeat(maxAlternatingNesting)}x`);
    expect(depthOf(deepest, "blockquote")).toBe(maxAlternatingNesting);
    expect(depthOf(deepest, "bullet_list")).toBe(maxAlternatingNesting);
    expect(deepest.textContent).toBe("x");

    // The 34th quote stands at the bound's hundredth block; the list its line would open next
    // would stand past it, so the rest of the line is that quote's text.
    const over = proof.parseMarkdown(`${"> - ".repeat(maxAlternatingNesting + 1)}x`);
    expect(depthOf(over, "blockquote")).toBe(maxAlternatingNesting + 1);
    expect(depthOf(over, "bullet_list")).toBe(maxAlternatingNesting);
    expect(over.textContent).toBe("- x");
  }, 300_000);

  test(`${mode} opens a typed block inside 99 blocks and reads one inside 100 as text`, async () => {
    const proof = await proofOf();
    const fence = [":::callout{}", "a", ":::"];
    const insideQuotes = "> ".repeat(maxBlockNesting - 1);
    const inside = proof.parseMarkdown(`${fence.map((line) => insideQuotes + line).join("\n")}\n`);
    expect(depthOf(inside, "blockquote")).toBe(maxBlockNesting - 1);
    expect(depthOf(inside, "callout")).toBe(1);
    expect(inside.textContent).toBe("a");

    const atBound = "> ".repeat(maxBlockNesting);
    const over = proof.parseMarkdown(`${fence.map((line) => atBound + line).join("\n")}\n`);
    expect(depthOf(over, "blockquote")).toBe(maxBlockNesting);
    expect(depthOf(over, "callout")).toBe(0);
    expect(over.textContent).toContain(":::callout{}");
  }, 300_000);

  test(`${mode} keeps every line past the bound as the text it is`, async () => {
    const proof = await proofOf();
    const past = 5;
    const lines: string[] = [];
    for (let level = 1; level <= maxListNesting + past; level++) {
      // Each line past the bound stands at the indentation of the innermost item the bound allows,
      // so every one of them is a list opener the bound refuses rather than a deeper item.
      const indent = Math.min(level, maxListNesting + 1) - 1;
      lines.push(`${"  ".repeat(indent)}- level ${level} words`);
    }
    const doc = proof.parseMarkdown(`${lines.join("\n")}\n`);

    expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    for (let level = 1; level <= maxListNesting + past; level++) {
      expect(doc.textContent).toContain(`level ${level} words`);
    }
    // The refused markers are text too, not a marker the reader never sees.
    expect(doc.textContent).toContain(`- level ${maxListNesting + 1} words`);
  }, 300_000);

  test(`${mode} reads a chain of 1,000 list markers as text past the bound`, async () => {
    // Without the bound this chain overflows the stack of Milkdown's parser, which is a page the
    // reader loses rather than a body they cannot read.
    const proof = await proofOf();
    const doc = proof.parseMarkdown(`${"- ".repeat(1_000)}x`);
    expect(depthOf(doc, "bullet_list")).toBe(maxListNesting);
    expect(doc.textContent).toBe(`${"- ".repeat(1_000 - maxListNesting)}x`);
  }, 300_000);
}

/** An mdast node, as much of one as the bare processor's tree is read through here. */
type MdastNode = {
  type: string;
  value?: string;
  children?: MdastNode[];
};

/** How many nodes of type `type` stand inside one another on `node`'s deepest path. */
function mdastDepthOf(node: MdastNode, type: string): number {
  let deepest = 0;
  for (const child of node.children ?? []) deepest = Math.max(deepest, mdastDepthOf(child, type));
  return node.type === type ? deepest + 1 : deepest;
}

/** Every character of text `node` holds, in order. */
function mdastText(node: MdastNode): string {
  if (node.children === undefined) return node.value ?? "";
  return node.children.map(mdastText).join("");
}

test("the bound holds through a bare remark-parse processor", async () => {
  // Loading the headless engine loads the module that holds micromark's own `list` and
  // `blockQuote` to the bound. The processor below shares those two objects with every parser in
  // this tree - unless a second copy of `micromark-core-commonmark` is installed, which is what
  // this test fails on.
  await readOnlyProof();
  const processor = unified().use(remarkParse);

  const items = processor.parse(`${"- ".repeat(maxListNesting + 1)}x`) as unknown as MdastNode;
  expect(mdastDepthOf(items, "list")).toBe(maxListNesting);
  expect(mdastText(items)).toBe("- x");

  const quotes = processor.parse(`${"> ".repeat(maxQuoteNesting + 1)}x`) as unknown as MdastNode;
  expect(mdastDepthOf(quotes, "blockquote")).toBe(maxQuoteNesting);
  expect(mdastText(quotes)).toBe("> x");
}, 300_000);
