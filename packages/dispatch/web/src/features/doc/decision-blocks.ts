import { BLANK_LINE, closingFenceAfter, codeFenceEnd } from "./document-blocks";

/**
 * `markdown` with every decision block cut out, on lines, so two versions of a document that
 * differ only in their decision blocks - an answer recorded in a block's attributes, a block
 * added or removed - cut to the same text, and a diff of the two shows only what changed
 * outside them.
 *
 * A block runs from its opening fence through its closing fence (`closingFenceAfter`), wherever
 * it stands: at the top level, inside a blockquote, a list item or another typed block. The one
 * blank line after the closing fence goes with it, or the one before the opening fence when the
 * block ends the document, so the text around it closes up as if the block had never been
 * written. A block no later line closes is left as it is, and a fenced code block is kept whole
 * (`codeFenceEnd`): what it spells is text, not a block. The Go server cuts the parsed tree
 * (`packages/envoy/internal/dispatch/docs/approval_carry.go`, `CutDecisionBlocks`); the two need
 * not agree byte for byte, but over the fixtures in `packages/contracts/fixtures/decision-blocks`
 * both give the same answer to whether two versions differ outside their blocks. The fence rules
 * are `document-blocks.ts`'s, so this cut and the block split the change view reads
 * (`splitBlocks`) see the same fences.
 */
export function cutDecisionBlocks(markdown: string): string {
  const endsWithNewline = markdown.endsWith("\n");
  const lines = (endsWithNewline ? markdown.slice(0, -1) : markdown).split("\n");
  const kept: string[] = [];
  let i = 0;
  while (i < lines.length) {
    const codeEnd = codeFenceEnd(lines, i);
    if (codeEnd !== undefined) {
      kept.push(...lines.slice(i, codeEnd + 1));
      i = codeEnd + 1;
      continue;
    }
    const end = closingFenceAfter(lines, i);
    if (end === undefined) {
      kept.push(lines[i]);
      i += 1;
      continue;
    }
    const next = end + 1;
    if (next < lines.length) {
      i = BLANK_LINE.test(lines[next]) ? next + 1 : next;
      continue;
    }
    if (kept.length > 0 && BLANK_LINE.test(kept[kept.length - 1])) kept.pop();
    i = next;
  }
  const cut = kept.join("\n");
  return endsWithNewline && cut !== "" ? `${cut}\n` : cut;
}
