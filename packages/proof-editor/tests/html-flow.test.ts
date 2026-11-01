/**
 * How this package reads a block of HTML. The Proof schema has no node for one and gains none; it
 * has the inline `html` node a tag inside a paragraph is drawn as.
 *
 * Two readings meet here. micromark opened an HTML block of one complete tag (kind 7) on a
 * paragraph's lazy continuation line, which exits the containers behind that line; CommonMark
 * forbids a type-7 block from interrupting a paragraph at all, and goldmark — the Dispatch
 * server's reader — obeys that. `src/html-flow.ts` is the copy of micromark's construct whose one
 * changed line closes that, registered the way the container directive's own copy is (#2403).
 *
 * A block of HTML that does open then stands where only a block node may. Milkdown built the
 * inline `html` node there, so the parse threw `Cannot create node for doc` and the whole view
 * failed to draw rather than drawing — and inside a block quote, a list item or a footnote
 * definition it dropped the container with the block and every word around it, the same loss of a
 * reader's text the lazy line used to cause. `remarkHtmlBlockParagraphs` draws each block of HTML
 * as a paragraph whose one child is that block's text as an inline `html` node, so it draws in
 * place, keeps its container, and serializes back to the markdown it was written as (#2507).
 *
 * Every case runs in both engines this package builds: the read-write parse the editor shares and
 * the read-only parse Dispatch draws a comment and a past version with.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/block-schema.js";
import { createHeadlessProof, type HeadlessProofEditor } from "../src/lib-headless.js";

/**
 * `node` as nested `type[child, ...]` text: a text node is its quoted string, an inline HTML node
 * its quoted value, and a node under marks carries `<mark+mark>`, so a tree reads as the markdown
 * it came from without its block ids.
 */
function outline(node: ProseMirrorNode): string {
  const marks = node.marks.map((mark) => mark.type.name).join("+");
  const under = marks === "" ? "" : `<${marks}>`;
  if (node.type.name === "text") return `${JSON.stringify(node.text)}${under}`;
  if (node.type.name === "html") return `html(${JSON.stringify(node.attrs.value)})${under}`;
  const children: string[] = [];
  node.forEach((child) => {
    children.push(outline(child));
  });
  return `${node.type.name}${under}[${children.join(", ")}]`;
}

/** One typed block, taking any block, so a `:::` container can hold a block of HTML. */
const blockSchema: BlockSchema = {
  version: 1,
  types: [{ name: "callout", content: "block+", render: "host", attributes: {} }],
};

const engines = ["read-write", "read-only"] as const;

/**
 * The engine `mode` names, minting a fixed block id so a typed block's serialized opening line is
 * the same text on every run.
 */
function engine(mode: (typeof engines)[number]): Promise<HeadlessProofEditor> {
  return createHeadlessProof({ blockSchema, readOnly: mode === "read-only", blockId: () => "b1" });
}

/** A case's name, its markdown, the tree it draws, and the markdown that tree writes back. */
type Case = [name: string, markdown: string, tree: string, written: string];

const htmlParagraph = (value: string) => `paragraph[html(${JSON.stringify(value)})]`;

/**
 * A block of HTML standing at document level, of every kind micromark opens one for: it draws as
 * a paragraph of its own text, however many lines it spans, and writes back unchanged.
 */
