import { expect, type Locator, test } from "@playwright/test";

import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import {
  type LeafRow,
  leafMarksInOneDocumentReading,
} from "../web/src/__tests__/pmdoc-leaf-readings";
import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

// An inline leaf - raw HTML, a picture, a footnote reference - carries no text of its own, so no
// mark may wrap it: the server's reader holds emphasis, strong, strikethrough and link marks on
// text alone, and the browser's engine reads them the same way. An inline HTML declaration (`<!`
// and an ASCII letter) is raw HTML to both readers. One reader of that answer needs a browser and
// the rest do not: a past version hands the stored markdown to the live Milkdown editor
// (VersionView.tsx's `setMarkdown`), so this row is the editor's own parse of what the server
// stored, and it reads its document from the one fixture table. The spec editor binds to the Yjs
// fragment the server writes, which `fixtures.json` pins without a browser (`pm_json`,
// `yjs_update_v1_b64`); the comment's headless parse, with this reader of its leaves, is
// `web/src/features/refs/fixture-table-readings.test.ts`'s, which runs it over both leaf-mark
// documents, over every declaration spelling and over the whole table; and the bytes the server
// stores for a declaration and for a label holding an autolink are
// `packages/envoy/internal/dispatch/pmdoc/writer_spelling_test.go`'s.
//
// A paragraph reading holds none of this - a leaf carries no text, and whitespace is part of what
// is read here - so this row keeps a reader of its own. The rows it reads against are the unit
// test's, shared from `web/src/__tests__/pmdoc-leaf-readings.ts`.

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-leaf-marks" },
  as: "agent" as const,
};

const entry = fixture("leaf-marks-in-one-document");

/** Reads every paragraph `view` draws in one pass over the rendered DOM, so a failure reports the
 *  whole table rather than the first row that differs. Only the document's own leaves are read:
 *  after a paragraph whose last child is a leaf node, ProseMirror appends an
 *  `<img class="ProseMirror-separator" alt="">` of its own, beside its trailing `<br>`, so that
 *  the line keeps its height and the caret has somewhere to stand. That image is editor chrome,
 *  carrying no `src` and standing for nothing in the document, so it is no leaf of it. */
function readParagraphs(view: Locator): Promise<LeafRow[]> {
  return view.locator("p").evaluateAll((rendered) =>
    rendered.map((paragraph) => ({
      leaves: [
        ...paragraph.querySelectorAll(
          'span[data-type="html"], img:not(.ProseMirror-separator), sup[data-type="footnote_reference"]'
        ),
      ].map((leaf) => {
        const marks: string[] = [];
        for (
          let ancestor = leaf.parentElement;
          ancestor !== null && ancestor !== paragraph;
          ancestor = ancestor.parentElement
        ) {
          const tag = ancestor.tagName.toLowerCase();
          if (tag === "strong" || tag === "em" || tag === "del" || tag === "a") {
            marks.push(tag);
          }
        }
        return {
          identity:
            leaf.getAttribute("data-value") ??
            leaf.getAttribute("src") ??
            leaf.getAttribute("data-label") ??
            "",
          kind: leaf.tagName.toLowerCase(),
          marks,
        };
      }),
      text: paragraph.textContent ?? "",
    }))
  );
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("no mark reaches an inline leaf in a past version of the spec, and a declaration is raw HTML", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: entry.markdown, title: "Inline leaves" },
    agentSession
  );
  await withAlice(browser, async (page) => {
    await page.goto(`/issues/${issue.key}/artifacts/spec?v=1`);
    const view = page.getByTestId("version-view");
    // The paragraph count shows once the view has drawn; the whole table is then compared at once.
    await expect(view.locator("p")).toHaveCount(leafMarksInOneDocumentReading.length);
    await expect.poll(() => readParagraphs(view)).toEqual(leafMarksInOneDocumentReading);
  });
});
