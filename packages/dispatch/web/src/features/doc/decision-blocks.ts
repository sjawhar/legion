/**
 * A decision block's opening fence: an optional prefix of spaces and `>` quote markers, a list
 * marker when the fence sits right after it on the item's first line, a run of three or more
 * colons, then `ask{`. The prefix and the colon count are what its closing fence repeats.
 */
const OPENING_FENCE = /^((?:[ \t]*>)*[ \t]*)((?:[-*+]|\d+[.)])[ \t]+)?(:{3,})ask\{/;

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
 * `markdown` with every decision block cut out, on lines, so two versions of a document that
 * differ only in their decision blocks - an answer recorded in a block's attributes, a block
 * added or removed - cut to the same text, and a diff of the two shows only what changed
 * outside them.
 *
 * A block runs from its opening fence through its closing fence (`closingFenceAfter`), wherever
 * it stands: at the top level, inside a blockquote, a list item or another typed block. The one
 * blank line after the closing fence goes with it, or the one before the opening fence when the
 * block ends the document, so the text around it closes up as if the block had never been
 * written. A block no later line closes is left as it is. The Go server cuts the parsed tree
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
