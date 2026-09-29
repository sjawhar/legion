import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText, getIssue } from "./api";
import { copy, documentEditor, openIssue, openWithCaret, paste, selectEditorText } from "./editor";
import { engineTables, goReadBack } from "./read-back";
import { resetDatabase } from "./seed";

test.beforeEach(async () => {
  await resetDatabase();
});

const loneAsk = ':::ask{#d2 urgency="med" multiple="false"}\nWhich one?\n\n- A\n- B\n:::\n';

// Plain text pasted into a table cell lands in that one cell, with its blocks and line breaks
// flattened to inline text joined by spaces, since a GFM cell holds one line. It once overwrote the
// caret's cell and spread the other lines into new cells of the same row, past the header's column
// count, where a GFM reader drops them.
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
  test(`${JSON.stringify(pasted)} pasted as plain text into ${cell} stays in that cell`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCaret(browser, "Cell paste", table, quote, "end");
    try {
      await paste(page, { html: "", text: pasted });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
      expect((await getIssue(issue.key)).open_asks).toEqual([]);
    } finally {
      await alice.close();
    }
  });
}

// HTML pasted into a table cell, the usual clipboard, lands in that one cell the same way, with its
// marks and links kept. It once spread its paragraphs into new cells of the row, overwriting the
// caret's cell, or replaced the header row.
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
  test(`HTML pasted into ${cell} stays in that cell with its marks`, async ({ browser }) => {
    const { alice, issue, page } = await openWithCaret(
      browser,
      "HTML cell paste",
      table,
      quote,
      "end"
    );
    try {
      await paste(page, { html, text: "Bold Link Code\n\nSecond" });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
    } finally {
      await alice.close();
    }
  });
}

// Text flattened into a cell takes the marks at the caret, or of the text it replaces, when it is one
// line, as a one-line paste does anywhere else, and more than one line pasted into bold text isn't
// bold, as anywhere else. The one line once kept only its own marks: after bold "delta" the pasted
// "x" was stored outside the bold, and pasted over bold "delta" it lost the bold.
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
  test(`${name} pasted ${target} in a cell stores ${JSON.stringify(stored)}`, async ({
    browser,
  }) => {
    const { alice, issue, page } =
      target === "over selected bold text"
        ? await openIssue(browser, "Marks paste", boldTable)
        : await openWithCaret(browser, "Marks paste", boldTable, "delta", "end");
    try {
      if (target === "over selected bold text") await selectEditorText(page, "delta");

      await paste(page, clipboard);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(`| alpha one | beta two |\n| --- | --- |\n| gamma three | ${stored} |\n`);
    } finally {
      await alice.close();
    }
  });
}

// Cells copied from a table and pasted with the caret in a cell are a grid paste, as on a selection
// of cells: prosemirror-tables writes them over the cells from the caret's on, growing the table as
// needed. Each pasted cell is retyped for the row it lands in: pasted onto the body, the copied
// header cells became header cells in a body row, and prosemirror-tables threw and stored nothing.
const copiedCells = "<table><tr><th>one</th><th>two</th></tr><tr><td>1</td><td>2</td></tr></table>";
for (const [cell, quote, stored] of [
  ["a header cell", "alpha", "| one | two |\n| :--- | :--- |\n| 1 | 2 |\n"],
  [
    "a body cell",
    "delta",
    "| alpha one | beta two |  |\n| --- | --- | :--- |\n| gamma three | one | two |\n|  | 1 | 2 |\n",
  ],
] as const) {
  test(`cells copied from a table and pasted into ${cell} fill the table from there`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCaret(browser, "Cells paste", table, quote, "end");
    try {
      await paste(page, { html: copiedCells, text: "one\ttwo\n1\t2" });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
    } finally {
      await alice.close();
    }
  });
}

