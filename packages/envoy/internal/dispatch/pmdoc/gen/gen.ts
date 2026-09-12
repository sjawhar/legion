// gen.ts — regenerate testdata/fixtures.json from testdata/corpus/*.md using the fork's headless engine.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import * as Y from "yjs";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import { Transform } from "prosemirror-transform";
import { setBlockIdGenerator } from "@sjawhar/proof-editor";
import { createHeadlessProof, type HeadlessProofEditor } from "@sjawhar/proof-editor/headless";

const here = import.meta.dir;
const corpus = join(here, "..", "testdata", "corpus");
const out = join(here, "..", "testdata", "fixtures.json");
const spliceOut = join(here, "..", "testdata", "splices.json");
const check = process.argv.includes("--check");

const fixtures = [];
for (const file of readdirSync(corpus).filter((f) => f.endsWith(".md")).sort()) {
  let blockNumber = 0;
  setBlockIdGenerator(() => `b-${String(++blockNumber).padStart(6, "0")}`);
  const engine = await createHeadlessProof();
  const markdown = readFileSync(join(corpus, file), "utf8");
  const doc = engine.parseMarkdown(markdown);
  const ydoc = new Y.Doc();
  ydoc.clientID = 1;
  prosemirrorToYXmlFragment(doc, ydoc.getXmlFragment("prosemirror"));
  fixtures.push({
    name: file.replace(/\.md$/, ""),
    markdown,
    pm_json: doc.toJSON(),
    yjs_update_v1_b64: Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString("base64"),
    rendered_by_milkdown: engine.serializeMarkdown(doc),
  });
}

let spliceBlockNumber = 0;
setBlockIdGenerator(() => `b-${String(++spliceBlockNumber).padStart(6, "0")}`);
const engine = await createHeadlessProof();

// These are browser-oracle replaceRange cases. A single paragraph replacement
// is sliced open to model Splice's inline paragraph replacement contract.
type ReplaceRangeCase = {
  name: string;
  markdown: string;
  from?: string;
  to?: string;
  at?: string;
  point?: "after" | "before" | "after-textblock" | "before-textblock" | "doc-start" | "doc-end";
  replacement: string;
  inline?: boolean;
};

