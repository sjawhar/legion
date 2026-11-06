import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { serverBlockSchema as blockSchema } from "./block-schema-fixture";
import { withMarksEditor } from "./marks-editor";
import {
  type Clipboard,
  cellPositions,
  pasteClipboard,
  placeCaret,
  selectCells,
  withDomGlobals,
} from "./paste";
import { type PasteCase, type PasteTarget, pasteCases } from "./paste-matrix-cases";

/**
 * The clipboard matrix that used to run in a browser, against a server and Postgres, once per row
 * (INFRA-1415): the same clipboards, the same carets and cell selections, and the same expected
 * markdown, verbatim in paste-matrix-cases.ts. Only the rows whose claim is the server's - Go's
 * reading back of what the editor stored, and the reload - stayed in the browser.
 *
 * The editor is the one Dispatch runs, with the document service's own block schema, so a pasted
 * `:::ask` or a seeded `:::callout` is the typed block it is there.
 */

/** The names a paste of this matrix reaches: `HTMLElement` for the DOM round trip of a table or a
 *  heading, `CSSStyleRule` for prosemirror-view's walk of `document.styleSheets` when it reads a
 *  spreadsheet's clipboard HTML. */
const DOM_GLOBALS = ["HTMLElement", "CSSStyleRule"] as const;

/** happy-dom's window, which `withDomWindow` has put on `globalThis` for the editor. */
function domWindow(): Record<string, unknown> {
  return (globalThis as unknown as { window: Record<string, unknown> }).window;
}

/**
 * Runs `paste` with `CSSStyleSheet.rules` readable. A browser's `rules` is the legacy alias of
 * `cssRules`, which happy-dom alone implements, and prosemirror-view's `readHTML` reads `rules`
 * on every sheet of the document it parses the clipboard in - the spreadsheet cases' clipboards
 * carry a `<style>` block, so without the alias the paste throws `style.rules is undefined`.
 */
function withStyleSheetRules<T>(run: () => T): T {
  const sheets = domWindow().CSSStyleSheet as { prototype: object } | undefined;
  if (sheets === undefined || "rules" in sheets.prototype) return run();
  Object.defineProperty(sheets.prototype, "rules", {
    configurable: true,
    get(this: { cssRules: unknown }) {
      return this.cssRules;
    },
  });
  try {
    return run();
  } finally {
    Reflect.deleteProperty(sheets.prototype, "rules");
  }
}

/** The first occurrence of `quote` in the document's text, as a document range: what the e2e's
 *  `selectEditorText` finds in the editor's DOM. */
function findText(view: EditorView, quote: string): { from: number; to: number } {
  let found: { from: number; to: number } | undefined;
  view.state.doc.descendants((node, pos) => {
    if (found !== undefined) return false;
    const index = node.isText ? (node.text ?? "").indexOf(quote) : -1;
    if (index >= 0) found = { from: pos + index, to: pos + index + quote.length };
    return found === undefined;
  });
  if (found === undefined) throw new Error(`the document holds no ${JSON.stringify(quote)}`);
  return found;
}

/** The position of the cell whose whole text is `text`, as the e2e picks a cell by what it reads. */
function cellAt(view: EditorView, text: string): number {
  const cell = cellPositions(view).find((candidate) => candidate.text === text);
  if (cell === undefined)
    throw new Error(`the table holds no cell reading ${JSON.stringify(text)}`);
  return cell.pos;
}

/** Puts the selection where the row put it before it pasted. */
function selectTarget(view: EditorView, target: PasteTarget): void {
  if ("caretAfter" in target) {
    placeCaret(view, findText(view, target.caretAfter).to);
    return;
  }
  if ("selectText" in target) {
    const { from, to } = findText(view, target.selectText);
    view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
    return;
  }
  if ("cells" in target) {
    selectCells(view, cellAt(view, target.cells[0]), cellAt(view, target.cells[1]));
  }
}

/**
 * Copies the cells of that rectangle the way the e2e row's `copy(page)` does: a real `copy` event
 * on the editor's own DOM, which prosemirror-view's copy handler answers by writing its clipboard
 * serialization - the table HTML carrying `data-pm-slice` - into the event's `DataTransfer`.
 */
