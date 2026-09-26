// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import { $remark } from '@milkdown/kit/utils';
import { directiveFromMarkdown, directiveToMarkdown } from 'mdast-util-directive';
import { directive } from 'micromark-extension-directive';
import type { Construct, Extension } from 'micromark-util-types';
import type { Processor } from 'unified';

import { remarkTypedBlocks, type BlockSchema } from './block-schema.js';

/**
 * The document grammar shared with the Dispatch server has exactly one directive form: the
 * `:::name{...}` container that carries a typed block. remark-directive's full grammar also
 * accepts leaf (`::name`) and text (`:name`) directives, and its text form takes `:` followed by
 * any non-punctuation character, so ordinary prose such as `16:25Z`, `a:b` or `:176-177` parses
 * as a directive; the server stores that text verbatim, so a document it accepted must read back
 * here as the same literal text. Registering only the container construct makes those characters
 * plain text at the tokenizer, the way the server reads them, rather than directive nodes a later
 * pass has to undo (a leaf directive interrupts a paragraph, so undoing it after the fact would
 * still leave one paragraph split in three).
 */
function containerDirectiveSyntax(): Extension {
  const flow = directive().flow?.[58];
  const constructs: Construct[] = Array.isArray(flow) ? flow : flow ? [flow] : [];
  // The container is the fenced construct: its body cannot be lazily continued, which micromark
  // expresses as `concrete`; the leaf construct is the other entry.
  const container = constructs.filter((construct) => construct.concrete === true);
  if (container.length !== 1) {
    throw new Error(`micromark-extension-directive: expected one container construct, found ${container.length}`);
  }
  return { flow: { 58: container } };
}

/** unified plugin: remark-directive restricted to `:::name{...}` container blocks. */
export function remarkContainerDirectives(this: Processor): void {
  const data = this.data();
  (data.micromarkExtensions ??= []).push(containerDirectiveSyntax());
  (data.fromMarkdownExtensions ??= []).push(directiveFromMarkdown());
  (data.toMarkdownExtensions ??= []).push(directiveToMarkdown());
}

export const libraryRemarkDirectivePlugin = $remark('remarkDirective', () => remarkContainerDirectives);

export function typedBlockRemarkPlugin(schema: BlockSchema) {
  return $remark('remarkTypedBlocks', () => () => remarkTypedBlocks(schema));
}
