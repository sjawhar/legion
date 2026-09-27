import { type Browser, expect, type Page, test } from "@playwright/test";

import { getArtifactText, getIssue } from "./api";
import { copy, documentEditor, openIssue, openWithCaret, paste, selectEditorText } from "./editor";
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
    "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaFirst Second four |\n",
  ],
  [
    "a header cell",
    "alpha",
    "First\n\nSecond\n",
    "| alphaFirst Second one | beta two |\n| :--- | :--- |\n| gamma three | delta four |\n",
  ],
  [
    "a body cell",
    "delta",
    loneAsk,
    "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaWhich one? A B four |\n",
  ],
  [
    "a header cell",
    "alpha",
    loneAsk,
    "| alphaWhich one? A B one | beta two |\n| :--- | :--- |\n| gamma three | delta four |\n",
  ],
  [
    "a body cell",
    "delta",
    "First\nSecond",
    "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaFirst Second four |\n",
  ],
  [
    "a body cell",
    "delta",
    "More words.",
    "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaMore words. four |\n",
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
    "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | delta**Bold** [Link](https://example.com) `Code` *Second* four |\n",
  ],
  [
    "a header cell",
    "alpha",
    "| alpha**Bold** [Link](https://example.com) `Code` *Second* one | beta two |\n| :--- | :--- |\n| gamma three | delta four |\n",
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
        .toBe(`| alpha one | beta two |\n| :--- | :--- |\n| gamma three | ${stored} |\n`);
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
    "| alpha one | beta two |  |\n| :--- | :--- | :--- |\n| gamma three | one | two |\n|  | 1 | 2 |\n",
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
      .toBe("| alpha one | beta two |\n| :--- | :--- |\n| gamma three | deltaIntro c1 c2 four |\n");
  } finally {
    await alice.close();
  }
});

/** Selects the cells from the one holding `from` to the one holding `to`, the way a person does:
 * dragging across them, or clicking one and shift-clicking the other. */
