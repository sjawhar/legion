/**
 * Container directives nest at most 32 deep (INFRA-581). The read-only engine draws an opener
 * standing inside 32 open directive containers as the line of text it is, and the default mode
 * refuses it naming its line, as the document service refuses a 33rd nested typed block. Bodies
 * that nest `:::` thousands deep, which froze a reader's tab for about a minute, parse in well
 * under a second in both modes.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/block-schema.js";
import { createHeadlessProof } from "../src/lib-headless.js";

/** The document service's own block schema: its callout takes `block+`, so callouts nest. */
const serverBlockSchema: BlockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);

const maxNesting = 32;

/** 2,000 nested unclosed openers, then one long line: 31,000 characters. */
const inputUnclosed = ":::a\n".repeat(2000) + "x".repeat(21000);
/** 1,000 nested openers, then 1,000 closing lines. */
const inputClosed = ":::a\n".repeat(1000) + ":::\n".repeat(1000);

/** The most a parse of either input may take: about 60 s and 15 s before INFRA-581, about 0.3 s
 *  after it. */
const parseBudgetMs = 5_000;

/**
 * `depth` callouts nested inside one another and closed, the innermost holding a paragraph. A
 * closing line closes the outermost block whose fence it satisfies, so the fences shorten inwards:
 * level i of `depth` opens with 3 + (depth - i) colons, and the blocks close innermost first, each
 * with its own run. The level-i opener stands on line i.
 */
function nestedCallouts(depth: number): string {
  const lines: string[] = [];
  for (let level = 1; level <= depth; level++) {
    lines.push(`${":".repeat(3 + depth - level)}callout{}`);
  }
  lines.push("a");
  for (let level = depth; level >= 1; level--) lines.push(":".repeat(3 + depth - level));
  return `${lines.join("\n")}\n`;
}

/** How many callouts stand inside one another on `node`'s deepest path. */
function calloutDepth(node: ProseMirrorNode): number {
  let deepest = 0;
  node.forEach((child) => {
    deepest = Math.max(deepest, calloutDepth(child));
  });
  return node.type.name === "callout" ? deepest + 1 : deepest;
}

/** The innermost callout on `node`'s first path of callouts. */
function innermostCallout(node: ProseMirrorNode): ProseMirrorNode | undefined {
  let found: ProseMirrorNode | undefined;
  node.forEach((child) => {
    if (found === undefined && child.type.name === "callout") {
      found = innermostCallout(child) ?? child;
    }
  });
  return found;
}

async function readOnlyProof() {
  return createHeadlessProof({ blockSchema: serverBlockSchema, readOnly: true });
}

async function defaultProof() {
  return createHeadlessProof({ blockSchema: serverBlockSchema });
}

/** How long `parse` takes, in milliseconds; a refusal it throws still counts its time. */
function timed(parse: () => unknown): number {
  const started = performance.now();
  try {
    parse();
  } catch {
    // The default mode refuses both inputs; what is measured is how long it takes to.
  }
  return performance.now() - started;
}

test("read-only draws the 33rd nested opener and every line after it as text", async () => {
  // Each level's opener is followed by a line of its own, so every line drawn as text can be
  // found by its words; the openers past the bound are left unclosed, as the reported body's were.
  const lines: string[] = [];
  for (let level = 1; level <= maxNesting + 1; level++) {
    lines.push(":::callout{}", `level ${level} words`, "");
  }
  lines.push("after the openers", "", "the last line");
  const doc = (await readOnlyProof()).parseMarkdown(lines.join("\n"));

  expect(calloutDepth(doc)).toBe(maxNesting);
  const innermost = innermostCallout(doc);
  expect(innermost).toBeDefined();
  innermost?.forEach((child) => {
    expect(child.type.name).toBe("paragraph");
  });
  // The 33rd opener, its line, and every line after it stand in the innermost callout as text.
  for (const line of lines.slice(maxNesting * 3).filter((line) => line !== "")) {
    expect(innermost?.textContent).toContain(line);
  }
  for (let level = 1; level <= maxNesting + 1; level++) {
    expect(doc.textContent).toContain(`level ${level} words`);
  }
});

test("read-only nests 32 callouts as 32 typed blocks", async () => {
  const doc = (await readOnlyProof()).parseMarkdown(nestedCallouts(maxNesting));
  expect(calloutDepth(doc)).toBe(maxNesting);
  expect(innermostCallout(doc)?.textContent).toBe("a");
  expect(doc.textContent).not.toContain(":::");
});

test("the default mode nests 32 callouts as 32 typed blocks", async () => {
  const doc = (await defaultProof()).parseMarkdown(nestedCallouts(maxNesting));
  expect(calloutDepth(doc)).toBe(maxNesting);
  expect(innermostCallout(doc)?.textContent).toBe("a");
  expect(doc.textContent).not.toContain(":::");
});

test("the default mode refuses a 33rd nested callout, naming its line", async () => {
  const proof = await defaultProof();
  const markdown = nestedCallouts(maxNesting + 1);
  const parse = () => {
    proof.parseMarkdown(markdown);
  };
  expect(parse).toThrow(/line 33\b/);
  expect(parse).toThrow(/32/);
});

for (const [name, input] of [
  ["2,000 nested unclosed openers then a long line", inputUnclosed],
  ["1,000 nested openers then 1,000 closing lines", inputClosed],
] as const) {
  test(`read-only parses ${name} in under 5 s`, async () => {
    const proof = await readOnlyProof();
    const took = timed(() => proof.parseMarkdown(input));
    expect(took, `the parse took ${Math.round(took)} ms`).toBeLessThan(parseBudgetMs);
  }, 300_000);

  test(`the default mode parses or refuses ${name} in under 5 s`, async () => {
    const proof = await defaultProof();
    const took = timed(() => proof.parseMarkdown(input));
    expect(took, `the parse took ${Math.round(took)} ms`).toBeLessThan(parseBudgetMs);
  }, 300_000);
}
