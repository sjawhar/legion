import { expect, test } from "@playwright/test";

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

// A paste onto a selection of whole cells stays prosemirror-tables' own grid paste: each pasted line
// fills one selected cell. The flattening is for a caret in a cell's text, and applied to a cell
// selection it would put all the pasted text into every selected cell.
test("plain text pasted onto a selection of cells fills them line by line", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: table, title: "Cell selection paste" });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const editor = documentEditor(page);
    const from = await editor.getByText("gamma three").boundingBox();
    const to = await editor.getByText("delta four").boundingBox();
    if (!from || !to) throw new Error("the body cells have no layout");
    await page.mouse.move(from.x + 5, from.y + from.height / 2);
    await page.mouse.down();
    await page.mouse.move(to.x + to.width / 2, to.y + to.height / 2, { steps: 10 });
    await page.mouse.up();
    await expect(page.locator(".selectedCell")).toHaveCount(2);

    await paste(page, { html: "", text: "First\n\nSecond\n" });

    await expect
      .poll(async () => (await getArtifactText(issue.primary_artifact_id)).markdown)
      .toBe("| alpha one | beta two |\n| :--- | :--- |\n| First | Second |\n");
  } finally {
    await alice.close();
  }
});
