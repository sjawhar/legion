import type { Clipboard } from "./paste";

/**
 * The clipboard matrix of `packages/dispatch/e2e/editor-paste.e2e.ts`, moved here: every row of
 * it whose claim is the document the editor builds out of a clipboard runs headless, in a
 * fraction of the browser's time. The data tables below are that file's own, verbatim, each with
 * the comment that explains it; only the rows whose claim needs the server - Go's reading back
 * of the stored markdown, and the clipboard-to-server-to-reload path - stayed in the browser
 * (INFRA-1415), and the editor-side half of each of those is here too.
 *
 * Each case's `stored` is the markdown the e2e row asserted the *server* had stored, kept
 * verbatim so a reviewer can diff it against the deleted literal. Here the editor's own
 * serializer writes that same document, and the two writers spell it differently, so
 * paste-matrix.test.ts compares through a normaliser. That normaliser collapses a row's padding,
 * the editor's `<br />` for an empty cell, its `&#x20;` against the engine's `&#32;`, its `*`
 * bullet against the engine's `-`, a directive attribute left bare against the engine's
 * `title=""`, and the blank line the engine writes after a table in a list item. It keeps each
 * column's alignment, which the test compares column by column against the delimiter row of
 * this `stored` string.
 */

/** Where the paste lands, as the e2e row placed it. */
export type PasteTarget =
  /** The caret collapsed after the first occurrence of this text (`openWithCaret(..., "end")`). */
  | { readonly caretAfter: string }
  /** That text selected, as `selectEditorText` selects it. */
  | { readonly selectText: string }
  /** Every cell of the rectangle between the cells reading these two texts; one cell when both
   *  name it, which the e2e row selects by dragging out of the cell and back into it. */
  | { readonly cells: readonly [string, string] }
  /** Nothing selected and nothing pasted: the case asserts what the seeded markdown alone
   *  stores and draws. */
  | { readonly noPaste: true };

export interface PasteCase {
  /** The e2e row's own name, so a reviewer finds the row this case replaces. */
  readonly name: string;
  /** The markdown the editor is seeded with: the e2e row's issue spec. */
  readonly spec: string;
  /** Where the caret or the cell selection sits when the clipboard arrives. */
  readonly target: PasteTarget;
  /** The clipboard pasted, or, with `copyCells`, the cells copied out of the editor itself. */
  readonly clipboard?: Clipboard;
  /** Copy the cells of this rectangle out of the editor first and paste that copy: the e2e row's
   *  `copy(page)`, ProseMirror's own clipboard HTML with its `data-pm-slice`. */
  readonly copyCells?: readonly [string, string];
  /** The markdown the e2e row asserted the server stored, verbatim; the editor's own writer
   *  spells that same document differently, so paste-matrix.test.ts compares through its
   *  normaliser. */
  readonly stored: string;
  /** How many hard breaks the document must then hold: the editor-side reading of the row's own
   *  count of `[data-type="hardbreak"]` elements. */
  readonly hardBreaks?: number;
  /** The cells the editor must draw, row by row: the editor-side reading of a row that read them
   *  out of the DOM. */
  readonly rows?: readonly (readonly string[])[];
  /** A per-test timeout in milliseconds, for a case whose regression is slow rather than wrong.
   *  Bun reports a case that overruns it only once the case returns; EMPTY_ROW_TIMEOUT_MS says
   *  what that does and does not buy. */
  readonly timeoutMs?: number;
}

/**
 * The limit the cases a missing prosemirror-tables patch would hang in carry. A full editor boot
 * and one paste is milliseconds here, so five seconds is a hang - but Bun interrupts nothing. A
 * case that overruns its limit is reported, by name, only after it returns: a three-second
 * synchronous spin under a 500 ms limit runs all three seconds and is then failed. The hang
 * these cases guard against is prosemirror-tables' `fixTables` looping inside
 * `appendTransaction`, which is synchronous and never returns, so nothing is reported at all and
 * the whole `proof-editor` test step hangs until the job's own timeout kills it.
 *
 * So the limit buys a named failure for a case that merely grew slow, or one that waits on a
 * promise that never settles. It buys nothing against that loop, and five seconds is Bun's own
 * default per-test timeout, so it states the intent rather than tightening anything.
 */
