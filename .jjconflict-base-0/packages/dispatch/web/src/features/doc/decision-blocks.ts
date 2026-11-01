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
const BLANK_LINE = /^[ \t>]*$/;

/**
 * The index of the closing fence of the decision block opening at `start`, or `undefined` when
 * no later line closes it. The closing fence is the first later line whose text after the
 * opening's prefix is exactly its colons: a typed block nested in another uses a shorter fence
 * than the one holding it, so matching the exact count keeps the two in step. Lines after a
 * list marker's first line sit at the marker's width, so the prefix they repeat is that many
 * spaces.
 */
function closingFenceAfter(lines: readonly string[], start: number): number | undefined {
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
function codeFenceEnd(lines: readonly string[], start: number): number | undefined {
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
 * both give the same answer to whether two versions differ outside their blocks.
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