// A spreadsheet's copy is a table too, although Google Sheets and Excel put a style block beside it:
// pasted with the caret in the header cell, it fills the table from there. Its CSS once counted as
// text outside the table, and the copy was joined into the one cell.
for (const [source, html] of [
  [
    "Google Sheets",
    '<google-sheets-html-origin><style type="text/css"><!--td {border: 1px solid #cccccc;}--></style><table><tbody><tr><td>S1</td><td>S2</td></tr><tr><td>T1</td><td>T2</td></tr></tbody></table></google-sheets-html-origin>',
  ],
  [
    "Excel",
    "<html><head><meta name=ProgId content=Excel.Sheet><style><!--td {mso-number-format:General;}--></style></head><body><table><!--StartFragment--><tr><td>S1</td><td>S2</td></tr><tr><td>T1</td><td>T2</td></tr><!--EndFragment--></table></body></html>",
  ],
] as const) {
  test(`a copy from ${source} pasted into a header cell fills the table from there`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCaret(
      browser,
      "Sheet paste",
      table,
      "alpha",
      "end"
    );
    try {
      await paste(page, { html, text: "S1\tS2\nT1\tT2" });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe("| S1 | S2 |\n| :--- | :--- |\n| T1 | T2 |\n");
    } finally {
      await alice.close();
    }
  });
}

// Only a clipboard whose content all sits in table cells is a grid paste. HTML holding a paragraph
// beside a table joins the caret's cell as text, like any other HTML, so the paragraph isn't lost.
test("HTML holding a paragraph and a table, pasted into a body cell, stays in that cell", async ({
  browser,
}) => {
  const { alice, issue, page } = await openWithCaret(browser, "Mixed paste", table, "delta", "end");
  try {
    await paste(page, {
      html: "<p>Intro</p><table><tr><td>c1</td><td>c2</td></tr></table>",
      text: "Intro\nc1\tc2",
    });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("| alpha one | beta two |\n| --- | --- |\n| gamma three | deltaIntro c1 c2 four |\n");
  } finally {
    await alice.close();
  }
});

/** How a person selects cells: dragging across them, clicking one and shift-clicking the other, or
 * dragging out of one cell and back into it, which selects that cell alone (prosemirror-tables). */
type CellGesture = "drag" | "shift-click" | "drag-back";

/** Selects the cells from the one holding `from` to the one holding `to` with `how`; for
 * "drag-back", `from` names the one cell. */
async function selectCells(page: Page, from: string, to: string, how: CellGesture) {
  const editor = documentEditor(page);
  if (how === "drag-back") {
    const cell = editor.locator("td, th").filter({ hasText: from });
    const start = await cell.boundingBox();
    const neighbour = await cell
      .locator("xpath=following-sibling::*[1] | preceding-sibling::*[1]")
      .first()
      .boundingBox();
    if (!start || !neighbour) throw new Error("the cells have no layout");
    await page.mouse.move(start.x + 5, start.y + start.height / 2);
    await page.mouse.down();
    await page.mouse.move(neighbour.x + neighbour.width / 2, neighbour.y + neighbour.height / 2, {
      steps: 5,
    });
    await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2, { steps: 5 });
    await page.mouse.up();
  } else if (how === "drag") {
    const start = await editor.getByText(from).boundingBox();
    const end = await editor.getByText(to).boundingBox();
    if (!start || !end) throw new Error("the cells have no layout");
    await page.mouse.move(start.x + 5, start.y + start.height / 2);
    await page.mouse.down();
    await page.mouse.move(end.x + end.width / 2, end.y + end.height / 2, { steps: 10 });
    await page.mouse.up();
  } else {
    await editor.getByText(from).click();
    await editor.getByText(to).click({ modifiers: ["Shift"] });
  }
}

/** Opens `spec` (the two-row table by default) as alice and selects the cells from `from` to `to`. */
async function openWithCellsSelected(
  browser: Browser,
  from: string,
  to: string,
  how: CellGesture,
  spec: string = table
) {
  const { alice, issue, page } = await openIssue(browser, "Cell selection paste", spec);
  await expect(documentEditor(page)).toContainText(to);
  await selectCells(page, from, to, how);
  return { alice, issue, page };
}

/** Where a paste lands in the two-row table: the caret after a cell's text, or the cells from one
 * to another, a single cell when both name it. */
type Target = string | readonly [string, string];

function targetName(target: Target): string {
  if (typeof target === "string") return `at the caret after "${target}"`;
  return target[0] === target[1]
    ? `onto the cell ${target[0]}`
    : `onto ${target[0]} to ${target[1]}`;
}