const EMPTY_ROW_TIMEOUT_MS = 5_000;

/** The same wording the e2e file's own `targetName` gives a row. */
function targetName(target: PasteTarget): string {
  if ("caretAfter" in target) return `at the caret after "${target.caretAfter}"`;
  if ("selectText" in target) return `over the selected "${target.selectText}"`;
  if ("noPaste" in target) return "nowhere";
  const [from, to] = target.cells;
  return from === to ? `onto the cell ${from}` : `onto ${from} to ${to}`;
}

/** The target a row writes as either the text its caret lands after or the two cells of its
 *  rectangle, which is how the rows of the loops below name where they paste. */
function targetAt(target: string | readonly [string, string]): PasteTarget {
  return typeof target === "string" ? { caretAfter: target } : { cells: target };
}

const cases: PasteCase[] = [];

const loneAsk = ':::ask{#d2 urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n';

// Plain text pasted into a table cell lands in that one cell, with its blocks and line breaks
// flattened to inline text joined by spaces, since a GFM cell holds one line. Overwriting the
// caret's cell and spreading the other lines into new cells of the row would put them past the
// header's column count, where a GFM reader drops them.
const table = "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta four |\n";
for (const [cell, quote, pasted, stored] of [
  [
    "a body cell",
    "delta",
    "First\n\nSecond\n",
    "| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaFirst Second four |\n",
  ],
  [
    "a header cell",
    "alpha",
    "First\n\nSecond\n",
    "| alphaFirst Second one | beta two |\n| --- | --- |\n| gamma three | delta four |\n",
  ],
  [
    "a body cell",
    "delta",
    loneAsk,
    "| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaWhich one? A B four |\n",
  ],
  [
    "a header cell",
    "alpha",
    loneAsk,
    "| alphaWhich one? A B one | beta two |\n| --- | --- |\n| gamma three | delta four |\n",
  ],
  [
    "a body cell",
    "delta",
    "First\nSecond",
    "| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaFirst Second four |\n",
  ],
  [
    "a body cell",
    "delta",
    "More words.",
    "| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaMore words. four |\n",
  ],
] as const) {
  // The e2e row also read the issue's open asks back as empty. That claim is the server's
  // indexer's, not the editor's, and the stored markdown here is what it rests on: the pasted
  // ask is the cell's text, not an ask.
  cases.push({
    clipboard: { html: "", text: pasted },
    name: `${JSON.stringify(pasted)} pasted as plain text into ${cell} stays in that cell`,
    spec: table,
    stored,
    target: { caretAfter: quote },
  });
}

// HTML pasted into a table cell, the usual clipboard, lands in that one cell the same way, with its
// marks and links kept; it never spreads into new cells of the row, replaces the caret cell's text,
// or replaces the header row.
const html =
  '<p><strong>Bold</strong> <a href="https://example.com">Link</a> <code>Code</code></p><p><em>Second</em></p>';
for (const [cell, quote, stored] of [
  [
    "a body cell",
    "delta",
    "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta**Bold** [Link](https://example.com) `Code` *Second* four |\n",
  ],
  [
    "a header cell",
    "alpha",
    "| alpha**Bold** [Link](https://example.com) `Code` *Second* one | beta two |\n| --- | --- |\n| gamma three | delta four |\n",
  ],
] as const) {
  cases.push({
    clipboard: { html, text: "Bold Link Code\n\nSecond" },
    name: `HTML pasted into ${cell} stays in that cell with its marks`,
    spec: table,
    stored,
    target: { caretAfter: quote },
  });
}