const replaceRangeCases: ReplaceRangeCase[] = [
  { name: "paragraph-inline", markdown: "Alpha first.\n\nSecond omega.\n", from: "first.", to: "Second", replacement: "X\n", inline: true },
  { name: "paragraph-multiblock", markdown: "Alpha first.\n\nSecond omega.\n", from: "first.", to: "Second", replacement: "X\n\nY\n" },
  { name: "paragraph-list", markdown: "Alpha first.\n\nSecond omega.\n", from: "first.", to: "Second", replacement: "- X\n- Y\n" },
  { name: "list-item-code", markdown: "- target\n- next\n", from: "target", to: "target", replacement: "```\ncode\n```\n" },
  { name: "list-items-paragraph", markdown: "- one\n- two\n- three\n", from: "one", to: "two", replacement: "X\n", inline: true },
  { name: "task-list-open-checked", markdown: "- [ ] keep\n- [x] one\n- [ ] two\n", from: "one", to: "two", replacement: "X\n", inline: true },
  { name: "task-list-open-unchecked", markdown: "- [x] keep\n- [ ] one\n- [x] two\n", from: "one", to: "two", replacement: "X\n", inline: true },
  { name: "list-items-join-inline", markdown: "- a one\n- two b\n", from: "one", to: "two", replacement: "X\n", inline: true },
  { name: "table-cells-join-inline", markdown: "| left | right |\n| :--- | :--- |\n| a one | two b |\n", from: "one", to: "two", replacement: "X\n", inline: true },
  { name: "nested-list-item-to-parent-next-inline", markdown: "- parent\n  - nested tail\n- head sibling\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "heading-into-paragraph-inline", markdown: "# Heading tail\n\nParagraph head after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "blockquote-last-paragraph-to-following-paragraph-inline", markdown: "> first\n>\n> tail\n\nhead after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "open-side-deeper-than-close-side-inline", markdown: "- parent\n  - child tail\n- head sibling\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "close-side-deeper-than-open-side-inline", markdown: "- parent tail\n  - head child\n- sibling\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "task-list-replacement-in-plain-bullet", markdown: "- a target c\n- keep\n", from: "target", to: "target", replacement: "- [x] done\n- [ ] todo\n" },
  { name: "heading-into-following-list-item-inline", markdown: "# Heading tail\n\n- head item\n- after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "list-item-into-following-heading-inline", markdown: "- tail\n\n# head after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "blockquote-into-following-list-inline", markdown: "> tail\n\n- head item\n- after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "blockquote-last-paragraph-into-following-list-inline", markdown: "> first\n>\n> tail\n\n- head item\n- after\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "blockquote-list", markdown: "> alpha\n>\n> beta\n", from: "alpha", to: "beta", replacement: "- X\n- Y\n" },
  { name: "headings-paragraph", markdown: "# Alpha first.\n\n## Second omega.\n", from: "first.", to: "Second", replacement: "X\n" },
  { name: "code-blocks-paragraph", markdown: "```go\nAlpha first.\n```\n\n```js\nSecond omega.\n```\n", from: "first.", to: "Second", replacement: "X\n" },
  { name: "heading-code", markdown: "# a target c\n", from: "target", to: "target", replacement: "```\ncode\n```\n" },
  { name: "code-list", markdown: "```\na target c\n```\n", from: "target", to: "target", replacement: "- X\n- Y\n" },
  { name: "table-cell-code", markdown: "| head |\n| :--- |\n| a target c |\n", from: "target", to: "target", replacement: "```\ncode\n```\n" },
  { name: "table-cell-inline", markdown: "| head |\n| :--- |\n| a target c |\n", from: "target", to: "target", replacement: "X\n", inline: true },
  { name: "table-header-into-first-body-cell-inline", markdown: "| a tail |\n| :--- |\n| head b |\n| after |\n", from: "tail", to: "head", replacement: "X\n", inline: true },
  { name: "insert-inline-after-quote", markdown: "Alpha first. omega.\n", at: "first.", point: "after", replacement: "X\n", inline: true },
  { name: "insert-inline-at-textblock-start", markdown: "Alpha omega.\n", at: "Alpha", point: "before", replacement: "X\n", inline: true },
  { name: "insert-inline-at-textblock-end", markdown: "Alpha omega.\n", at: "omega.", point: "after", replacement: "X\n", inline: true },
  { name: "insert-blocks-after-quote-splits-paragraph", markdown: "Alpha first. omega.\n", at: "first.", point: "after", replacement: "- X\n- Y\n" },
  { name: "insert-block-at-doc-start", markdown: "Body.\n", point: "doc-start", replacement: "# Title\n" },
  { name: "insert-block-at-doc-end", markdown: "Body.\n", point: "doc-end", replacement: "Tail.\n" },
  { name: "insert-block-after-heading-textblock", markdown: "# Title\n\nBody.\n", at: "Title", point: "after-textblock", replacement: "Intro.\n" },
  { name: "insert-block-before-heading-textblock", markdown: "# Title\n\nBody.\n", at: "Title", point: "before-textblock", replacement: "Lead.\n" },
  { name: "insert-paragraph-after-list-item-textblock", markdown: "- one\n- two\n", at: "one", point: "after-textblock", replacement: "extra\n" },
].map(({ name, markdown, from, to, at, point, replacement, inline = false }) => {
  const doc = engine.parseMarkdown(markdown);
  const inserted = engine.parseMarkdown(replacement);
  const transformed = new Transform(doc);
  const pointPosition = (): number => {
    switch (point) {
      case "doc-start": return 0;
      case "doc-end": return doc.content.size;
      case "after": return quotePosition(doc, at!) + at!.length;
      case "before": return quotePosition(doc, at!);
      case "after-textblock": return doc.resolve(quotePosition(doc, at!)).after();
      case "before-textblock": return doc.resolve(quotePosition(doc, at!)).before();
    }
  };
  const fromPos = point ? pointPosition() : quotePosition(doc, from!);
  const toPos = point ? fromPos : quotePosition(doc, to!) + to!.length;
  const slice = inline
    ? inserted.slice(1, inserted.content.size - 1)
    : inserted.slice(0, inserted.content.size);
  transformed.replaceRange(fromPos, toPos, slice);
  return {
    name,
    markdown,
    from,
    to,
    at,
    point,
    replacement,
    pm_json: transformed.doc.toJSON(),
  };
});
// Cases conform to the pmdoc/replace-range-oracle/v1 schema: {schema, cases},
// each case a browser-oracle ProseMirror replaceRange result Splice must match.
const replaceRangeOracle = {
  schema: "pmdoc/replace-range-oracle/v1",
  cases: replaceRangeCases,
};
const nextSplices = JSON.stringify(replaceRangeOracle, null, 2) + "\n";

function quotePosition(doc: ProseMirrorNode, quote: string): number {
  let result = -1;
  doc.descendants((node, pos) => {
    if (node.isText) {
      const offset = node.text?.indexOf(quote) ?? -1;
      if (offset >= 0) {
        result = pos + offset;
        return false;
      }
    }
    return result < 0;
  });
  if (result < 0) throw new Error(`quote not found: ${quote}`);
  return result;
}
const next = JSON.stringify(fixtures, null, 2) + "\n";
if (check) {
  const current = readFileSync(out, "utf8");
  const currentSplices = readFileSync(spliceOut, "utf8");
  if (current !== next || currentSplices !== nextSplices) {
    console.error("generated pmdoc fixtures are stale: run `bun run gen`");
    process.exit(1);
  }
  console.log(`fixtures up to date (${fixtures.length} documents, ${replaceRangeCases.length} splice cases)`);
} else {
  writeFileSync(out, next);
  writeFileSync(spliceOut, nextSplices);
  console.log(`wrote ${fixtures.length} documents and ${replaceRangeCases.length} splice cases`);
}
setBlockIdGenerator(null);