const documentLevel: Case[] = [
  ["a lone open tag", "<div>\n", `doc[${htmlParagraph("<div>")}]`, "<div>\n"],
  [
    "an open and a close tag",
    "<div>\n</div>\n",
    `doc[${htmlParagraph("<div>\n</div>")}]`,
    "<div>\n</div>\n",
  ],
  ["a comment", "<!-- c -->\n", `doc[${htmlParagraph("<!-- c -->")}]`, "<!-- c -->\n"],
  [
    "one tag and its text",
    "<div>x</div>\n",
    `doc[${htmlParagraph("<div>x</div>")}]`,
    "<div>x</div>\n",
  ],
  [
    "a tag around its own lines",
    "<div>\nfoo\n</div>\n",
    `doc[${htmlParagraph("<div>\nfoo\n</div>")}]`,
    "<div>\nfoo\n</div>\n",
  ],
  [
    "a script",
    "<script>x</script>\n",
    `doc[${htmlParagraph("<script>x</script>")}]`,
    "<script>x</script>\n",
  ],
  ["a processing instruction", "<?x?>\n", `doc[${htmlParagraph("<?x?>")}]`, "<?x?>\n"],
  ["a declaration", "<!X>\n", `doc[${htmlParagraph("<!X>")}]`, "<!X>\n"],
  [
    "a CDATA section",
    "<![CDATA[x]]>\n",
    `doc[${htmlParagraph("<![CDATA[x]]>")}]`,
    "<![CDATA[x]]>\n",
  ],
  [
    "a block after a paragraph",
    "a\n\n<div>\n",
    `doc[paragraph["a"], ${htmlParagraph("<div>")}]`,
    "a\n\n<div>\n",
  ],
  // The up-to-three spaces a block may open with are block syntax: micromark takes them into the
  // block's token, and the Dispatch server, reading the same line as the inline HTML it is,
  // reads a paragraph whose leading whitespace is not its text. The drawn paragraph holds the
  // tag alone, so the two readers agree and the markdown writes back as it came.
  [
    "a block opening one space in",
    " </script>\n",
    `doc[${htmlParagraph("</script>")}]`,
    "</script>\n",
  ],
  ["a block opening three spaces in", "   <b>\n", `doc[${htmlParagraph("<b>")}]`, "<b>\n"],
  [
    "a block of more than one line keeps every line after its first",
    "  <div>\n  foo\n  </div>\n",
    `doc[${htmlParagraph("<div>\n  foo\n  </div>")}]`,
    "<div>\n  foo\n  </div>\n",
  ],
];

/**
 * A container holding nothing but a block of HTML keeps both: the container, which Milkdown used
 * to drop with it, and the block's text. A bullet list writes back with remark-stringify's `*`
 * marker, as every bullet list in this package does, and a typed block with its minted id.
 */
const containers: Case[] = [
  ["a block quote", "> <div>\n", `doc[blockquote[${htmlParagraph("<div>")}]]`, "> <div>\n"],
  [
    "a list item",
    "- <div>\n",
    `doc[bullet_list[list_item[${htmlParagraph("<div>")}]]]`,
    "* <div>\n",
  ],
  [
    "a footnote definition",
    "[^f]: <div>\n",
    `doc[footnote_definition[${htmlParagraph("<div>")}]]`,
    "[^f]: <div>\n",
  ],
  [
    "a typed block",
    ":::callout{}\n<div>\n:::\n",
    `doc[callout[${htmlParagraph("<div>")}]]`,
    ":::callout{#b1}\n<div>\n:::\n",
  ],
  [
    "a quoted block of two lines",
    "> <div>\n> more\n",
    `doc[blockquote[${htmlParagraph("<div>\nmore")}]]`,
    "> <div>\n> more\n",
  ],
];

const quotedParagraph = 'blockquote[paragraph["a ", html("<b>")]]';
const reference = 'paragraph["Ref", footnote_reference[], "."]';
const notedDefinition = 'footnote_definition[paragraph["note ", html("<b>")]]';

/** Each case the lazy-line exception got wrong: the text the reader lost is back, in place. */
const lazy: Case[] = [
  [
    "a block quote keeps its text and its HTML",
    "> a\n<b>\n",
    `doc[${quotedParagraph}]`,
    "> a <b>\n",
  ],
  [
    "a list item keeps its text and its HTML",
    "- a\n<b>\n",
    'doc[bullet_list[list_item[paragraph["a ", html("<b>")]]]]',
    "* a <b>\n",
  ],
  [
    "a footnote definition keeps its text and its HTML",
    "Ref[^f].\n\n[^f]: note\n<b>\n",
    `doc[${reference}, ${notedDefinition}]`,
    "Ref[^f].\n\n[^f]: note <b>\n",
  ],
  [
    "a quoted footnote definition keeps its text and its HTML",
    "Ref[^f].\n\n>[^f]: note\n>    <b>\n",
    `doc[${reference}, blockquote[${notedDefinition}]]`,
    "Ref[^f].\n\n> [^f]: note <b>\n",
  ],
];