// Text flattened into a cell takes the marks at the caret, or of the text it replaces, when it is one
// line, as a one-line paste does anywhere else, and more than one line pasted into bold text isn't
// bold, as anywhere else. A line that kept only its own marks would store an "x" pasted after bold
// "delta" outside the bold, and lose the bold pasted over bold "delta".
const boldTable = "| alpha one | beta two |\n| --- | --- |\n| gamma three | **delta** four |\n";
for (const [name, clipboard, target, stored] of [
  ["plain text", { html: "", text: "x" }, "after bold text", "**deltax** four"],
  ["plain text", { html: "", text: "x" }, "over selected bold text", "**x** four"],
  ["HTML", { html: "<p>x</p>", text: "x" }, "after bold text", "**deltax** four"],
  ["HTML", { html: "<p>x</p>", text: "x" }, "over selected bold text", "**x** four"],
  [
    "two paragraphs",
    { html: "", text: "First\n\nSecond\n" },
    "after bold text",
    "**delta**First Second four",
  ],
] as const) {
  cases.push({
    clipboard,
    name: `${name} pasted ${target} in a cell stores ${JSON.stringify(stored)}`,
    spec: boldTable,
    stored: `| alpha one | beta two |\n| --- | --- |\n| gamma three | ${stored} |\n`,
    target:
      target === "over selected bold text" ? { selectText: "delta" } : { caretAfter: "delta" },
  });
}

// Cells copied from a table and pasted with the caret in a cell are a grid paste, as on a selection
// of cells: prosemirror-tables writes them over the cells from the caret's on, growing the table as
// needed. Each pasted cell is retyped for the row it lands in: copied header cells pasted onto the
// body as they are would be header cells in a body row, where prosemirror-tables throws and stores
// nothing.
const copiedCells = "<table><tr><th>one</th><th>two</th></tr><tr><td>1</td><td>2</td></tr></table>";
for (const [cell, quote, stored] of [
  ["a header cell", "alpha", "| one | two |\n| :--- | :--- |\n| 1 | 2 |\n"],
  [
    "a body cell",
    "delta",
    "| alpha one | beta two |  |\n| --- | --- | :--- |\n| gamma three | one | two |\n|  | 1 | 2 |\n",
  ],
] as const) {
  cases.push({
    clipboard: { html: copiedCells, text: "one\ttwo\n1\t2" },
    name: `cells copied from a table and pasted into ${cell} fill the table from there`,
    spec: table,
    stored,
    target: { caretAfter: quote },
  });
}

// A spreadsheet's copy is a table too, although Google Sheets and Excel put a style block beside it:
// pasted with the caret in the header cell, it fills the table from there. If its CSS counted as
// text outside the table, the copy would be joined into the one cell.
for (const [source, sheetHtml] of [
  [
    "Google Sheets",
    '<google-sheets-html-origin><style type="text/css"><!--td {border: 1px solid #cccccc;}--></style><table><tbody><tr><td>S1</td><td>S2</td></tr><tr><td>T1</td><td>T2</td></tr></tbody></table></google-sheets-html-origin>',
  ],
  [
    "Excel",
    "<html><head><meta name=ProgId content=Excel.Sheet><style><!--td {mso-number-format:General;}--></style></head><body><table><!--StartFragment--><tr><td>S1</td><td>S2</td></tr><tr><td>T1</td><td>T2</td></tr><!--EndFragment--></table></body></html>",
  ],
] as const) {
  cases.push({
    clipboard: { html: sheetHtml, text: "S1\tS2\nT1\tT2" },
    name: `a copy from ${source} pasted into a header cell fills the table from there`,
    spec: table,
    stored: "| S1 | S2 |\n| :--- | :--- |\n| T1 | T2 |\n",
    target: { caretAfter: "alpha" },
  });
}

// Only a clipboard whose content all sits in table cells is a grid paste. HTML holding a paragraph
// beside a table joins the caret's cell as text, like any other HTML, so the paragraph isn't lost.
cases.push({
  clipboard: {
    html: "<p>Intro</p><table><tr><td>c1</td><td>c2</td></tr></table>",
    text: "Intro\nc1\tc2",
  },
  name: "HTML holding a paragraph and a table, pasted into a body cell, stays in that cell",
  spec: table,
  stored: "| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaIntro c1 c2 four |\n",
  target: { caretAfter: "delta" },
});

// Tab-separated text pasted onto a selection of cells fills them as a grid, one value per cell, the
// way a spreadsheet's copy pastes, header row included, whose cells must be header cells. A grid
// paste that throws inside prosemirror-tables there leaves the browser pasting the text into the
// last cell by itself.
// The gesture is the browser's: both rows select the same four cells, so both read the same here.
for (const how of ["drag", "shift-click"] as const) {
  cases.push({
    clipboard: { html: "", text: "H1\tH2\nB1\tB2" },
    name: `tab-separated text pasted onto four cells selected by ${how} fills them as a grid`,
    spec: table,
    stored: "| H1 | H2 |\n| :--- | :--- |\n| B1 | B2 |\n",
    target: { cells: ["alpha one", "delta four"] },
  });
}

