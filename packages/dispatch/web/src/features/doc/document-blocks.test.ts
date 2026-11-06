import { expect, test } from "bun:test";

import { leadingBlocks, splitBlocks } from "./document-blocks";

test("a fenced code block holding a decision block as an example stays one code block", () => {
  const fenced = [
    "```markdown",
    ':::ask{#example urgency="med"}',
    "Which region goes first?",
    "",
    "- West: The busy one.",
    ":::",
    "```",
  ].join("\n");
  const blocks = splitBlocks(`Before.\n\n${fenced}\n\nAfter.\n`);

  expect(blocks.map((block) => block.kind)).toEqual(["paragraph", "code", "paragraph"]);
  expect(blocks[1].source).toBe(fenced);
});

test("a decision block holding a nested colon run stays one ask block", () => {
  const ask = [
    '::::ask{#outer urgency="med"}',
    "Which region goes first?",
    "",
    ':::callout{kind="note"}',
    "The west is the busy one.",
    ":::",
    "::::",
  ].join("\n");
  const blocks = splitBlocks(`${ask}\n\nAfter.\n`);

  expect(blocks.map((block) => block.kind)).toEqual(["ask", "paragraph"]);
  expect(blocks[0].source).toBe(ask);
});

test("a table is one block, its header row with it", () => {
  const table = ["| Region | Wave |", "| --- | --- |", "| West | 1 |", "| East | 2 |"].join("\n");
  const blocks = splitBlocks(`${table}\n`);

  expect(blocks).toEqual([{ source: table, kind: "table" }]);
});

test("a list is one block, each of its items with it", () => {
  const list = ["- Announce the dates.", "- Pick the regions.", "  - The two busiest."].join("\n");
  const blocks = splitBlocks(`${list}\n`);

  expect(blocks).toEqual([{ source: list, kind: "list" }]);
});

test("a heading is one block at every depth, whatever follows it", () => {
  const blocks = splitBlocks("# Rollout plan\n\nThe plan.\n\n###### Footnote\n");

  expect(blocks).toEqual([
    { source: "# Rollout plan", kind: "heading" },
    { source: "The plan.", kind: "paragraph" },
    { source: "###### Footnote", kind: "heading" },
  ]);
});

test("every kind of block is cut out, and the blocks re-join to the document", () => {
  const document = [
    "# Rollout plan",
    "",
    "The service ships behind a flag.",
    "",
    "## Decisions",
    "",
    "- Announce the dates.",
    "- Pick the regions.",
    "",
    "| Region | Wave |",
    "| --- | --- |",
    "| West | 1 |",
    "",
    "```ts",
    "const wave = 1;",
    "```",
    "",
    ':::ask{#wave urgency="med"}',
    "Which region goes first?",
    ":::",
    "",
    "> The platform team asked for a rollback path.",
    "",
    "The last paragraph closes the plan.",
    "",
  ].join("\n");
  const blocks = splitBlocks(document);

  expect(blocks.map((block) => block.kind)).toEqual([
    "heading",
    "paragraph",
    "heading",
    "list",
    "table",
    "code",
    "ask",
    "other",
    "paragraph",
  ]);
  expect(`${blocks.map((block) => block.source).join("\n\n")}\n`).toBe(document);
});

test("a document with nothing in it has no blocks", () => {
  expect(splitBlocks("")).toEqual([]);
  expect(splitBlocks("\n\n")).toEqual([]);
});

test("the first blocks under the budget are what a document opens with, cut at a block boundary", () => {
  const document = [
    "# The lease ledger",
    "",
    "The first paragraph, which is the one a reader meets.",
    "",
    "The second paragraph, which is the one that does not fit.",
    "",
    "The third paragraph, which is further still.",
    "",
  ].join("\n");

  expect(leadingBlocks(document, 80)).toBe(
    "# The lease ledger\n\nThe first paragraph, which is the one a reader meets."
  );
});

test("a document whose every block fits is its own first blocks, to the character", () => {
  const document = "# The lease ledger\n\nOne short paragraph.\n";

  expect(leadingBlocks(document, 2_000)).toBe(document);
});

test("the first block comes whole however long it is, and a fence inside it with it", () => {
  const fenced = ["```markdown", "# Not a heading of its own", "", "- An item", "```"].join("\n");
  const document = `${fenced}\n\nThe paragraph after it.\n`;

  expect(leadingBlocks(document, 10)).toBe(fenced);
});

test("a document with nothing in it has nothing to open with", () => {
  expect(leadingBlocks("", 2_000)).toBe("");
  expect(leadingBlocks("\n\n", 2_000)).toBe("\n\n");
});
