import type { DocumentJSON, DocumentMarkJSON } from "@legion/proof-editor";

export interface Highlight {
  by: string;
  id: string;
  quote: string;
}

/** Proof's comment mark. The editor draws it as a span carrying `data-id`, which is the span
 *  `focusMark` scrolls the reader to. */
const COMMENT_MARK = "proofComment";

/** One search of a document for a quote: the mark to draw over it, and how many times the
 *  document's text has held it so far. */
interface Search {
  found: number;
  readonly mark: DocumentMarkJSON;
  readonly quote: string;
}

/** The consecutive text nodes of one inline row, with `search.quote` marked where it stands,
 *  counting every occurrence of it on the way. The row's nodes are returned as they came in when
 *  the quote is not in them, or when an earlier row already carried the one that is marked: a
 *  quote the document holds twice marks nothing at all (`embedHighlight`).
 *
 *  A row rather than a node at a time, because the quote a comment anchors to is the words a
 *  reader selected, and a link or an emphasis inside them splits those words across text nodes
 *  the reader never saw a seam in. */
function markRow(row: readonly DocumentJSON[], search: Search): readonly DocumentJSON[] {
  const text = row.map((node) => node.text ?? "").join("");
  let at = text.indexOf(search.quote);
  if (at === -1) return row;
  const from = search.found === 0 ? at : -1;
  while (at !== -1) {
    search.found += 1;
    at = text.indexOf(search.quote, at + 1);
  }
  if (from === -1) return row;

  const to = from + search.quote.length;
  const marked: DocumentJSON[] = [];
  let start = 0;
  for (const node of row) {
    const own = node.text ?? "";
    const quoteFrom = Math.max(from - start, 0);
    const quoteTo = Math.min(to - start, own.length);
    start += own.length;
    if (quoteTo <= quoteFrom) {
      marked.push(node);
      continue;
    }
    if (quoteFrom > 0) marked.push({ ...node, text: own.slice(0, quoteFrom) });
    marked.push({
      ...node,
      marks: [...(node.marks ?? []), search.mark],
      text: own.slice(quoteFrom, quoteTo),
    });
    if (quoteTo < own.length) marked.push({ ...node, text: own.slice(quoteTo) });
  }
  return marked;
}

/** `node` with `search.quote` marked wherever it stands in one row of its text, and every
 *  occurrence of it below counted. The node is returned as it came in where nothing changed, so
 *  a document holding no occurrence allocates nothing. */
function markNode(node: DocumentJSON, search: Search): DocumentJSON {
  const children = node.content;
  if (children === undefined) return node;
  const content: DocumentJSON[] = [];
  let changed = false;
  let row: DocumentJSON[] = [];
  const closeRow = () => {
    if (row.length === 0) return;
    const marked = markRow(row, search);
    if (marked !== row) changed = true;
    content.push(...marked);
    row = [];
  };
  for (const child of children) {
    if (child.type === "text") {
      row.push(child);
      continue;
    }
    closeRow();
    const next = markNode(child, search);
    if (next !== child) changed = true;
    content.push(next);
  }
  closeRow();
  return changed ? { ...node, content } : node;
}

/**
 * `tree` - the reading the server made of a stored text - with a comment mark of `highlight.id`
 * over the words `highlight.quote` names, so the editor draws the quote highlighted and
 * `focusMark` can scroll to it. `undefined` where the text does not hold those words exactly
 * once, which the reader is told as text that changed: a highlight drawn over the wrong words
 * would be worse than none.
 *
 * The mark goes into the tree rather than into the version's markdown: the past-version page
 * draws the tree the server sent and parses nothing, so markdown the editor would have to parse
 * is no longer part of drawing it.
 */
export function embedHighlight(tree: DocumentJSON, highlight: Highlight): DocumentJSON | undefined {
  const search: Search = {
    found: 0,
    mark: { attrs: { by: highlight.by, id: highlight.id }, type: COMMENT_MARK },
    quote: highlight.quote,
  };
  const marked = markNode(tree, search);
  return search.found === 1 ? marked : undefined;
}