function copyOfCells(view: EditorView, cells: readonly [string, string]): Clipboard {
  selectCells(view, cellAt(view, cells[0]), cellAt(view, cells[1]));
  const window = domWindow();
  const Transfer = window.DataTransfer as new () => { getData(type: string): string };
  const Clip = window.ClipboardEvent as new (type: string, init: Record<string, unknown>) => Event;
  const clipboardData = new Transfer();
  view.dom.dispatchEvent(new Clip("copy", { bubbles: true, cancelable: true, clipboardData }));
  return { html: clipboardData.getData("text/html"), text: clipboardData.getData("text/plain") };
}

/** The cells the editor draws, row by row. */
function drawnRows(view: EditorView): string[][] {
  const rows: string[][] = [];
  view.state.doc.descendants((node) => {
    if (!node.type.name.endsWith("_row")) return true;
    rows.push(node.children.map((cell) => cell.textContent));
    return false;
  });
  return rows;
}

/** The hard breaks in the document: `hardbreak` is Milkdown's name for ProseMirror's
 *  `hard_break`, and the e2e counted them as `[data-type="hardbreak"]` elements. */
function hardBreakCount(view: EditorView): number {
  let breaks = 0;
  view.state.doc.descendants((node) => {
    if (node.type.name === "hardbreak") breaks += 1;
    return true;
  });
  return breaks;
}

/** A table row's cells, split on the `|`s that end a cell: one after an odd run of backslashes is
 *  the cell's own text (GFM), which is a case of this matrix in its own right. */
function rowCells(row: string): string[] {
  const cells: string[] = [];
  let cell = "";
  let backslashes = 0;
  for (const character of row) {
    if (character === "|" && backslashes % 2 === 0) {
      cells.push(cell);
      cell = "";
    } else {
      cell += character;
    }
    backslashes = character === "\\" ? backslashes + 1 : 0;
  }
  cells.push(cell);
  return cells;
}

const TABLE_ROW = /^(\s*)\|(.*\|)\s*$/;

/** A cell of a table's alignment row, as `---`, `:---`, `---:` or `:---:`. */
const DELIMITER_CELL = /^\s*:?-+:?\s*$/;

/** The escaped space the editor writes, against the engine's `&#32;`. */
const EDITOR_ESCAPED_SPACE = /&#x20;/gi;

/** The bullet the editor writes, against the engine's `-`. */
const EDITOR_BULLET = /^(\s*)\* /;

/** A directive's own line, whose attributes the two writers spell differently. */
const DIRECTIVE_LINE = /^:::.*$/;

/** A directive attribute the writer left without a value, as `title` in `:::callout{#k1 title}`. */
const BARE_ATTRIBUTE = /(?<=[\s{])([A-Za-z_][\w-]*)(?=[\s}])/g;

/** Whether the row the `index`th line holds is a table's delimiter row: the row under the
 *  table's first row, every cell of it a delimiter. */
function isDelimiterRow(
  lines: readonly string[],
  index: number,
  cells: readonly string[]
): boolean {
  return (
    index > 0 &&
    TABLE_ROW.test(lines[index - 1] ?? "") &&
    !TABLE_ROW.test(lines[index - 2] ?? "") &&
    cells.every((cell) => DELIMITER_CELL.test(cell))
  );
}

/** A delimiter cell without the padding its writer gave it: `:----------` and `:---` are the
 *  same column, aligned left. The colons stay, because they are the column's alignment. */
function withoutDelimiterPadding(cell: string): string {
  const written = cell.trim();
  return `${written.startsWith(":") ? ":" : ""}---${written.endsWith(":") ? ":" : ""}`;
}