// A line break pasted into a table as cells, from HTML or a code block, is a space in its cell, as in
// text pasted at a caret, since a GFM cell holds one line and a hard break stored in a table ends the
// row. A break kept in the cell would store a table that reads back broken, or the paste would
// throw and store nothing.
const lineBreakHtml = { html: "<p>one<br>two</p>", text: "one\ntwo" };
const lineBreakCells = {
  html: "<table><tr><td>b1<br>b2</td><td>c</td></tr></table>",
  text: "b1\nb2\tc",
};
for (const [name, clipboard, target, stored] of [
  [
    "HTML with a line break",
    lineBreakHtml,
    ["alpha one", "gamma three"],
    "| one two | beta two |\n| :--- | --- |\n| one two | delta four |\n",
  ],
  [
    "HTML with a line break",
    lineBreakHtml,
    ["alpha one", "delta four"],
    "| one two | one two |\n| :--- | :--- |\n| one two | one two |\n",
  ],
  [
    "a code block's HTML",
    { html: "<pre><code>line1\nline2</code></pre>", text: "line1\nline2" },
    ["alpha one", "delta four"],
    "| line1 line2 | line1 line2 |\n| :--- | :--- |\n| line1 line2 | line1 line2 |\n",
  ],
  [
    "cells holding a line break",
    lineBreakCells,
    "delta",
    "| alpha one | beta two |  |\n| --- | --- | :--- |\n| gamma three | b1 b2 | c |\n",
  ],
  [
    "cells holding a line break",
    lineBreakCells,
    "alpha",
    "| b1 b2 | c |\n| :--- | :--- |\n| gamma three | delta four |\n",
  ],
  [
    "cells holding a line break",
    lineBreakCells,
    ["gamma three", "delta four"],
    "| alpha one | beta two |\n| --- | --- |\n| b1 b2 | c |\n",
  ],
  [
    "cells holding a line break",
    lineBreakCells,
    ["alpha one", "delta four"],
    "| b1 b2 | c |\n| :--- | :--- |\n| b1 b2 | c |\n",
  ],
] as const) {
  const where = targetAt(target);
  cases.push({
    clipboard,
    hardBreaks: 0,
    name: `${name} pasted ${targetName(where)} keeps each cell on one line`,
    spec: table,
    stored,
    target: where,
  });
}

// A copied cell holding more than one block, as a Docs cell of two paragraphs or a list does, keeps
// all of it, joined into its one line the way the caret path joins pasted text: text beside a block,
// and a table nested in the cell, as email HTML nests them, included. Fitting the parsed cell into a
// Milkdown cell, which holds one paragraph, must not keep only the first block, run text before a
// block into that block's first line, or turn a nested table's rows into rows of the grid.
const cellBlocks = [
  ["two paragraphs", "<p>x</p><p>y</p>", "x y"],
  ["a list", "<ul><li>a</li><li>b</li></ul>", "a b"],
  ["text then a list", "a<ul><li>i</li><li>j</li></ul>", "a i j"],
  ["text then a paragraph", "a<p>b</p>", "a b"],
  ["a paragraph then text", "<p>x</p>tail", "x tail"],
  ["bold text then a paragraph", "<b>x</b><p>y</p>", "**x** y"],
  ["text around a rule", "a<hr>b", "a b"],
  ["a table", "o1<table><tr><td>i1</td><td>i2</td></tr></table>", "o1 i1 i2"],
] as const;
for (const [blocks, cellHtml, joined] of cellBlocks) {
  const clipboard = {
    html: `<table><tr><td>${cellHtml}</td><td>z</td></tr></table>`,
    text: `${joined.replaceAll("*", "")}\tz`,
  };
  for (const [target, stored] of [
    [
      "delta",
      `| alpha one | beta two |  |\n| --- | --- | :--- |\n| gamma three | ${joined} | z |\n`,
    ],
    ["alpha", `| ${joined} | z |\n| :--- | :--- |\n| gamma three | delta four |\n`],
    [
      ["delta four", "delta four"],
      `| alpha one | beta two |\n| --- | --- |\n| gamma three | ${joined} |\n`,
    ],
    [["gamma three", "delta four"], `| alpha one | beta two |\n| --- | --- |\n| ${joined} | z |\n`],
    [["alpha one", "delta four"], `| ${joined} | z |\n| :--- | :--- |\n| ${joined} | z |\n`],
  ] as const) {
    const where = targetAt(target);
    cases.push({
      clipboard,
      name: `cells, one holding ${blocks}, pasted ${targetName(where)} keep all of it`,
      spec: table,
      stored,
      target: where,
    });
  }
}