/**
 * The tokenization that must not change: the copy is one line, not a disabled construct, and the
 * paragraph a block of HTML is drawn as stands exactly where the block did. A kind-7 block still
 * opens a paragraph of its own and takes the lines it swallows with it; a comment still interrupts
 * a quoted paragraph, which leaves the quote closed behind it; and a tag inside a line of text is
 * still read inline, in a paragraph and in a heading alike.
 */
const unchanged: Case[] = [
  [
    "a complete tag that opens a paragraph of its own is still one HTML block",
    "<x>\ny\n",
    `doc[${htmlParagraph("<x>\ny")}]`,
    "<x>\ny\n",
  ],
  [
    "a comment still interrupts a quoted paragraph",
    "> a\n<!-- c -->\n",
    `doc[blockquote[paragraph["a"]], ${htmlParagraph("<!-- c -->")}]`,
    "> a\n\n<!-- c -->\n",
  ],
  [
    "a written continuation prefix reads as it did",
    "> a\n> <b>\n",
    `doc[${quotedParagraph}]`,
    "> a <b>\n",
  ],
  [
    "a paragraph in no container reads as it did",
    "a\n<b>\n",
    'doc[paragraph["a ", html("<b>")]]',
    "a <b>\n",
  ],
  [
    "a tag around text in a line reads as it did",
    "a <b>x</b>\n",
    'doc[paragraph["a ", html("<b>"), "x", html("</b>")]]',
    "a <b>x</b>\n",
  ],
  [
    "a tag in a heading is still inline",
    "# <b>h</b>\n",
    'doc[heading[html("<b>"), "h", html("</b>")]]',
    "# <b>h</b>\n",
  ],
];

/**
 * A record mark covering whole blocks is written as a `<span …>` line and a `</span>` line, which
 * are two blocks of HTML until the mark plugins have paired them: drawing a paragraph around
 * either line first would leave the mark unread and both lines in the document, so the transform
 * runs after those plugins. The pair collapses to the one node it covers rather than standing as
 * two blocks of HTML of its own.
 *
 * A mark over a block of HTML alone then reaches no node that may carry it: the drawn paragraph
 * holds one inline `html` node, an inline leaf, and `withoutLeafMarks`
 * (`src/editor/schema/leaf-marks`) takes every mark off such a node, since Go's reader refuses a
 * mark on any node that is not text and this editor's own store drops one on the next round trip.
 * So the spans are read and consumed, and what stands is the block of HTML on its own.
 */
const markSpans: Case[] = [
  [
    "a block-level mark span is read before a block of HTML is drawn",
    '<span data-dispatch="ask" data-id="x">\n\n<div>\n\n</span>\n',
    'doc[paragraph[html("<div>")]]',
    "<div>\n",
  ],
  [
    "a block-level mark span around text reads as it did",
    '<span data-dispatch="ask" data-id="x">\n\nQ?\n\n</span>\n',
    'doc[paragraph["Q?"<dispatchAsk>]]',
    '<span data-dispatch="ask" data-id="x" data-by="unknown">Q?</span>\n',
  ],
];

for (const mode of engines) {
  for (const [name, markdown, tree, written] of [
    ...documentLevel,
    ...containers,
    ...lazy,
    ...unchanged,
    ...markSpans,
  ]) {
    test(`${name} (${mode})`, async () => {
      const proof = await engine(mode);
      const doc = proof.parseMarkdown(markdown);
      expect(outline(doc)).toBe(tree);
      expect(proof.serializeMarkdown(doc)).toBe(written);
    });
  }
}
