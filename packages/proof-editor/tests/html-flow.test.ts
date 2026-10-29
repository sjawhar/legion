/**
 * An HTML block of one complete tag (kind 7) never interrupts a paragraph, on a lazy line as on
 * any other, which is CommonMark's rule and the one goldmark — the Dispatch server's reader —
 * follows. micromark made an exception for a lazy line, so `<b>` on the continuation line of
 * `> a` opened a block, which exits the containers behind the lazy line; milkdown has no node for
 * a block of HTML there, so it dropped the block, its container and the container's text, and a
 * reader of the comment saw neither the quote nor the word `a`. `src/html-flow.ts` is the copy of
 * micromark's construct whose one changed line closes that, registered the way the container
 * directive's own copy is (#2403).
 *
 * Every other kind of HTML block is untouched: a kind-7 block that opens a paragraph of its own
 * still opens, and kinds 1-6 still interrupt a paragraph on any line. The schema has no
 * block-level HTML node, so a parse that opens a block of HTML at document level throws naming
 * what the document would have held; those cases assert that message, which is how they read
 * before this change too.
 */

import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { createHeadlessProof } from "../src/lib-headless.js";

/**
 * `node` as nested `type[child, ...]` text: a text node is its quoted string and an inline HTML
 * node its quoted value, so a tree reads as the markdown it came from without its block ids.
 */
function outline(node: ProseMirrorNode): string {
  if (node.type.name === "text") return JSON.stringify(node.text);
  if (node.type.name === "html") return `html(${JSON.stringify(node.attrs.value)})`;
  const children: string[] = [];
  node.forEach((child) => {
    children.push(outline(child));
  });
  return `${node.type.name}[${children.join(", ")}]`;
}

const quotedParagraph = 'blockquote[paragraph["a ", html("<b>")]]';
const reference = 'paragraph["Ref", footnote_reference[], "."]';
const notedDefinition = 'footnote_definition[paragraph["note ", html("<b>")]]';

/** Each case the lazy-line exception got wrong: the text the reader lost is back, in place. */
const lazy: Array<[string, string, string]> = [
  ["a block quote keeps its text and its HTML", "> a\n<b>\n", `doc[${quotedParagraph}]`],
  [
    "a list item keeps its text and its HTML",
    "- a\n<b>\n",
    'doc[bullet_list[list_item[paragraph["a ", html("<b>")]]]]',
  ],
  [
    "a footnote definition keeps its text and its HTML",
    "Ref[^f].\n\n[^f]: note\n<b>\n",
    `doc[${reference}, ${notedDefinition}]`,
  ],
  [
    "a quoted footnote definition keeps its text and its HTML",
    "Ref[^f].\n\n>[^f]: note\n>    <b>\n",
    `doc[${reference}, blockquote[${notedDefinition}]]`,
  ],
];

for (const [name, markdown, tree] of lazy) {
  test(name, async () => {
    const proof = await createHeadlessProof();
    expect(outline(proof.parseMarkdown(markdown))).toBe(tree);
  });
}

/** The readings that must not change: the copy is one line, not a disabled construct. */
const unchanged: Array<[string, string, string]> = [
  [
    "a complete tag that opens a paragraph of its own is still an HTML block",
    "<x>\ny\n",
    '[Content]: ["html"]',
  ],
  ["a basic tag at document level is still an HTML block", "<div>\n", '[Content]: ["html"]'],
  [
    "a comment still interrupts a quoted paragraph",
    "> a\n<!-- c -->\n",
    '[Content]: ["blockquote(paragraph(\\"a\\"))", "html"]',
  ],
];

for (const [name, markdown, content] of unchanged) {
  test(name, async () => {
    const proof = await createHeadlessProof();
    const parse = () => {
      proof.parseMarkdown(markdown);
    };
    // The block of HTML stands at document level, where the schema has no node for it: what the
    // refusal names is what the block swallowed.
    expect(parse).toThrow(/Cannot create node for doc/);
    expect(parse).toThrow(content);
  });
}

test("a written continuation prefix reads as it did", async () => {
  const proof = await createHeadlessProof();
  expect(outline(proof.parseMarkdown("> a\n> <b>\n"))).toBe(`doc[${quotedParagraph}]`);
});

test("a paragraph in no container reads as it did", async () => {
  const proof = await createHeadlessProof();
  expect(outline(proof.parseMarkdown("a\n<b>\n"))).toBe('doc[paragraph["a ", html("<b>")]]');
});
