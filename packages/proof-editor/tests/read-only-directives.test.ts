/**
 * The headless parser's read-only mode draws a container directive the block schema refuses as
 * its literal fence lines around its formatted content, and leaves the paragraph-line directive
 * refusals unapplied, so a displayed body formats instead of falling back to its raw source. The
 * default mode keeps every refusal.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/block-schema.js";
import { createHeadlessProof, type SoftBreaks } from "../src/lib-headless.js";
import { remarkReadOnlyDirectives } from "../src/read-only-directives.js";

const blockSchema: BlockSchema = {
  version: 1,
  types: [
    {
      name: "callout",
      content: "paragraph+",
      render: "host",
      attributes: {
        kind: { kind: "enum", choices: ["info", "warning"], default: "info" },
      },
    },
  ],
};

async function readOnlyParse(markdown: string, softBreaks?: SoftBreaks) {
  const proof = await createHeadlessProof({ blockSchema, readOnly: true });
  return proof.parseMarkdown(markdown, softBreaks);
}

/** Each child block of `node` as its type name and its text. */
function blocks(node: ProseMirrorNode): [string, string][] {
  const out: [string, string][] = [];
  node.forEach((child) => {
    out.push([child.type.name, child.textContent]);
  });
  return out;
}

/** Each inline child of `node` as its text and the names of its marks. */
function runs(node: ProseMirrorNode): { text: string; marks: string[] }[] {
  const out: { text: string; marks: string[] }[] = [];
  node.forEach((child) => {
    out.push({ text: child.text ?? "", marks: child.marks.map((mark) => mark.type.name) });
  });
  return out;
}

test("the default mode still refuses a directive the schema does not know", async () => {
  const proof = await createHeadlessProof({ blockSchema });
  expect(() => proof.parseMarkdown(":::note\nA note line\n:::\n")).toThrow(
    'unknown typed block "note"'
  );
});

test("read-only draws an unknown directive's fences as text and formats the rest", async () => {
  for (const softBreaks of ["space", "line"] as const) {
    const doc = await readOnlyParse(
      "Some **bold** text\n\n:::note\nA note line\n:::\n",
      softBreaks
    );
    expect({ softBreaks, blocks: blocks(doc) }).toEqual({
      softBreaks,
      blocks: [
        ["paragraph", "Some bold text"],
        ["paragraph", ":::note"],
        ["paragraph", "A note line"],
        ["paragraph", ":::"],
      ],
    });
    expect({ softBreaks, runs: runs(doc.child(0)) }).toEqual({
      softBreaks,
      runs: [
        { text: "Some ", marks: [] },
        { text: "bold", marks: ["strong"] },
        { text: " text", marks: [] },
      ],
    });
  }
});

test("read-only formats the content inside a refused directive", async () => {
  const doc = await readOnlyParse(":::note\nSome **inner** text\n:::\n");
  expect(blocks(doc).map(([type]) => type)).toEqual(["paragraph", "paragraph", "paragraph"]);
  expect(runs(doc.child(1))).toEqual([
    { text: "Some ", marks: [] },
    { text: "inner", marks: ["strong"] },
    { text: " text", marks: [] },
  ]);
});

test("read-only draws no closing fence for an unclosed directive", async () => {
  const doc = await readOnlyParse(":::note\nbody");
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
  ]);
});

test("read-only keeps a refused directive's label on its opening line only", async () => {
  const doc = await readOnlyParse(":::note[Heads up]{.x}\nbody\n:::");
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::note[Heads up]{.x}"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only draws a refused directive's fences without the blockquote prefix", async () => {
  const doc = await readOnlyParse("> :::note\n> body\n> :::");
  expect(blocks(doc)).toEqual([["blockquote", ":::notebody:::"]]);
  expect(blocks(doc.child(0))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only draws a refused directive's fences without the list item indent", async () => {
  const doc = await readOnlyParse("- :::note\n  body\n  :::");
  expect(blocks(doc).map(([type]) => type)).toEqual(["bullet_list"]);
  const item = doc.child(0).child(0);
  expect(item.type.name).toBe("list_item");
  expect(blocks(item)).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only keeps a known typed block inside an unknown directive typed", async () => {
  const doc = await readOnlyParse("::::note\n:::callout{}\ninner\n:::\n::::\n");
  expect(blocks(doc)).toEqual([
    ["paragraph", "::::note"],
    ["callout", "inner"],
    ["paragraph", "::::"],
  ]);
});

test("read-only does not take a nested typed block's closing fence as an unclosed outer's", async () => {
  const doc = await readOnlyParse("::::note\n:::callout{}\ninner\n:::\n");
  expect(blocks(doc)).toEqual([
    ["paragraph", "::::note"],
    ["callout", "inner"],
  ]);
});

test("read-only leaves Pandoc, leaf and text directive lines as paragraph text", async () => {
  expect(blocks(await readOnlyParse("::: warning\nbody\n:::"))).toEqual([
    ["paragraph", "::: warning body :::"],
  ]);
  expect(blocks(await readOnlyParse("::youtube{id=1}"))).toEqual([
    ["paragraph", "::youtube{id=1}"],
  ]);
  expect(blocks(await readOnlyParse(":callout{#x}"))).toEqual([["paragraph", ":callout{#x}"]]);
});

test("read-only draws a known type with a refused attribute as text", async () => {
  const doc = await readOnlyParse(':::callout{bogus="1"}\nbody\n:::');
  expect(blocks(doc)).toEqual([
    ["paragraph", ':::callout{bogus="1"}'],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only keeps an accepted typed block typed", async () => {
  const doc = await readOnlyParse(':::callout{kind="warning"}\nbody\n:::');
  expect(blocks(doc)).toEqual([["callout", "body"]]);
  expect(doc.child(0).attrs.kind).toBe("warning");
});

test("read-only without a block schema draws every container directive as text", async () => {
  const proof = await createHeadlessProof({ readOnly: true });
  expect(blocks(proof.parseMarkdown(":::note\nbody\n:::"))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("a refused directive without source offsets throws", () => {
  const transform = remarkReadOnlyDirectives(blockSchema);
  const tree = { type: "root", children: [{ type: "containerDirective", name: "note" }] };
  expect(() => transform(tree, { toString: () => "" })).toThrow(
    'container directive "note" has no source position'
  );
});