async function selectCells(page: Page, from: string, to: string, how: "drag" | "shift-click") {
  const editor = documentEditor(page);
  if (how === "drag") {
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
  how: "drag" | "shift-click",
  spec: string = table
) {
  const { alice, issue, page } = await openIssue(browser, "Cell selection paste", spec);
  await expect(documentEditor(page)).toContainText(to);
  await selectCells(page, from, to, how);
  return { alice, artifactId: issue.primary_artifact_id, page };
}

// Tab-separated text pasted onto a selection of cells fills them as a grid, one value per cell, the
// way a spreadsheet's copy pastes. It once threw inside prosemirror-tables once the selection
// reached the header row, whose cells must be header cells, and the browser then pasted the text
// into the last cell by itself.
for (const how of ["drag", "shift-click"] as const) {
  test(`tab-separated text pasted onto four cells selected by ${how} fills them as a grid`, async ({
    browser,
  }) => {
    const { alice, artifactId, page } = await openWithCellsSelected(
      browser,
      "alpha one",
      "delta four",
      how
    );
    try {
      await expect(page.locator(".selectedCell")).toHaveCount(4);

      await paste(page, { html: "", text: "H1\tH2\nB1\tB2" });

      await expect
        .poll(async () => (await getArtifactText(artifactId)).markdown)
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
    "| one two | beta two |\n| :--- | :--- |\n| one two | delta four |\n",
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
    "| alpha one | beta two |  |\n| :--- | :--- | :--- |\n| gamma three | b1 b2 | c |\n",
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
    "| alpha one | beta two |\n| :--- | :--- |\n| b1 b2 | c |\n",
  ],
  [
    "cells holding a line break",
    lineBreakCells,
    ["alpha one", "delta four"],
    "| b1 b2 | c |\n| :--- | :--- |\n| b1 b2 | c |\n",
  ],
] as const) {
  const where =
    typeof target === "string"
      ? `at the caret after "${target}"`
      : `onto ${target[0]} to ${target[1]}`;
  test(`${name} pasted ${where} keeps each cell on one line`, async ({ browser }) => {
    const { alice, issue, page } =
      typeof target === "string"
        ? await openWithCaret(browser, "Line break paste", table, target, "end")
        : await openIssue(browser, "Line break paste", table);
    try {
      if (typeof target !== "string") {
        await expect(documentEditor(page)).toContainText(target[1]);
        await selectCells(page, target[0], target[1], "shift-click");
        await expect(page.locator(".selectedCell")).not.toHaveCount(0);
      }

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
    "| alpha one | beta two |  |\n| :--- | :--- | :--- |\n| gamma three | a | b |\n",
  ],
  [emptyFirstRow, "alpha", "| a | b |\n| :--- | :--- |\n| gamma three | delta four |\n"],
  [
    emptyFirstRow,
    ["gamma three", "delta four"],
    "| alpha one | beta two |\n| :--- | :--- |\n| a | b |\n",
  ],
  [emptyLastRow, ["alpha one", "delta four"], "| a | b |\n| :--- | :--- |\n| a | b |\n"],
] as const) {
  const where =
    typeof target === "string"
      ? `at the caret after "${target}"`
      : `onto ${target[0]} to ${target[1]}`;
  const row = clipboard === emptyFirstRow ? "first" : "last";
  test(`cells whose ${row} row is empty, pasted ${where}, paste the other row`, async ({
    browser,
  }) => {
    const { alice, issue, page } =
      typeof target === "string"
        ? await openWithCaret(browser, "Empty row paste", table, target, "end")
        : await openIssue(browser, "Empty row paste", table);
    try {
      if (typeof target !== "string") {
        await expect(documentEditor(page)).toContainText(target[1]);
        await selectCells(page, target[0], target[1], "shift-click");
        await expect(page.locator(".selectedCell")).not.toHaveCount(0);
      }

      await paste(page, clipboard);

      await expect
        .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
        .toBe(stored);
    } finally {
      await alice.close();
    }
  });
}

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
    const { alice, artifactId, page } = await openWithCellsSelected(
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
        .poll(async () => (await getArtifactText(artifactId)).markdown)
        .toBe(
          "| alpha one | beta two |\n| :--- | :--- |\n| gamma three | delta four |\n| gamma three | delta four |\n"
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
  const { alice, artifactId, page } = await openWithCellsSelected(
    browser,
    "gamma three",
    "delta four",
    "drag"
  );
  try {
    await expect(page.locator(".selectedCell")).toHaveCount(2);

    await paste(page, { html: "", text: "First\n\nSecond\n" });

    await expect
      .poll(async () => (await getArtifactText(artifactId)).markdown)
      .toBe("| alpha one | beta two |\n| :--- | :--- |\n| First | Second |\n");
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
for (const [shape, spec, quote, clipboard, stored, breaks] of [
  [
    "a soft line break into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "IntroFirst\\\nSecond end.\n",
    1,
  ],
  [
    "a soft line break into a list item",
    "- Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "- IntroFirst\\\n  Second end.\n",
    1,
  ],
  [
    "a soft line break into a quote",
    "> Intro end.\n",
    "Intro",
    { html: "", text: "First\nSecond" },
    "> IntroFirst\\\n> Second end.\n",
    1,
  ],
  [
    "a soft line break into a callout",
    ':::callout{#k1 kind="note"}\nIntro end.\n:::\n',
    "Intro",
    { html: "", text: "First\nSecond" },
    ':::callout{#k1 kind="note" title=""}\nIntroFirst\\\nSecond end.\n:::\n',
    1,
  ],
  [
    "a soft line break into an ask's question",
    ':::ask{#q1 urgency="med" multiple="false"}\nWhich here?\n\n- X\n- Y\n:::\n',
    "Which here?",
    { html: "", text: "First\nSecond" },
    ':::ask{#q1 urgency="med" multiple="false" state="open"}\nWhich here?First\\\nSecond\n\n- X\n- Y\n:::\n',
    1,
  ],
  [
    "a markdown hard break into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "", text: "First\\\nSecond" },
    "IntroFirst\\\nSecond end.\n",
    1,
  ],
  [
    "an HTML <br> into a paragraph",
    "Intro end.\n",
    "Intro",
    { html: "<p>one<br>two</p>", text: "one\ntwo" },
    "Introone\\\ntwo end.\n",
    1,
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
      await expect(documentEditor(page).locator('br[data-type="hardbreak"]')).toHaveCount(breaks);
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