/** Opens the two-row table as alice with the caret after `target`, or with its cells selected. */
async function openAt(browser: Browser, title: string, target: Target) {
  if (typeof target === "string") return openWithCaret(browser, title, table, target, "end");
  const single = target[0] === target[1];
  const opened = await openWithCellsSelected(
    browser,
    target[0],
    target[1],
    single ? "drag-back" : "shift-click"
  );
  if (single) await expect(opened.page.locator(".selectedCell")).toHaveCount(1);
  else await expect(opened.page.locator(".selectedCell")).not.toHaveCount(0);
  return opened;
}

// Tab-separated text pasted onto a selection of cells fills them as a grid, one value per cell, the
// way a spreadsheet's copy pastes. It once threw inside prosemirror-tables once the selection
// reached the header row, whose cells must be header cells, and the browser then pasted the text
// into the last cell by itself.
for (const how of ["drag", "shift-click"] as const) {
  test(`tab-separated text pasted onto four cells selected by ${how} fills them as a grid`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCellsSelected(
      browser,
      "alpha one",
      "delta four",
      how
    );
    try {
      await expect(page.locator(".selectedCell")).toHaveCount(4);

      await paste(page, { html: "", text: "H1\tH2\nB1\tB2" });

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe("| H1 | H2 |\n| :--- | :--- |\n| B1 | B2 |\n");
    } finally {
      await alice.close();
    }
  });
}

// A line break pasted into a table as cells, from HTML or a code block, is a space in its cell, as in
// text pasted at a caret, since a GFM cell holds one line and a hard break stored in a table ends the
// row. These pastes once kept the break in the cell, and the stored table read back broken, or
// they threw and stored nothing.
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
  test(`${name} pasted ${targetName(target)} keeps each cell on one line`, async ({ browser }) => {
    const { alice, issue, page } = await openAt(browser, "Line break paste", target);
    try {
      await paste(page, clipboard);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
      await expect(
        documentEditor(page).locator(
          "table br:not(.ProseMirror-trailingBreak), table [data-type='hardbreak']"
        )
      ).toHaveCount(0);
    } finally {
      await alice.close();
    }
  });
}

// A copied cell holding more than one block, as a Docs cell of two paragraphs or a list does, keeps
// all of it, joined into its one line the way the caret path joins pasted text: text beside a block,
// and a table nested in the cell, as email HTML nests them, included. Fitting the parsed cell into a
// Milkdown cell, which holds one paragraph, once kept only the first block, text before a block
// once ran into that block's first line and lost the rest, and a nested table's rows once became
// rows of the grid, with their text removed from the cell.
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
    test(`cells, one holding ${blocks}, pasted ${targetName(target)} keep all of it`, async ({
      browser,
    }) => {
      const { alice, issue, page } = await openAt(browser, "Block cell paste", target);
      try {
        await paste(page, clipboard);

        await expect
          .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
          .toBe(stored);
      } finally {
        await alice.close();
      }
    });
  }
}

// HTML pasted onto selected cells that isn't a copy of cells fills them one line each, a line being
// a paragraph, a list item, or text beside a block, and like a copy of cells it repeats from its
// first line across a selection wider than it. A list once filled every cell with its first item,
// and text or an image beside a block was once dropped.
const threeColumns = "| c1 | c2 | c3 |\n| --- | --- | --- |\n| d1 | d2 | d3 |\n";
const image = "![](https://example.com/i.png)";
for (const [shape, html, cells] of [
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
    test(`${shape} pasted onto d1 to ${to} fills them one line each`, async ({ browser }) => {
      const { alice, issue, page } = await openWithCellsSelected(
        browser,
        "d1",
        to,
        "shift-click",
        threeColumns
      );
      try {
        await expect(page.locator(".selectedCell")).toHaveCount(to === "d2" ? 2 : 3);

        await paste(page, { html, text: cells.join("\n") });

        await expect
          .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
          .toBe(`| c1 | c2 | c3 |\n| --- | --- | --- |\n${row}\n`);
      } finally {
        await alice.close();
      }
    });
  }
}

// A copied table's row with no cells (an empty <tr>) carries nothing to paste, so the grid paste
// leaves it out. It once reached prosemirror-tables' insert as a row of no cells, which threw, and the
// paste stored nothing.
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
  test(`cells whose ${row} row is empty, pasted ${targetName(target)}, paste the other row`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openAt(browser, "Empty row paste", target);
    try {
      await paste(page, clipboard);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
    } finally {
      await alice.close();
    }
  });
}

