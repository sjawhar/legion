/**
 * The headless parser's read-only mode draws a container directive the block schema refuses as
 * its literal fence lines around its formatted content, and leaves the paragraph-line directive
 * refusals unapplied, so a displayed body formats instead of falling back to its raw source. The
 * default mode keeps every refusal.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
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

test("read-only keeps every line of a typed block whose content a drawn directive would break", async () => {
  // The server's ask takes `paragraph+ bullet_list?`. Drawing the unknown :::note after the list
  // puts paragraphs where the ask allows none; Milkdown then drops the whole ask and the reader
  // sees none of its text, where the default mode's refusal at least showed the raw source.
  const proof = await createHeadlessProof({
    blockSchema: {
      version: 1,
      types: [{ name: "ask", content: "paragraph+ bullet_list?", render: "host", attributes: {} }],
    },
    readOnly: true,
  });
  const doc = proof.parseMarkdown(
    "Lead\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n"
  );
  for (const line of ["Lead", "Which option?", "first", ":::note", "A note line"]) {
    expect(doc.textContent).toContain(line);
  }
});

test("a refused directive without source offsets throws", async () => {
  const { schema } = await createHeadlessProof({ blockSchema });
  const transform = remarkReadOnlyDirectives(blockSchema, schema);
  const tree = { type: "root", children: [{ type: "containerDirective", name: "note" }] };
  expect(() => transform(tree, { toString: () => "" })).toThrow(
    'container directive "note" has no source position'
  );
});

const askBlockSchema: BlockSchema = {
  version: 1,
  types: [
    ...blockSchema.types,
    { name: "ask", content: "paragraph+ bullet_list?", render: "host", attributes: {} },
  ],
};

async function readOnlyAskParse(markdown: string) {
  const proof = await createHeadlessProof({ blockSchema: askBlockSchema, readOnly: true });
  return proof.parseMarkdown(markdown);
}

test("read-only draws a typed block whose content breaks its rule as text", async () => {
  const doc = await readOnlyAskParse(":::ask{#a1}\n# Heading\n\nBody text\n:::\n");
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::ask{#a1}"],
    ["heading", "Heading"],
    ["paragraph", "Body text"],
    ["paragraph", ":::"],
  ]);
});

test("read-only keeps a typed block whose content fits its rule typed", async () => {
  const doc = await readOnlyAskParse(":::ask{#a1}\nWhich option?\n\n- first\n- second\n:::\n");
  expect(blocks(doc)).toEqual([["ask", "Which option?firstsecond"]]);
  expect(blocks(doc.child(0)).map(([type]) => type)).toEqual(["paragraph", "bullet_list"]);
  expect(doc.child(0).attrs.blockId).toBe("a1");
});

test("read-only draws the drawn directive and the typed block it breaks, in order", async () => {
  const doc = await readOnlyAskParse(
    "Lead\n\n::::ask\nWhich option?\n\n- first\n\n:::note\nA note line\n:::\n::::\n"
  );
  expect(blocks(doc)).toEqual([
    ["paragraph", "Lead"],
    ["paragraph", "::::ask"],
    ["paragraph", "Which option?"],
    ["bullet_list", "first"],
    ["paragraph", ":::note"],
    ["paragraph", "A note line"],
    ["paragraph", ":::"],
    ["paragraph", "::::"],
  ]);
});

test("read-only draws an outer typed block a drawn inner block's content breaks", async () => {
  const doc = await readOnlyAskParse("::::callout\n:::ask\n# Heading\n\nBody\n:::\n::::\n");
  expect(blocks(doc)).toEqual([
    ["paragraph", "::::callout"],
    ["paragraph", ":::ask"],
    ["heading", "Heading"],
    ["paragraph", "Body"],
    ["paragraph", ":::"],
    ["paragraph", "::::"],
  ]);
});

test("read-only keeps an outer typed block typed when a drawn directive's lines fit it", async () => {
  const doc = await readOnlyAskParse("::::callout\n:::note\ninner\n:::\n::::\n");
  expect(blocks(doc)).toEqual([["callout", ":::noteinner:::"]]);
  expect(blocks(doc.child(0))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "inner"],
    ["paragraph", ":::"],
  ]);
});

/** The document service's own block schema, the one Dispatch parses its bodies with. */
const serverBlockSchema: BlockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);

async function readOnlyServerParse(markdown: string) {
  const proof = await createHeadlessProof({ blockSchema: serverBlockSchema, readOnly: true });
  return proof.parseMarkdown(markdown);
}

