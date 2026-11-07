/**
 * What two of the corpus documents read as, and what a leaf reading of one is: the types, the
 * row constructors and the expected readings three files hold the engine to -
 * `leafMarksInOneDocumentReading` by `e2e/leaf-marks.e2e.ts` and
 * `web/src/features/refs/fixture-table-readings.test.ts`, `listsSpacedByIndentedCodeReading` by
 * `e2e/indented-code-whitespace-lines.e2e.ts` and that same unit test - so a browser row and a
 * headless row compare against one literal rather than two copies of it.
 *
 * Declarations and data alone live here, never the readers that produce the rows. An e2e row's
 * reader runs inside an `evaluateAll(…)`, whose callback Playwright serializes and evaluates in
 * the browser, where it can close over nothing this module exports; the unit test's reads a
 * happy-dom element in this process. The readers stay written out, one per file.
 */

/** One inline leaf a paragraph draws. */
export interface Leaf {
  /** What names the leaf: an `html` span's `data-value`, a picture's `src`, a footnote
   *  reference's `data-label`. */
  readonly identity: string;
  /** The element the leaf is drawn as: `span` for raw HTML, `img`, `sup`. */
  readonly kind: string;
  /** The mark elements between the leaf and its paragraph, innermost first: always none, since a
   *  mark never reaches an inline leaf. */
  readonly marks: readonly string[];
}

/** One paragraph of a leaf reading. */
export interface LeafRow {
  readonly leaves: readonly Leaf[];
  /** The paragraph's text exactly as it is drawn; whitespace is part of the reading, so no
   *  matcher that collapses it (`toHaveText`) may stand in for this. */
  readonly text: string;
}

export function html(value: string): Leaf {
  return { identity: value, kind: "span", marks: [] };
}

export function picture(src: string): Leaf {
  return { identity: src, kind: "img", marks: [] };
}

export function footnote(label: string): Leaf {
  return { identity: label, kind: "sup", marks: [] };
}

export function row(text: string, ...leaves: Leaf[]): LeafRow {
  return { leaves, text };
}

/** What the surface draws for `leaf-marks-in-one-document`'s paragraphs, in order: the corpus
 *  file's own opening sentence, then inline HTML inside strong, a footnote reference inside
 *  emphasis and its definition, a picture inside a link inside strong, an inline HTML
 *  declaration, an autolink alone in a link label and an autolink between a label's words. No
 *  leaf carries a mark, the declaration `<!a>` is an `html` node like any other raw HTML, and the
 *  two labels holding an autolink draw their words beside its address. */
export const leafMarksInOneDocumentReading: LeafRow[] = [
  row(
    "An inline leaf carries no mark, a declaration is raw HTML, and an autolink in a link label leaves the label's own link on the text around it: one block for each."
  ),
  row("a<b>c", html("<b>")),
  row("d", footnote("d")),
  row("note"),
  row("", picture("i.png")),
  row("p <!a> q", html("<!a>")),
  row("https://u.test"),
  row("a https://u.test b"),
];

/** What the engine draws for `lists-spaced-by-indented-code-whitespace-lines`: the whole text of
 *  each code block, and the `data-spread` of each item and of each list, in document order. No
 *  list is spread; of the thirteen items only those parted from their code by a blank line are;
 *  the quoted item's code keeps its inner blank line and drops its trailing ones, and the rest
 *  keep the whitespace-only lines that hold four columns or more past their indent. The e2e row
 *  compares this as one `JSON.stringify` line, so the keys keep the order they are written in. */
export const listsSpacedByIndentedCodeReading = {
  code: [
    "code\n\nnote",
    "    x\n  ",
    "   x\n   ",
    "code",
    "  x\n  ",
    "code\nmore\n  \n\n\n    ",
    "code",
  ],
  items: [
    "false",
    "true",
    "false",
    "true",
    "false",
    "true",
    "false",
    "false",
    "false",
    "false",
    "false",
    "true",
    "false",
  ],
  lists: ["false", "false", "false", "false", "false", "false", "false", "false"],
};
