import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText } from "./api";
import { documentEditor, openIssue, openWithCaret, paste } from "./editor";
import { engineTables, goReadBack } from "./read-back";
import { resetDatabase } from "./seed";

// What is left of the clipboard matrix: the rows no headless test can make. Every other row
// asserted the markdown the server stored for the document the paste built, and runs in a
// fraction of the time in packages/proof-editor/tests/paste-matrix.test.ts, which rebuilds that
// document headless and compares its own serializer's spelling of it - each column's alignment
// included - against that same stored markdown (INFRA-1415).

test.beforeEach(async () => {
  await resetDatabase();
});

const table = "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta four |\n";

/** Selects the cells from the one holding `from` to the one holding `to` by dragging across them,
 * as a person does. */
async function selectCells(page: Page, from: string, to: string) {
  const editor = documentEditor(page);
  const start = await editor.getByText(from).boundingBox();
  const end = await editor.getByText(to).boundingBox();
  if (!start || !end) throw new Error("the cells have no layout");
  await page.mouse.move(start.x + 5, start.y + start.height / 2);
  await page.mouse.down();
  await page.mouse.move(end.x + end.width / 2, end.y + end.height / 2, { steps: 10 });
  await page.mouse.up();
}

/** Opens the two-row table as alice and selects the cells from `from` to `to`. */
async function openWithCellsSelected(browser: Browser, from: string, to: string) {
  const { alice, issue, page } = await openIssue(browser, "Cell selection paste", table);
  await expect(documentEditor(page)).toContainText(to);
  await selectCells(page, from, to);
  return { alice, issue, page };
}

/** The cells the editor draws, row by row, as the page shows them. */
function drawnRows(page: Page) {
  return documentEditor(page)
    .locator("tr")
    .evaluateAll((rows) =>
      rows.map((row) => Array.from(row.querySelectorAll("th, td"), (cell) => cell.textContent))
    );
}

// A copy of cells pasted onto a selection of every cell of the table reaches the server and is
// there on the next visit: the clipboard event, the grid paste, the document socket, the stored
// artifact and the reload, the one path that is not the editor's serializer alone. The same
// clipboard's markdown is asserted headless; this row is the round trip.
const copiedCells = "<table><tr><th>one</th><th>two</th></tr><tr><td>1</td><td>2</td></tr></table>";
test("cells pasted onto a selection of cells are stored and read back after a reload", async ({
  browser,
}) => {
  const { alice, issue, page } = await openWithCellsSelected(browser, "alpha one", "delta four");
  try {
    await expect(page.locator(".selectedCell")).toHaveCount(4);

    await paste(page, { html: copiedCells, text: "one\ttwo\n1\t2" });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("| one | two |\n| :--- | :--- |\n| 1 | 2 |\n");

    await page.reload();
    await expect(documentEditor(page)).toContainText("one");
    await expect
      .poll(() => drawnRows(page))
      .toEqual([
        ["one", "two"],
        ["1", "2"],
      ]);
  } finally {
    await alice.close();
  }
});

/** A copied table whose first row has no cells (an empty `<tr>`), and one whose last row has
 *  none: the clipboards the rows below paste. */
const emptyFirstRow = {
  html: "<table><tr></tr><tr><td>a</td><td>b</td></tr></table>",
  text: "a\tb",
};
const emptyLastRow = {
  html: "<table><tr><td>a</td><td>b</td></tr><tr></tr></table>",
  text: "a\tb",
};

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
  // Both readers give back the stored bytes exactly, except in a tight list item: a paragraph after
  // a table there is written after a blank line, so it does not read back as one more table row,
  // and the blank line reads back as a spread item, the canonicalisation markdown has for it, in Go
  // as in the engine (LEGION-290). There only the table's own rows are compared.
  for (const [context, spec, stored, indent, readsBackExactly] of [
    ["a paragraph", "Intro end.\n", `Intro\n\n${rows}\n&#32;end.\n`, "", true],
    [
      "a callout",
      ':::callout{#k1 kind="note"}\nIntro end.\n:::\n',
      `:::callout{#k1 kind="note" title=""}\nIntro\n\n${rows}\n&#32;end.\n:::\n`,
      "",
      true,
    ],
    ["a heading", "# Intro end\n", `# Intro\n\n${rows}\n# &#32;end\n`, "", true],
    [
      "a nested list",
      "- top\n  - Intro end\n",
      `- top\n  - Intro\n${tableRows(header, body, "    ")}\n    &#32;end\n`,
      "    ",
      false,
    ],
  ] as const) {
    test(`a table whose ${row} row is empty, pasted into ${context}, keeps its cells`, async ({
      browser,
    }) => {
      const { alice, issue, page } = await openWithCaret(
        browser,
        "Table paste",
        spec,
        "Intro",
        "end"
      );
      try {
        await paste(page, clipboard);

        await expect
          .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
          .toBe(stored);
        const [table] = engineTables(stored);
        const goRead = await goReadBack(stored);
        expect(table.slice(0, 2)).toEqual([header, body]);
        expect(goRead).toContain(tableRows(header, body, indent));
        expect(table.length === 2 && goRead === stored).toBe(readsBackExactly);
      } finally {
        await alice.close();
      }
    });
  }
}

// A `|` after an even run of backslashes ends a table cell, and one after an odd run is the cell's
// text, to Go as to the browser editor: `| a \\| b |` is the cells `a \` and `b`, and `| a \| b |`
// the one cell `a | b`, which Go pads to the header's width with an empty cell, as the editor
// shows it. Go stores a document written through the API as it reads it, so a Go that read the
// even run as the cell's text would store, and the editor show, the one cell `a \| b` in its place.
test("a pipe after two backslashes ends a table cell in Go as in the editor", async ({
  browser,
}) => {
  const written = "| one | two |\n| --- | --- |\n| a \\\\| b |\n| a \\| b |\n";
  const cells = [
    ["one", "two"],
    ["a \\", "b"],
    ["a | b", ""],
  ];
  const { alice, issue, page } = await openIssue(browser, "Escaped table pipes", written);
  try {
    expect(engineTables(written)).toEqual([[...cells.slice(0, 2), ["a | b"]]]);
    await expect.poll(() => drawnRows(page)).toEqual(cells);
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect(engineTables(stored)).toEqual([cells]);
    expect(engineTables(await goReadBack(stored))).toEqual([cells]);
  } finally {
    await alice.close();
  }
});
