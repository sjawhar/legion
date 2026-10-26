import type { Node as ProseMirrorNode } from "prosemirror-model";

/** A selection named by its text: the quote, and which match of it, from zero, it is. */
export interface QuoteAnchor {
  readonly occurrence: number;
  readonly quote: string;
}

/** The ATX marker a quote may open with to mean "the heading whose text follows" (`FindQuote`). */
const HEADING_MARKER = /^#{1,6} +/;

/**
 * The selection `[from, to)` of `doc` as the server finds a comment's anchor quote
 * (`FindQuote` in packages/envoy/internal/dispatch/pmdoc/find.go), so a comment posted with it is
 * anchored where the reader selected, on a document the browser cannot mark itself - a closed
 * issue's. The server reads the document as one text: its text nodes in order, U+0000 read as
 * U+FFFD, and one space wherever text does not run on - between textblocks, around an inline atom
 * such as an image or a hard break. The quote is the selection's text in that reading, without a
 * separator at either end, so it matches exactly and the server's looser matchers never run; the
 * occurrence counts the matches that start before it, overlapping ones too, as the server's search
 * steps one unit at a time. A quote opening with a heading marker would be read as a heading's
 * title, so the marker is left out of it. Undefined when the selection holds no text.
 */
export function quoteAnchor(
  doc: ProseMirrorNode,
  from: number,
  to: number
): QuoteAnchor | undefined {
  let text = "";
  // Each UTF-16 unit's document position; a separator space has none.
  const positions: (number | undefined)[] = [];
  let lastEnd: number | undefined;
  doc.descendants((node, pos) => {
    if (!node.isText) {
      return true;
    }
    if (lastEnd !== undefined && lastEnd !== pos) {
      text += " ";
      positions.push(undefined);
    }
    const value = (node.text ?? "").replaceAll("\u0000", "\uFFFD");
    text += value;
    for (let offset = 0; offset < value.length; offset += 1) {
      positions.push(pos + offset);
    }
    lastEnd = pos + value.length;
    return false;
  });

  let start = positions.findIndex((position) => position !== undefined && position >= from);
  let end = positions.length;
  while (end > 0) {
    const last = positions[end - 1];
    if (last !== undefined && last < to) {
      break;
    }
    end -= 1;
  }
  if (start < 0 || end <= start) {
    return undefined;
  }
  for (
    let marker = HEADING_MARKER.exec(text.slice(start, end));
    marker !== null && marker[0].length < end - start;
    marker = HEADING_MARKER.exec(text.slice(start, end))
  ) {
    start += marker[0].length;
  }
  const quote = text.slice(start, end);
  let occurrence = 0;
  for (
    let index = text.indexOf(quote);
    index >= 0 && index < start;
    index = text.indexOf(quote, index + 1)
  ) {
    occurrence += 1;
  }
  return { occurrence, quote };
}