// HTML pasted onto selected cells that isn't a copy of cells fills them one line each, a line being
// a paragraph, a list item, or text beside a block, and like a copy of cells it repeats from its
// first line across a selection wider than it. A list's items each fill a cell, and text or an image
// beside a block is never dropped.
const threeColumns = "| c1 | c2 | c3 |\n| --- | --- | --- |\n| d1 | d2 | d3 |\n";
const image = "![](https://example.com/i.png)";
for (const [shape, shapeHtml, cells] of [
  ["a list of 2 items", "<ul><li>a</li><li>b</li></ul>", ["a", "b"]],
  ["a list of 3 items", "<ul><li>a</li><li>b</li><li>c</li></ul>", ["a", "b", "c"]],
  ["an ordered list of 2 items", "<ol><li>a</li><li>b</li></ol>", ["a", "b"]],
  ["an ordered list of 3 items", "<ol><li>a</li><li>b</li><li>c</li></ol>", ["a", "b", "c"]],
  ["two paragraphs", "<p>x</p><p>y</p>", ["x", "y"]],
  ["a line then a block, as a Gmail copy", "Line one<div>line two</div>", ["Line one", "line two"]],
  ["text then a paragraph", "lead<p>b</p>", ["lead", "b"]],
  ["a paragraph then text", "<p>a</p>tail", ["a", "tail"]],
  [
    "a paragraph then a table",
    "<p>lead</p><table><tr><td>one</td><td>two</td></tr></table>",
    ["lead", "one", "two"],
  ],
  [
    "a partial copy of three paragraphs",
    "<span>end of one</span><p>two</p><span>start of three</span>",
    ["end of one", "two", "start of three"],
  ],
  ["a paragraph then an image", '<p>a</p><img src="https://example.com/i.png">', ["a", image]],
] as const) {
  for (const [to, row] of [
    ["d2", `| ${cells[0]} | ${cells[1]} | d3 |`],
    ["d3", `| ${cells[0]} | ${cells[1]} | ${cells[2] ?? cells[0]} |`],
  ] as const) {
    cases.push({
      clipboard: { html: shapeHtml, text: cells.join("\n") },
      name: `${shape} pasted onto d1 to ${to} fills them one line each`,
      spec: threeColumns,
      stored: `| c1 | c2 | c3 |\n| --- | --- | --- |\n${row}\n`,
      target: { cells: ["d1", to] },
    });
  }
}

// A copied table's row with no cells (an empty <tr>) carries nothing to paste, so the grid paste
// leaves it out: prosemirror-tables' insert throws on a row of no cells, and the paste would store
// nothing.
const emptyFirstRow = {
  html: "<table><tr></tr><tr><td>a</td><td>b</td></tr></table>",
  text: "a\tb",
};
const emptyLastRow = {
  html: "<table><tr><td>a</td><td>b</td></tr><tr></tr></table>",
  text: "a\tb",
};
for (const [clipboard, target, stored] of [
  [
    emptyFirstRow,
    "delta",
    "| alpha one | beta two |  |\n| --- | --- | :--- |\n| gamma three | a | b |\n",
  ],
  [emptyFirstRow, "alpha", "| a | b |\n| :--- | :--- |\n| gamma three | delta four |\n"],
  [
    emptyFirstRow,
    ["gamma three", "delta four"],
    "| alpha one | beta two |\n| --- | --- |\n| a | b |\n",
  ],
  [emptyLastRow, ["alpha one", "delta four"], "| a | b |\n| :--- | :--- |\n| a | b |\n"],
] as const) {
  const row = clipboard === emptyFirstRow ? "first" : "last";
  const where = targetAt(target);
  cases.push({
    clipboard,
    name: `cells whose ${row} row is empty, pasted ${targetName(where)}, paste the other row`,
    spec: table,
    stored,
    target: where,
    // Without the prosemirror-tables patch an empty row sends fixTables round forever. That loop
    // is synchronous, so Bun interrupts nothing and names nothing: the step hangs until the job's
    // own timeout. The limit below reports this case only if it grows slow and returns.
    timeoutMs: EMPTY_ROW_TIMEOUT_MS,
  });
}