/**
 * Two writers wrote the markdown this file compares: the editor's own serializer wrote what
 * `getMarkdown` answers, and the engine wrote what the e2e row read back out of the server, from
 * the same document. They spell the same document differently, and only in these ways, each of
 * which this collapses so that nothing else can hide behind it:
 *
 * - a table row's cells are padded to a common width, the delimiter row's cells included, so the
 *   same left-aligned column is `| :---------- |` to the editor and `| :--- |` to the engine;
 * - an empty cell is `<br />` to the editor and nothing at all to the engine;
 * - a space that has to be escaped is `&#x20;` to the editor and `&#32;` to the engine;
 * - a bullet is `*` to the editor and `-` to the engine;
 * - the paragraph after a table inside a list item is written after a blank line by the engine
 *   and on the next line by the editor (LEGION-290, the e2e row that stayed in the browser);
 * - an attribute with an empty value is `title` to the editor and `title=""` to the engine.
 *
 * A delimiter cell keeps its colons, so a column's alignment is compared rather than collapsed:
 * the padding is the writers' style, the colons are the document. `runCase` compares each
 * column's alignment against the case's own delimiter row as well (`columnAlignments` against
 * `storedAlignments`), which names the column a markdown diff only shows.
 *
 * It never touches a cell's own text, the number of cells in a row, the number of rows, or any
 * text outside a table: a wrong cell, a lost cell or a lost row still reports.
 */
function withoutWriterStyle(markdown: string): string {
  const lines = markdown.split("\n");
  const read: string[] = [];
  for (const [index, line] of lines.entries()) {
    const row = TABLE_ROW.exec(line);
    if (row === null) {
      if (line.trim() === "") {
        const afterTable = TABLE_ROW.test(lines[index - 1] ?? "") && lines[index + 1] !== undefined;
        if (afterTable) continue;
      }
      read.push(
        line
          .replaceAll(EDITOR_ESCAPED_SPACE, "&#32;")
          .replace(EDITOR_BULLET, "$1- ")
          .replace(DIRECTIVE_LINE, (directive) => directive.replaceAll(BARE_ATTRIBUTE, '$1=""'))
      );
      continue;
    }
    const [, indent, body] = row;
    const cells = rowCells(body ?? "").slice(0, -1);
    const written = isDelimiterRow(lines, index, cells)
      ? cells.map(withoutDelimiterPadding)
      : cells.map((cell) => (cell.trim() === "<br />" ? "" : cell.trim()));
    read.push(`${indent}| ${written.join(" | ")} |`);
  }
  return read.join("\n");
}

/** A column's alignment, as a cell of the editor's table holds it and as the document service
 *  reads it off a delimiter row. `null` is a column aligned nowhere. */
type ColumnAlignment = "center" | "left" | "right" | null;

/** The alignment a delimiter cell names: `---` none, `:---` left, `---:` right and `:---:`
 *  centre. The document service reads the same four, and a column goldmark reports as
 *  `AlignNone` has no alignment at all (`parseTableRow`,
 *  packages/envoy/internal/dispatch/pmdoc/parse.go), which its writer writes back as `---`
 *  (`tableAlignment`, render_table.go). */
function alignmentOf(cell: string): ColumnAlignment {
  const written = cell.trim();
  const left = written.startsWith(":");
  const right = written.endsWith(":");
  if (left && right) return "center";
  if (left) return "left";
  if (right) return "right";
  return null;
}

/** Each table's column alignments in `markdown`, table by table, as its delimiter row spells
 *  them. */
function storedAlignments(markdown: string): ColumnAlignment[][] {
  const lines = markdown.split("\n");
  const tables: ColumnAlignment[][] = [];
  for (const [index, line] of lines.entries()) {
    const row = TABLE_ROW.exec(line);
    if (row === null) continue;
    const cells = rowCells(row[2] ?? "").slice(0, -1);
    if (isDelimiterRow(lines, index, cells)) tables.push(cells.map(alignmentOf));
  }
  return tables;
}

/** Each table's column alignments as the editor holds them: the cells of its header row, which
 *  is the row both writers read a column's alignment from - the editor's serializer from the
 *  table's first row (@milkdown/preset-gfm's `table.toMarkdown`), the document service's writer
 *  from the first row of its grid (`table`, render_table.go). */
function columnAlignments(view: EditorView): ColumnAlignment[][] {
  const tables: ColumnAlignment[][] = [];
  view.state.doc.descendants((node) => {
    if (node.type.name !== "table_header_row") return true;
    tables.push(node.children.map((cell) => (cell.attrs.alignment as ColumnAlignment) ?? null));
    return false;
  });
  return tables;
}

