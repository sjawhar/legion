import { $remark } from '@milkdown/kit/utils';
import { directiveFromMarkdown, directiveToMarkdown } from 'mdast-util-directive';
import type { Processor } from 'unified';

import { directiveContainerSyntax } from './directive-container.js';

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