// A copied table whose rows hold no cells pastes as nothing, so onto selected cells it empties them,
// as pasting nothing over selected text deletes it. Its cell-less row, handed to prosemirror-tables,
// is a grid of no columns, which throws, and the paste would store nothing.
cases.push({
  clipboard: { html: "<table><tr></tr></table>", text: "" },
  name: "a table with no cells, pasted onto the whole table, empties its cells",
  spec: table,
  stored: "|  |  |\n| :--- | :--- |\n|  |  |\n",
  target: { cells: ["alpha one", "delta four"] },
  timeoutMs: EMPTY_ROW_TIMEOUT_MS,
});

// Cells copied inside the editor paste back as the same cells, at a caret in another row's cell
// and onto a selection of that row's cells. The copy is ProseMirror's own clipboard HTML, a table
// marked with data-pm-slice. Parsed at the caret, that gained an empty leading row, and the paste
// blanked the target row, or appended the copied one below it.
const threeRows =
  "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta four |\n| eps five | zeta six |\n";
for (const target of [
  "a caret in another row's cell",
  "a selection of another row's cells",
] as const) {
  cases.push({
    copyCells: ["gamma three", "delta four"],
    name: `cells copied in the editor and pasted at ${target} arrive as the same cells`,
    spec: threeRows,
    stored:
      "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta four |\n| gamma three | delta four |\n",
    target:
      target === "a caret in another row's cell"
        ? { caretAfter: "eps" }
        : { cells: ["eps five", "zeta six"] },
  });
}

// A paste onto a selection of whole cells stays prosemirror-tables' own grid paste: each pasted line
// fills one selected cell. The flattening is for a caret in a cell's text, and applied to a cell
// selection it would put all the pasted text into every selected cell.
cases.push({
  clipboard: { html: "", text: "First\n\nSecond\n" },
  name: "plain text pasted onto a selection of cells fills them line by line",
  spec: table,
  stored: "| alpha one | beta two |\n| --- | --- |\n| First | Second |\n",
  target: { cells: ["gamma three", "delta four"] },
});

// A soft line break in pasted plain text is pasted as a hard break, since a pasted "alpha\nbeta"
// keeps its line break (packages/proof-editor/src/lib.ts, where markdown import alone turns soft
// breaks into spaces). The editor shows the break, and the stored backslash and newline read back as
// that break. Pasted as a soft break, it would show as a space in the editor while its stored copy
// reads back as a line break. A markdown hard break, an HTML <br>, and a soft break in markdown
// written through the API are the controls.
for (const [shape, spec, quote, clipboard, stored] of [
  [
    "a soft line break into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "IntroFirst\\\nSecond end.\n",
  ],
  [
    "a soft line break into a list item",
    "- Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "- IntroFirst\\\n  Second end.\n",
  ],
  [
    "a soft line break into a quote",
    "> Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "> IntroFirst\\\n> Second end.\n",
  ],
  [
    "a soft line break into a callout",
    ':::callout{#k1 kind="note"}\nIntro end.\n:::\n',
    "Intro",
    { html: "", text: "First\nSecond" },
    ':::callout{#k1 kind="note" title=""}\nIntroFirst\\\nSecond end.\n:::\n',
  ],
  [
    "a soft line break into an ask's question",
    ':::ask{#q1 urgency="med" multiple="false"}\nWhich here?\n\n- X\n- Y\n:::\n',
    "Which here?",
    { html: "", text: "First\nSecond" },
    ':::ask{#q1 urgency="med" multiple="false" state="open"}\nWhich here?First\\\nSecond\n\n- X\n- Y\n:::\n',
  ],
  [
    "a markdown hard break into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "", text: "First\\\nSecond" },
    "IntroFirst\\\nSecond end.\n",
  ],
  [
    "an HTML <br> into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "<p>one<br>two</p>", text: "one\ntwo" },
    "Introone\\\ntwo end.\n",
  ],
] as const) {
  cases.push({
    clipboard,
    hardBreaks: 1,
    name: `${shape} is stored as the break the editor shows`,
    spec,
    stored,
    target: { caretAfter: quote },
  });
}