/**
 * Gives every seeded cell the alignment its spec's delimiter row spells, which is the alignment
 * the document the e2e row pasted into carried.
 *
 * Production seeds the room from the stored markdown with the document service's parser, which
 * leaves a `---` column with no alignment (parse.go) and writes it into the Y.Doc as `"none"`
 * (`liveNulls`, update.go), which `treeAttrs` (read.go) reads back as no alignment. The editor's
 * own parser reads `---` as no alignment too, but `withMarksEditor` seeds through
 * `prosemirrorToYXmlFragment`, which drops a null attribute - and reading the fragment back fills
 * the gap with the schema's default, `left` (@milkdown/preset-gfm's `cellAttributes`). So without
 * this every seeded column would arrive left-aligned, where the stored markdown the case compares
 * against aligns it nowhere, and a paste that reset a column to left would change nothing. This
 * seeds `null` where production's editor holds `"none"`: the two serialize alike, and they differ
 * only where a cell goes through the DOM, whose `setDOMAttr` writes a `null` as `left`.
 */
function seedColumnAlignments(view: EditorView, spec: string): void {
  const tables = storedAlignments(spec);
  const transaction = view.state.tr;
  let table = -1;
  view.state.doc.descendants((node, pos) => {
    if (node.type.name === "table") {
      table += 1;
      return true;
    }
    if (!node.type.name.endsWith("_row")) return true;
    const alignments = tables[table];
    if (alignments === undefined)
      throw new Error(`the spec spells no delimiter row for table ${table + 1}`);
    let at = pos + 1;
    for (const [column, cell] of node.children.entries()) {
      const alignment = alignments[column] ?? null;
      if (cell.attrs.alignment !== alignment)
        transaction.setNodeAttribute(at, "alignment", alignment);
      at += cell.nodeSize;
    }
    return false;
  });
  if (transaction.steps.length > 0) view.dispatch(transaction);
}

test("the writer normaliser collapses the padding of a row and of a delimiter row", () => {
  expect(
    withoutWriterStyle(
      "| alpha one   | beta two               |\n| :---------- | :--------------------- |\n| gamma three | deltaFirst Second four |\n"
    )
  ).toBe(
    withoutWriterStyle(
      "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaFirst Second four |\n"
    )
  );
  expect(withoutWriterStyle("| a | <br /> |\n")).toBe(withoutWriterStyle("| a |  |\n"));
});

test("the writer normaliser reports a column the two writers align differently", () => {
  expect(withoutWriterStyle("| a | b |\n| :---------- | --- |\n| c | d |\n")).not.toBe(
    withoutWriterStyle("| a | b |\n| --- | --- |\n| c | d |\n")
  );
  expect(withoutWriterStyle("| a |\n| ---: |\n| c |\n")).not.toBe(
    withoutWriterStyle("| a |\n| :---: |\n| c |\n")
  );
});

test("a delimiter row reads as the alignment the document service gives each column", () => {
  expect(
    storedAlignments("| a | b | c | d |\n| --- | :--- | ---: | :---: |\n| 1 | 2 | 3 | 4 |\n")
  ).toEqual([[null, "left", "right", "center"]]);
  expect(storedAlignments("| a |\n| --- |\n| --- |\n")).toEqual([[null]]);
  expect(storedAlignments("Intro end.\n")).toEqual([]);
});

test("the writer normaliser collapses the escapes, bullets and blank lines of the other writer", () => {
  expect(withoutWriterStyle("* top\n  * Intro\n")).toBe(withoutWriterStyle("- top\n  - Intro\n"));
  expect(withoutWriterStyle("&#x20;end\n")).toBe(withoutWriterStyle("&#32;end\n"));
  expect(withoutWriterStyle("| a |\n| --- |\n| b |\n\nTail\n")).toBe(
    withoutWriterStyle("| a |\n| --- |\n| b |\nTail\n")
  );
  expect(withoutWriterStyle(':::callout{#k1 kind="note" title}\nx\n:::\n')).toBe(
    withoutWriterStyle(':::callout{#k1 kind="note" title=""}\nx\n:::\n')
  );
});

