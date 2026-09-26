import { type Browser, expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText, getIssue } from "./api";
import { documentEditor, openWithCaret, paste } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

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

/** Opens the table as alice and selects the cells from the one holding `from` to the one holding
 * `to`, the way a person does: dragging across them, or clicking one and shift-clicking the other. */
async function openWithCellsSelected(
  browser: Browser,
  from: string,
  to: string,
  how: "drag" | "shift-click"
): Promise<{ alice: Awaited<ReturnType<typeof asUser>>; artifactId: string; page: Page }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: table, title: "Cell selection paste" });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}`);
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
