import { $remark } from '@milkdown/kit/utils';
import { directiveFromMarkdown, directiveToMarkdown } from 'mdast-util-directive';
import { codes } from 'micromark-util-symbol';
import type { Root } from '@milkdown/kit/transformer';
import type { Extension } from 'micromark-util-types';
import type { Processor, Transformer } from 'unified';

import { remarkTypedBlocks, type BlockSchema } from './block-schema.js';
import { directiveContainerSyntax } from './directive-container.js';
import { htmlFlowStrict } from './html-flow.js';

/**
 * unified plugin: `:::name{...}` container blocks, and no other directive.
 *
 * The document grammar shared with the Dispatch server has exactly one directive form: the
 * `:::name{...}` container that carries a typed block. remark-directive's full grammar also
 * accepts leaf (`::name`) and text (`:name`) directives, and its text form takes `:` followed by
 * any non-punctuation character, so ordinary prose such as `16:25Z`, `a:b` or `:176-177` parses
 * as a directive; the server stores that text verbatim, so a document it accepted must read back
 * here as the same literal text. Registering only a container construct makes those characters
 * plain text at the tokenizer, the way the server reads them, rather than directive nodes a later
 * pass has to undo (a leaf directive interrupts a paragraph, so undoing it after the fact would
 * still leave one paragraph split in three).
 *
 * The container construct is legion's own, `directiveContainerSyntax` from
 * `./directive-container`: a micromark document construct rather than
 * micromark-extension-directive's flow construct, which tokenized a block's content once per block
 * around it. It tokenizes the content once, and blocks nest at most `maxDirectiveNesting` (32)
 * deep, an opener inside that many reading as the text it is, which bounds what each line costs
 * (INFRA-581). It emits the package's token types, so `mdast-util-directive` builds the nodes from
 * them as before.
 */
export function remarkContainerDirectives(this: Processor): void {
  const data = this.data();
  (data.micromarkExtensions ??= []).push(directiveContainerSyntax);
  (data.fromMarkdownExtensions ??= []).push(directiveFromMarkdown());
  (data.toMarkdownExtensions ??= []).push(directiveToMarkdown());
}

export const libraryRemarkDirectivePlugin = $remark('remarkDirective', () => remarkContainerDirectives);

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
  disable: { null: ['htmlFlow'] }
};

/** unified plugin: the HTML (flow) construct above, in place of micromark's. */
export function remarkStrictHtmlFlow(this: Processor): void {
  const data = this.data();
  (data.micromarkExtensions ??= []).push(strictHtmlFlowSyntax);
}

export const libraryRemarkHtmlFlowPlugin = $remark('remarkStrictHtmlFlow', () => remarkStrictHtmlFlow);

export function typedBlockRemarkPlugin(schema: BlockSchema) {
  return $remark('remarkTypedBlocks', () => () => remarkTypedBlocks(schema) as unknown as Transformer<Root, Root>);
}