test("the writer normaliser still reports a wrong cell", () => {
  expect(withoutWriterStyle("| a  | b |\n| --- | --- |\n| c | d |\n")).not.toBe(
    withoutWriterStyle("| a | b |\n| --- | --- |\n| c | e |\n")
  );
  expect(withoutWriterStyle("| a b | c |\n")).not.toBe(withoutWriterStyle("| ab | c |\n"));
  expect(withoutWriterStyle("| x <br /> y |\n")).not.toBe(withoutWriterStyle("| x y |\n"));
});

test("the writer normaliser still reports a lost cell, a lost row and lost text", () => {
  expect(withoutWriterStyle("| a | b |  |\n")).not.toBe(withoutWriterStyle("| a | b |\n"));
  expect(withoutWriterStyle("| a |\n| --- |\n| b |\n| c |\n")).not.toBe(
    withoutWriterStyle("| a |\n| --- |\n| b |\n")
  );
  expect(withoutWriterStyle("Intro  end.\n")).toBe("Intro  end.\n");
  expect(withoutWriterStyle("Intro end.\n")).not.toBe(withoutWriterStyle("Introend.\n"));
});

test("the writer normaliser leaves a body cell reading --- and a cell's escaped pipe alone", () => {
  expect(withoutWriterStyle("| h |\n| --- |\n| --- |\n")).toBe("| h |\n| --- |\n| --- |\n");
  expect(withoutWriterStyle("| a \\| b  | c |\n")).toBe("| a \\| b | c |\n");
  expect(withoutWriterStyle("| a \\| b | c |\n")).not.toBe(withoutWriterStyle("| a | b | c |\n"));
});

/** The matrix's own first case, whose two-column spec aligns neither column: the seed's proof
 *  runs on a case rather than on a table of its own. */
const [firstCase] = pasteCases;

test("a seeded column carries the alignment its spec spells, not the schema's default", async () => {
  if (firstCase === undefined) throw new Error("the matrix holds no cases");
  const { spec } = firstCase;
  expect(storedAlignments(spec)).toEqual([[null, null]]);
  await withMarksEditor(
    spec,
    ({ handle, view }) => {
      // What the Y.Doc round trip of the seed leaves behind: the schema's default, where the
      // spec - and the document service's parser - align the column nowhere.
      expect(columnAlignments(view)).toEqual([["left", "left"]]);
      expect(withoutWriterStyle(handle.getMarkdown())).not.toBe(withoutWriterStyle(spec));

      seedColumnAlignments(view, spec);

      expect(columnAlignments(view)).toEqual([[null, null]]);
      expect(withoutWriterStyle(handle.getMarkdown())).toBe(withoutWriterStyle(spec));
    },
    { blockSchema }
  );
});

function runCase({ clipboard, copyCells, hardBreaks, rows, spec, stored, target }: PasteCase) {
  return withMarksEditor(
    spec,
    ({ handle, view }) => {
      seedColumnAlignments(view, spec);
      withDomGlobals(DOM_GLOBALS, () => {
        const pasted = copyCells === undefined ? clipboard : copyOfCells(view, copyCells);
        selectTarget(view, target);
        if (pasted !== undefined) withStyleSheetRules(() => pasteClipboard(view, pasted));
      });

      // What the stored markdown's delimiter row spells, column by column: a paste that lost or
      // reset a column's alignment names the column here, before the markdown comparison reads
      // the same loss as a changed delimiter row.
      expect(columnAlignments(view)).toEqual(storedAlignments(stored));
      expect(withoutWriterStyle(handle.getMarkdown())).toBe(withoutWriterStyle(stored));
      if (hardBreaks !== undefined) expect(hardBreakCount(view)).toBe(hardBreaks);
      if (rows !== undefined) expect(drawnRows(view)).toEqual(rows as string[][]);
    },
    { blockSchema }
  );
}

for (const pasteCase of pasteCases) {
  test(pasteCase.name, () => runCase(pasteCase), pasteCase.timeoutMs);
}

test("the matrix moved out of the browser runs whole", () => {
  expect(pasteCases.length).toBe(114);
});
