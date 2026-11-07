import { expect, type Locator, test } from "@playwright/test";

import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import { listsSpacedByIndentedCodeReading } from "../web/src/__tests__/pmdoc-leaf-readings";
import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

// The whitespace-only lines that end an indented code block read as the browser's engine
// (milkdown/micromark) reads them: a line with spaces past the block's indent stays in the block
// as those spaces, a line without them drops off its end, and neither spreads the list or the item
// around the block (sjawhar/legion#2481). One reader of that answer needs a browser and the rest
// do not: a past version hands the stored markdown to the live Milkdown editor (VersionView.tsx's
// `setMarkdown`), so this row is the editor's own parse of what the server stored, and it reads
// its document from the one fixture table. The spec editor binds to the Yjs fragment the server
// writes, which `fixtures.json` pins without a browser (`pm_json`, `yjs_update_v1_b64`); the
// comment's headless parse, with this reader of its code blocks and spreads, is
// `web/src/features/refs/fixture-table-readings.test.ts`'s "an indented code block's whitespace
// lines stay in the block and spread no list or item", which runs it over this document and over
// the two the file's other rows held.
//
// A paragraph reading holds none of this - a code block's own text, and the `data-spread` of each
// list and item - so this row keeps a reader of its own. The reading it reads against is the unit
// test's, shared from `web/src/__tests__/pmdoc-leaf-readings.ts`.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-indented-code-whitespace-lines" },
  as: "agent" as const,
};

const entry = fixture("lists-spaced-by-indented-code-whitespace-lines");

/** Asserts `view` draws the document as `listsSpacedByIndentedCodeReading`, once its last
 *  paragraph shows. The whole reading is compared as one JSON line, so a failure prints every
 *  value, each code block's whitespace escaped. */
async function expectEngineReading(view: Locator): Promise<void> {
  await expect(view.getByText("End.", { exact: true })).toBeVisible();
  await expect
    .poll(async () =>
      JSON.stringify({
        code: await view
          .locator("pre code")
          .evaluateAll((blocks) => blocks.map((block) => block.textContent)),
        items: await view
          .locator("li")
          .evaluateAll((items) => items.map((item) => item.getAttribute("data-spread"))),
        lists: await view
          .locator("ul, ol")
          .evaluateAll((lists) => lists.map((list) => list.getAttribute("data-spread"))),
      })
    )
    .toBe(JSON.stringify(listsSpacedByIndentedCodeReading));
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("an indented code block's trailing whitespace lines stay in the block and spread nothing in a past version of the spec", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: entry.markdown, title: "Indented code whitespace lines" },
    agentSession
  );
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${issue.key}/artifacts/spec?v=1`);
    await expectEngineReading(page.getByTestId("version-view"));
  });
});
