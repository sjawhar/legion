import { expect, type Locator, test } from "@playwright/test";

import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import { createComment, createIssue, createProject, getArtifactText } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A bare web address after punctuation or an escaped scheme colon is a link remark-gfm reads, and
// a document written through the API holds the same links a comment holding the same markdown
// does. This family has no past-version reading row of its own: its one browser row reads the
// stored document in a browser, and what it reads is a link's name and href, which no paragraph
// reading holds. Its document and the text of each of its paragraphs are the one fixture table's
// `bare-addresses-each-with-its-own-host`, whose `pm_json` pins the link marks below without a
// browser and whose paragraphs `web/src/features/refs/fixture-table-readings.test.ts` reads in a
// DOM. The rows that held a `www.`, an uppercase scheme or a bare email are shapes of
// `testdata/corpus/bare-address-after-punctuation.md`, which the Go reader's fixture tests pin
// against the engine's `pm_json`, link marks and all.

const entry = fixture("bare-addresses-each-with-its-own-host");

/** The links the engine reads in the entry's nine address paragraphs, one list per paragraph, in
 *  the order of the paragraphs after the corpus file's opening sentence. Taken from the entry's
 *  `pm_json` link marks, which `pmdoc/gen/differential.ts` recorded from the engine; a host per
 *  paragraph keeps every link name unique on the page, since a reader tells two links apart by
 *  their accessible names. The engine's two readers end the last two addresses differently: its
 *  tokenizer drops a trailing `_` after `;`, its transform keeps it after an escaped colon. */
const links: { href: string; name: string }[][] = [
  [{ href: "http://lower.test", name: "http://lower.test" }],
  [{ href: "https://semi.test", name: "https://semi.test" }],
  [{ href: "https://hash.test", name: "https://hash.test" }],
  [{ href: "https://amp.test", name: "https://amp.test" }],
  [{ href: "https://star.test", name: "https://star.test" }],
  [
    { href: "http://www.a.test/?u=", name: "www.a.test/?u=" },
    { href: "https://b.test", name: "https://b.test" },
  ],
  [{ href: "https://quote.test", name: "https://quote.test" }],
  [{ href: "https://a.test/a", name: "https://a.test/a" }],
  [{ href: "http://b.test/a_", name: "http://b.test/a_" }],
];

test.beforeEach(async () => {
  await resetDatabase();
});

test.describe("a bare web address is a link in a document written through the API, as in a comment", () => {
  // SLOW(>=2s): 3,811 ms in this head's four-worker CI report (`dispatch-e2e-report-1` of run
  // 42334783358), 2,545 ms at the merge base's merge-group run. Nine paragraphs, each a bare
  // address, asserted twice - once in the Conversation and once in the document editor - which
  // is ten link lookups per surface.
  test("after punctuation or an escaped scheme colon", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: entry.markdown,
      title: "Address links",
    });
    await createComment(issue.key, { body: entry.markdown });

    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      // The corpus file's opening sentence holds no address; the nine paragraphs after it are the
      // rows, each paired with its own links.
      const rows = links.map((row, index) => ({
        links: row,
        text: entry.reading.paragraphs[index + 1].text,
      }));

      /** Asserts `root` shows every row's text and the name and href of every link in it. */
      const expectRows = async (root: Locator): Promise<void> => {
        for (const row of rows) {
          await expect(root).toContainText(row.text);
          for (const link of row.links) {
            await expect(root.getByRole("link", { name: link.name, exact: true })).toHaveAttribute(
              "href",
              link.href
            );
          }
        }
      };

      await page.goto(`/issues/${issue.key}/conversation`);
      await expectRows(page.getByRole("list", { name: "Conversation turns" }));

      // Each row's text is asserted before its links, so a row Go read as text fails as "text
      // present, no link".
      await openSpec(page, issue.key, rows[0].text);
      await expectRows(documentEditor(page));

      expect((await getArtifactText(issue.primary_artifact_id)).markdown).not.toContain("\\:");
    } finally {
      await context.close();
    }
  });
});