test("read-only ends a drawn directive's lines at a lone carriage return", async () => {
  const doc = await readOnlyServerParse("Intro\n\n:::a\rbody\r:::\n\nAfter");
  expect(blocks(doc)).toEqual([
    ["paragraph", "Intro"],
    ["paragraph", ":::a"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
    ["paragraph", "After"],
  ]);
});

test("read-only draws each carriage-return directive's own lines only", async () => {
  const doc = await readOnlyServerParse(":::a\r:::\r".repeat(3));
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::a"],
    ["paragraph", ":::"],
    ["paragraph", ":::a"],
    ["paragraph", ":::"],
    ["paragraph", ":::a"],
    ["paragraph", ":::"],
  ]);
  // Each opening line used to run to the next "\n", here the end of the body, so the drawn text
  // grew with the square of the body's length.
  const source = ":::a\r:::\r".repeat(3555);
  expect((await readOnlyServerParse(source)).textContent.length).toBeLessThan(2 * source.length);
});

test("read-only draws a carriage-return directive in a list item once", async () => {
  const doc = await readOnlyServerParse("- :::a\r  body\r  :::\r");
  expect(blocks(doc).map(([type]) => type)).toEqual(["bullet_list"]);
  const item = doc.child(0).child(0);
  expect(item.type.name).toBe("list_item");
  expect(blocks(item)).toEqual([
    ["paragraph", ":::a"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only draws a carriage-return directive in a blockquote once", async () => {
  const doc = await readOnlyServerParse("> :::a\r> body\r> :::\r");
  expect(blocks(doc)).toEqual([["blockquote", ":::abody:::"]]);
  expect(blocks(doc.child(0))).toEqual([
    ["paragraph", ":::a"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only checks the blocks inside a block-level mark span against the content rule", async () => {
  // The span wraps the heading in a mark node; Milkdown runs the mark's children into the ask
  // itself, so the heading breaks the ask's `paragraph+ bullet_list?` and the ask is drawn.
  const doc = await readOnlyServerParse(
    ':::ask\n<span data-dispatch="ask" data-id="x">\n\n# Heading words\n\n</span>\n:::'
  );
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::ask"],
    ["heading", "Heading words"],
    ["paragraph", ":::"],
  ]);
  expect(runs(doc.child(1))).toEqual([{ text: "Heading words", marks: ["dispatchAsk"] }]);
});

test("read-only draws no closing fence from a last line ending in one colon", async () => {
  expect(blocks(await readOnlyServerParse(":::note\nbody\n\n[x]: mailto:"))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
  ]);
});

test("read-only draws no closing fence from a last line with text before its colons", async () => {
  expect(blocks(await readOnlyServerParse(":::note\nbody\n\n[x]: https://e.x/a:::"))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
  ]);
});

test("read-only draws no closing fence from a colon run shorter than the opening one", async () => {
  // `:::` is the definition's destination: a run shorter than `::::` does not close the block.
  expect(blocks(await readOnlyServerParse("::::note\nbody\n\n[x]:\n:::"))).toEqual([
    ["paragraph", "::::note"],
    ["paragraph", "body"],
  ]);
});

test("read-only draws no closing fence from a line inside a mark span's last block", async () => {
  // The unclosed span wraps the blockquote in a mark node with no source position of its own;
  // the `:::` is the quote's text, not the directive's fence.
  const doc = await readOnlyServerParse(':::note\n<span data-dispatch="ask" data-id="x">\n\n> :::');
  expect(blocks(doc)).toEqual([
    ["paragraph", ":::note"],
    ["blockquote", ":::"],
  ]);
});

test("read-only draws a directive after a leading byte-order mark from its own lines", async () => {
  // micromark drops one leading U+FEFF before it counts offsets, so the transform cuts the lines
  // from the source without it.
  expect(blocks(await readOnlyServerParse("\ufeff:::note\nbody\n:::\n"))).toEqual([
    ["paragraph", ":::note"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});

test("read-only keeps every line of a body that starts with a byte-order mark", async () => {
  const errors: unknown[][] = [];
  const consoleError = console.error;
  console.error = (...args: unknown[]) => {
    errors.push(args);
  };
  try {
    const doc = await readOnlyServerParse("\ufeffHello **b**\n\n:::note\nbody\n:::\n");
    expect(blocks(doc)).toEqual([
      ["paragraph", "Hello b"],
      ["paragraph", ":::note"],
      ["paragraph", "body"],
      ["paragraph", ":::"],
    ]);
    // remark drops the mark from the first paragraph's text itself, as the default mode does.
    const expected = [
      { text: "Hello ", marks: [] },
      { text: "b", marks: ["strong"] },
    ];
    expect(runs(doc.child(0))).toEqual(expected);
    const defaultMode = await createHeadlessProof({ blockSchema: serverBlockSchema });
    expect(runs(defaultMode.parseMarkdown("\ufeffHello **b**\n").child(0))).toEqual(expected);
  } finally {
    console.error = consoleError;
  }
  expect(errors).toEqual([]);
});

test("read-only draws a directive inside a mark span inside a refused directive once", async () => {
  const doc = await readOnlyServerParse(
    '::::note\n<span data-dispatch="ask" data-id="x">\n\n:::zzz\nbody\n:::'
  );
  expect(blocks(doc)).toEqual([
    ["paragraph", "::::note"],
    ["paragraph", ":::zzz"],
    ["paragraph", "body"],
    ["paragraph", ":::"],
  ]);
});