cases.push({
  hardBreaks: 0,
  name: "a soft line break in markdown written through the API is a space in the editor",
  spec: "Intro\nend.\n",
  stored: "Intro end.\n",
  target: { noPaste: true },
});

// A pasted table whose first or last row is empty keeps its table and its cells. With an empty first
// row, prosemirror-tables' fixTables would fill the cell-less header row with body cells, which
// Milkdown's header row can't hold, so ProseMirror would fit them as a new row and fixTables would
// run again, forever, hanging the page. With an empty last row, preset-gfm's paste rule would count
// the table's columns in that row, find none, and replace the table with an empty paragraph.
const tableRows = (header: readonly string[], body: readonly string[], indent = "") =>
  [header, header.map(() => ":---"), body]
    .map((cells) => `${indent}| ${cells.join(" | ")} |\n`)
    .join("");
for (const [row, clipboard, header, body] of [
  ["first", emptyFirstRow, ["", ""], ["a", "b"]],
  ["last", emptyLastRow, ["a", "b"], ["", ""]],
] as const) {
  const rows = tableRows(header, body);
  // The e2e row keeps the half only Go answers for - that Go reads the stored markdown back as
  // the same table - and this is its editor-side half: what the editor stored.
  for (const [context, spec, stored] of [
    ["a paragraph", "Intro end.\n", `Intro\n\n${rows}\n&#32;end.\n`],
    [
      "a callout",
      ':::callout{#k1 kind="note"}\nIntro end.\n:::\n',
      `:::callout{#k1 kind="note" title=""}\nIntro\n\n${rows}\n&#32;end.\n:::\n`,
    ],
    ["a heading", "# Intro end\n", `# Intro\n\n${rows}\n# &#32;end\n`],
    [
      "a nested list",
      "- top\n  - Intro end\n",
      `- top\n  - Intro\n${tableRows(header, body, "    ")}\n    &#32;end\n`,
    ],
  ] as const) {
    cases.push({
      clipboard,
      name: `a table whose ${row} row is empty, pasted into ${context}, keeps its cells`,
      spec,
      stored,
      target: { caretAfter: "Intro" },
      // The hang this row exists for: without the prosemirror-tables patch fixTables runs forever
      // on a cell-less header row, inside appendTransaction and synchronously, so the case never
      // returns. Bun cannot interrupt it, so `bun test` reports nothing and the whole
      // `proof-editor` step hangs until the job's own timeout; the limit below reports only a
      // case that grew slow and returned.
      timeoutMs: EMPTY_ROW_TIMEOUT_MS,
    });
  }
}

// A `|` after an even run of backslashes ends a table cell, and one after an odd run is the cell's
// text, to Go as to the browser editor: `| a \\| b |` is the cells `a \` and `b`, and `| a \| b |`
// the one cell `a | b`, which Go pads to the header's width with an empty cell, as the editor
// shows it. Go stores a document written through the API as it reads it, so a Go that read the
// even run as the cell's text would store, and the editor show, the one cell `a \| b` in its place.
// The e2e row keeps Go's half; this is the editor's: the cells it draws, and the markdown it
// writes for them. That row asserted its cells rather than the stored bytes, so `stored` here is
// the table those same cells make, the only case whose `stored` the e2e file did not spell out.
cases.push({
  name: "a pipe after two backslashes ends a table cell in the editor",
  rows: [
    ["one", "two"],
    ["a \\", "b"],
    ["a | b", ""],
  ],
  spec: "| one | two |\n| --- | --- |\n| a \\\\| b |\n| a \\| b |\n",
  stored: "| one | two |\n| --- | --- |\n| a \\\\ | b |\n| a \\| b |  |\n",
  target: { noPaste: true },
});

export const pasteCases: readonly PasteCase[] = cases;
