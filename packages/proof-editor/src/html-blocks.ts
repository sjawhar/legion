import type { Root } from "@milkdown/kit/transformer";
import { $remark } from "@milkdown/kit/utils";
import { codes } from "micromark-util-symbol";
import type { Extension } from "micromark-util-types";
import type { Processor, Transformer } from "unified";

import { htmlFlowStrict } from "./html-flow.js";
import type { MdastNode } from "./typed-block-drawing.js";

/**
 * How this package reads a block of HTML: the construct that opens one (`strictHtmlFlowSyntax`,
 * `htmlFlowStrict` from `./html-flow` in place of micromark's own) and the transform that draws
 * one (`remarkHtmlBlockParagraphs`), with the `$remark` plugin each parse path installs. Both are
 * legion's own, so they live here rather than in the copied `./lib-remark-directive-plugin`
 * (#2507).
 */

/**
 * The micromark extension that reads an HTML block the way CommonMark and the Dispatch server do:
 * `htmlFlowStrict` from `./html-flow` under `<`, with micromark's own `htmlFlow` disabled by name,
 * since a construct whose name micromark finds in `disable.null` is skipped.
 *
 * micromark opens a block of one complete tag (an HTML block of kind 7) on a paragraph's
 * continuation line when that line is lazy. CommonMark forbids a type-7 block from interrupting a
 * paragraph at all, and goldmark, the server's reader, obeys that: the server reads `> a` then
 * `<b>` as one quoted paragraph holding `a ` and the inline HTML. Here the block exited the
 * containers behind the lazy line, and milkdown, which has no node for a block of HTML there,
 * dropped the block with its container and the container's text, so a reader saw neither the quote
 * nor the word `a`. The copy's one changed line is what makes the two readers agree; every other
 * kind of HTML block, and a kind-7 block that opens a paragraph of its own, reads as before.
 */
export const strictHtmlFlowSyntax: Extension = {
  flow: { [codes.lessThan]: htmlFlowStrict },
  disable: { null: ["htmlFlow"] },
};

/**
 * unified plugin: the HTML (flow) construct above, in place of micromark's. The list is made
 * before it is pushed to, rather than in the push's own expression as the copied plugin beside it
 * writes it (`./lib-remark-directive-plugin`), because this module is biome-checked and its lint
 * refuses an assignment inside an expression.
 */
export function remarkStrictHtmlFlow(this: Processor): void {
  const data = this.data();
  data.micromarkExtensions ??= [];
  data.micromarkExtensions.push(strictHtmlFlowSyntax);
}

export const libraryRemarkHtmlFlowPlugin = $remark(
  "remarkStrictHtmlFlow",
  () => remarkStrictHtmlFlow
);

/**
 * The mdast nodes whose children are phrasing content: an `html` child of one is a tag inside a
 * line of text, which the schema's inline `html` node holds. Every other node this parse builds
 * with children holds flow content, so an `html` child of it is a block of HTML.
 */
const phrasingParents: Record<string, true> = { paragraph: true, heading: true, tableCell: true };

/**
 * Each block of HTML in `nodes`, at any depth, replaced in place by a paragraph whose one child
 * is that block as an inline `html` node, keeping its position so a later transform reading the
 * source offsets (./typed-block-drawing) still finds the lines it was cut from.
 *
 * The up-to-three spaces a block of HTML may open with are block syntax, not content: micromark
 * takes them into the block's own token (`resolveToHtmlFlow`), and a paragraph of the same text
 * holds none of them, because a paragraph's leading whitespace is not its text. Keeping them
 * would draw ` </script>` as a tag with a space before it where the Dispatch server, reading the
 * line as the inline HTML it is, draws the tag alone. Only the first line's indentation goes; a
 * later line's belongs to the block's text.
 */
function drawHtmlBlocks(nodes: MdastNode[]): void {
  for (const [index, node] of nodes.entries()) {
    if (node.type === "html") {
      const tag = { ...node, value: (node.value ?? "").replace(/^[ \t]+/, "") };
      nodes[index] = { type: "paragraph", children: [tag], position: node.position };
    } else if (node.children !== undefined && phrasingParents[node.type] !== true) {
      drawHtmlBlocks(node.children);
    }
  }
}

/**
 * unified plugin: a block of HTML drawn as a paragraph of the same text.
 *
 * The Proof schema has no node for a block of HTML and gains none; it has the inline `html` node
 * this package already draws for a tag inside a paragraph. Milkdown, given an mdast `html` node
 * in block position, builds that inline node where only a block may stand, so the parse threw
 * `Cannot create node for doc` and a reader of the comment saw the whole body fail to draw — or,
 * inside a block quote, a list item or a footnote definition, Milkdown dropped the container with
 * the block and every word around it. Every comment and every past-version view in Dispatch is
 * drawn by this engine from the bytes as written, and those bytes are written by any person or
 * agent, so `<div>` on a line of its own has to draw (#2507).
 *
 * Wrapping the node keeps the block's text exactly as micromark read it, one `html` node per
 * block however many lines it spans, so the paragraph serializes back to the same markdown; it
 * keeps the container around it; and it leaves both the copied construct (./html-flow) and the
 * schema alone. `remarkUnfitTypedBlocks` (./typed-block-drawing) is the precedent: a node the
 * schema cannot hold is made drawable before the parse reaches it, rather than refused.
 *
 * Where it is registered is load-bearing at both ends. It runs after the mark plugins, since a
 * record mark covering whole blocks is written as a `<span …>` line and a `</span>` line, two
 * blocks of HTML until `remarkProofMarks` and `remarkDispatchMarks` have paired them into one
 * mark node; a paragraph drawn around either line leaves the mark unread and its two empty
 * paragraphs in the document. And it runs before the typed-block transforms, which check a
 * container directive's content against the node types its rule names: a paragraph fits a block
 * rule where an inline `html` node does not, so a `:::callout` holding a block of HTML stays the
 * typed block it is.
 */
export function remarkHtmlBlockParagraphs() {
  return (tree: MdastNode): void => {
    if (tree.children !== undefined) drawHtmlBlocks(tree.children);
  };
}

export const libraryRemarkHtmlBlockPlugin = $remark(
  "remarkHtmlBlockParagraphs",
  () => () => remarkHtmlBlockParagraphs() as unknown as Transformer<Root, Root>
);
