/**
 * A decision block's opening fence: an optional prefix of spaces and `>` quote markers, a list
 * marker when the fence sits right after it on the item's first line, a run of three or more
 * colons, then `ask{`. The prefix and the colon count are what its closing fence repeats.
 */
const OPENING_FENCE = /^((?:[ \t]*>)*[ \t]*)((?:[-*+]|\d+[.)])[ \t]+)?(:{3,})ask\{/;

/**
 * A code fence's opening line: the same prefix and optional list marker, then a run of three or
 * more backticks or tildes and the info string. A rendering is the input here, and the renderer
 * writes every code block fenced and escapes a paragraph that would read as one, so a line
 * opening with such a run is a fence whatever its info string says: CommonMark's exception for a
 * backtick in the info string is not applied, since a `code_block.language` holding one (which the
 * markdown parser refuses, so only a live tree carries it) still renders as a fence, and reading
 * its lines as prose would cut a block written inside them.
 */
const CODE_FENCE = /^((?:[ \t]*>)*[ \t]*)((?:[-*+]|\d+[.)])[ \t]+)?(`{3,}|~{3,})/;

/** A line that separates blocks: nothing, or only spaces and quote markers (`>` inside a quote). */
export const BLANK_LINE = /^[ \t>]*$/;

/** An ATX heading's first line, at any of its six depths. */
const HEADING = /^ {0,3}#{1,6}(?:[ \t]|$)/;

/** A block quote's first line. Its content is read as the quote, not as what it holds. */
const QUOTE = /^ {0,3}>/;

/** A list item's first line: a bullet or an ordered marker, then a space. */
const LIST_ITEM = /^ {0,3}(?:[-*+]|\d{1,9}[.)])(?:[ \t]|$)/;

/** A table's delimiter row, the second line of every table: pipes, dashes and alignment colons. */
const TABLE_DELIMITER = /^ {0,3}\|?(?:[ \t]*:?-+:?[ \t]*\|)+(?:[ \t]*:?-+:?[ \t]*)?$/;

/**
 * A line that opens a block which is neither prose nor one of the kinds above: a typed block
 * other than `ask` (a callout), a thematic break, or raw HTML.
 */
const OTHER_OPENING = /^ {0,3}(?::{3,}|-{3,}[ \t]*$|\*{3,}[ \t]*$|_{3,}[ \t]*$|<)/;

/**
 * The index of the closing fence of the decision block opening at `start`, or `undefined` when
 * no later line closes it. The closing fence is the first later line whose text after the
 * opening's prefix is exactly its colons: a typed block nested in another uses a shorter fence
 * than the one holding it, so matching the exact count keeps the two in step. Lines after a
 * list marker's first line sit at the marker's width, so the prefix they repeat is that many
 * spaces.
 */
export function closingFenceAfter(lines: readonly string[], start: number): number | undefined {
  const opening = OPENING_FENCE.exec(lines[start]);
  if (opening === null) return undefined;
  const [, quotePrefix, listMarker = "", colons] = opening;
  const prefix = quotePrefix + " ".repeat(listMarker.length);
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.startsWith(prefix) && line.slice(prefix.length).trimEnd() === colons) return i;
  }
  return undefined;
}

/**
 * The index of the last line of the fenced code block opening at `start`, or `undefined` when
 * the line opens none. Code is text, whatever it spells: a decision block written as an example
 * inside a fence is not a decision block, and the server keeps it as code too. The block closes
 * at the first later line that, after the opening's prefix and up to three spaces, is a run of
 * the same character at least as long as the opening's; one nothing closes runs to the end of
 * the document, as CommonMark reads it. The renderer writes every code block fenced
 * (`packages/envoy/internal/dispatch/pmdoc/render.go`, `codeBlockFence`), so a stored version
 * holds no indented code for this to read.
 */
export function codeFenceEnd(lines: readonly string[], start: number): number | undefined {
  const opening = CODE_FENCE.exec(lines[start]);
  if (opening === null) return undefined;
  const [, quotePrefix, listMarker = "", fence] = opening;
  const prefix = quotePrefix + " ".repeat(listMarker.length);
  const closing = new RegExp(`^ {0,3}${fence[0] === "`" ? "`" : "~"}{${fence.length},}[ \\t]*$`);
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.startsWith(prefix) && closing.test(line.slice(prefix.length))) return i;
  }
  return lines.length - 1;
}

/** What a reader sees a block as. `other` is a quote, a callout, a thematic break or raw HTML. */
export type BlockKind = "heading" | "paragraph" | "list" | "table" | "code" | "ask" | "other";

/** One unit a reader sees, its `source` the block's own lines exactly as they stand. */
export interface DocumentBlock {
  source: string;
  kind: BlockKind;
}

/** The block `lines` spell, read from its first line (and, for a table, its second). */
function describeBlock(lines: readonly string[]): DocumentBlock {
  const source = lines.join("\n");
  if (HEADING.test(lines[0])) return { source, kind: "heading" };
  if (QUOTE.test(lines[0])) return { source, kind: "other" };
  if (lines.length > 1 && lines[0].includes("|") && TABLE_DELIMITER.test(lines[1])) {
    return { source, kind: "table" };
  }
  if (LIST_ITEM.test(lines[0])) return { source, kind: "list" };
  if (OTHER_OPENING.test(lines[0])) return { source, kind: "other" };
  return { source, kind: "paragraph" };
}

/**
 * The index after the last line of the plain block starting at `start`: the next blank line, or
 * the end of the document. A fence met along the way is jumped whole, so a list item holding a
 * code block or a decision block keeps the blank lines inside that fence and stays one block.
 */
function plainBlockEnd(lines: readonly string[], start: number): number {
  let i = start;
  while (i < lines.length && !BLANK_LINE.test(lines[i])) {
    const fenced = codeFenceEnd(lines, i) ?? closingFenceAfter(lines, i);
    i = (fenced ?? i) + 1;
  }
  return i;
}

/**
 * `markdown` cut into the units a reader sees: a heading, a paragraph, a whole list, a whole
 * table, a fenced code block or a `:::ask` block. Blocks are separated by blank lines, except
 * that a fenced code block (`codeFenceEnd`) and a decision block (`closingFenceAfter`) are kept
 * whole whatever they hold - an `:::ask` example written inside a code fence is code, and a
 * nested colon run inside a decision block does not end it.
 *
 * A block's `source` is its own lines, exactly as they stand, and the blank lines between two
 * blocks belong to neither: joining every `source` with the one blank line the renderer writes
 * between blocks (`\n\n`) gives the document back.
 */
export function splitBlocks(markdown: string): DocumentBlock[] {
  const endsWithNewline = markdown.endsWith("\n");
  const lines = (endsWithNewline ? markdown.slice(0, -1) : markdown).split("\n");
  const blocks: DocumentBlock[] = [];
  let i = 0;
  while (i < lines.length) {
    if (BLANK_LINE.test(lines[i])) {
      i += 1;
      continue;
    }
    const codeEnd = codeFenceEnd(lines, i);
    if (codeEnd !== undefined) {
      blocks.push({ source: lines.slice(i, codeEnd + 1).join("\n"), kind: "code" });
      i = codeEnd + 1;
      continue;
    }
    const askEnd = closingFenceAfter(lines, i);
    if (askEnd !== undefined) {
      blocks.push({ source: lines.slice(i, askEnd + 1).join("\n"), kind: "ask" });
      i = askEnd + 1;
      continue;
    }
    const end = plainBlockEnd(lines, i);
    blocks.push(describeBlock(lines.slice(i, end)));
    i = end;
  }
  return blocks;
}