// A copied table whose rows hold no cells pastes as nothing, so onto selected cells it empties them,
// as pasting nothing over selected text deletes it. Its cell-less row once reached prosemirror-tables
// as a grid of no columns, which threw, and the paste stored nothing.
test("a table with no cells, pasted onto the whole table, empties its cells", async ({
  browser,
}) => {
  const { alice, issue, page } = await openAt(browser, "No cells paste", [
    "alpha one",
    "delta four",
  ]);
  try {
    await paste(page, { html: "<table><tr></tr></table>", text: "" });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("|  |  |\n| :--- | :--- |\n|  |  |\n");
  } finally {
    await alice.close();
  }
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
  test(`cells copied in the editor and pasted at ${target} arrive as the same cells`, async ({
    browser,
  }) => {
    const { alice, issue, page } = await openWithCellsSelected(
      browser,
      "gamma three",
      "delta four",
      "drag",
      threeRows
    );
    try {
      await expect(page.locator(".selectedCell")).toHaveCount(2);
      const copied = await copy(page);
      if (target === "a caret in another row's cell") {
        await selectEditorText(page, "eps");
        await page.keyboard.press("ArrowRight");
      } else {
        await selectCells(page, "eps five", "zeta six", "shift-click");
        await expect(page.locator(".selectedCell")).toHaveCount(2);
      }

      await paste(page, copied);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(
          "| alpha one | beta two |\n| --- | --- |\n| gamma three | delta four |\n| gamma three | delta four |\n"
        );
    } finally {
      await alice.close();
    }
  });
}

// A paste onto a selection of whole cells stays prosemirror-tables' own grid paste: each pasted line
// fills one selected cell. The flattening is for a caret in a cell's text, and applied to a cell
// selection it would put all the pasted text into every selected cell.
test("plain text pasted onto a selection of cells fills them line by line", async ({ browser }) => {
  const { alice, issue, page } = await openWithCellsSelected(
    browser,
    "gamma three",
    "delta four",
    "drag"
  );
  try {
    await expect(page.locator(".selectedCell")).toHaveCount(2);

    await paste(page, { html: "", text: "First\n\nSecond\n" });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("| alpha one | beta two |\n| --- | --- |\n| First | Second |\n");
  } finally {
    await alice.close();
  }
});

// A soft line break in pasted plain text is pasted as a hard break, since a pasted "alpha\nbeta"
// keeps its line break (packages/proof-editor/src/lib.ts, where markdown import alone turns soft
// breaks into spaces). The editor shows the break, and the stored backslash and newline read back as
// that break. It was once pasted as a soft break, which the editor draws as a space while its stored
// copy reads back as a line break. A markdown hard break, an HTML <br>, and a soft break in markdown
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
  test(`${shape} is stored as the break the editor shows`, async ({ browser }) => {
    const { alice, issue, page } = await openWithCaret(
      browser,
      "Line break paste",
      spec,
      quote,
      "end"
    );
    try {
      await paste(page, clipboard);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
      await expect(documentEditor(page).locator('br[data-type="hardbreak"]')).toHaveCount(1);
      await expect(documentEditor(page).locator('span[data-type="hardbreak"]')).toHaveCount(0);
    } finally {
      await alice.close();
    }
  });
}

test("a soft line break in markdown written through the API is a space in the editor", async ({
  browser,
}) => {
  const { alice, issue, page } = await openIssue(browser, "Soft break import", "Intro\nend.\n");
  try {
    await expect(documentEditor(page)).toContainText("Intro end.");
    await expect(documentEditor(page).locator('[data-type="hardbreak"]')).toHaveCount(0);
    expect((await getArtifactText(issue.primary_artifact_id)).markdown).toBe("Intro end.\n");
  } finally {
    await alice.close();
  }
});

// A pasted table whose first or last row is empty keeps its table and its cells. With an empty first
// row the paste hung the page: prosemirror-tables' fixTables filled the cell-less header row with
// body cells, which Milkdown's header row can't hold, so ProseMirror fitted them as a new row and
// fixTables ran again, forever. With an empty last row, preset-gfm's paste rule counted the table's
// columns in that row, found none, and replaced the table with an empty paragraph.
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
