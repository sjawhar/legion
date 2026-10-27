import { type Browser, expect, test } from "@playwright/test";

import { createComment, createIssue, createProject, getArtifactText } from "./api";
import { documentEditor, openSpec } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** A paragraph of markdown, the text the engine shows for it, and the links it reads in it. */
interface AddressRow {
  markdown: string;
  text: string;
  links: { name: string; href: string }[];
}

// Each row is a bare web address remark-gfm reads as a link: the engine a comment renders with.
// Each row's `links` are the engine's reading, taken from `pmdoc/gen/differential.ts`, and a host
// per row keeps every link name unique on the page.
const afterPunctuation: AddressRow[] = [
  {
    markdown: "see http\\://lower.test b",
    text: "see http://lower.test b",
    links: [{ name: "http://lower.test", href: "http://lower.test" }],
  },
  {
    markdown: "x;https://semi.test b",
    text: "x;https://semi.test b",
    links: [{ name: "https://semi.test", href: "https://semi.test" }],
  },
  {
    markdown: "x#https://hash.test b",
    text: "x#https://hash.test b",
    links: [{ name: "https://hash.test", href: "https://hash.test" }],
  },
  {
    markdown: "x&https://amp.test b",
    text: "x&https://amp.test b",
    links: [{ name: "https://amp.test", href: "https://amp.test" }],
  },
  {
    markdown: "\\*https://star.test b",
    text: "*https://star.test b",
    links: [{ name: "https://star.test", href: "https://star.test" }],
  },
  {
    markdown: '"www.a.test/?u=https://b.test"',
    text: '"www.a.test/?u=https://b.test"',
    links: [
      { name: "www.a.test/?u=", href: "http://www.a.test/?u=" },
      { name: "https://b.test", href: "https://b.test" },
    ],
  },
  {
    markdown: '"https://quote.test" b',
    text: '"https://quote.test" b',
    links: [{ name: "https://quote.test", href: "https://quote.test" }],
  },
  // The engine's two readers end these addresses differently: its tokenizer drops a trailing `_`
  // after `;`, its transform keeps it after an escaped colon.
  {
    markdown: "x;https://a.test/a_ b",
    text: "x;https://a.test/a_ b",
    links: [{ name: "https://a.test/a", href: "https://a.test/a" }],
  },
  {
    markdown: "see http\\://b.test/a_ b",
    text: "see http://b.test/a_ b",
    links: [{ name: "http://b.test/a_", href: "http://b.test/a_" }],
  },
];

// A `www.` or an uppercase scheme after an escape. Before INFRA-531 Go read each as text and wrote
// that text bare, which its own reader reads as a link, so it refused the whole write: these rows
// have a document of their own, so that refusal does not hide the rows above.
const afterAnEscape: AddressRow[] = [
  {
    markdown: "see www\\.www1.test b",
    text: "see www.www1.test b",
    links: [{ name: "www.www1.test", href: "http://www.www1.test" }],
  },
  {
    markdown: "see HTTP\\://upper.test b",
    text: "see HTTP://upper.test b",
    links: [{ name: "HTTP://upper.test", href: "HTTP://upper.test" }],
  },
  {
    markdown: "\\(www.paren.test b",
    text: "(www.paren.test b",
    links: [{ name: "www.paren.test", href: "http://www.paren.test" }],
  },
];

/**
 * Writes `rows`, a paragraph each, as an issue's spec and as a comment on it, and expects each
 * address to be the engine's link in the Conversation and then in the live Document editor, which
 * shows the tree Go stored, with no backslash before a scheme's colon in the stored markdown.
 */
async function expectLinksInCommentAndDocument(browser: Browser, rows: AddressRow[]) {
  const markdown = `${rows.map((row) => row.markdown).join("\n\n")}\n`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: markdown, title: "Address links" });
  await createComment(issue.key, { body: markdown });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();

    await page.goto(`/issues/${issue.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    for (const row of rows) {
      await expect(turns).toContainText(row.text);
      for (const link of row.links) {
        await expect(turns.getByRole("link", { name: link.name, exact: true })).toHaveAttribute(
          "href",
          link.href
        );
      }
    }

    // Each row's text is asserted before its links, so a row Go read as text fails as "text
    // present, no link".
    await openSpec(page, issue.key, rows[0].text);
    const editor = documentEditor(page);
    for (const row of rows) {
      await expect(editor).toContainText(row.text);
      for (const link of row.links) {
        await expect(editor.getByRole("link", { name: link.name, exact: true })).toHaveAttribute(
          "href",
          link.href
        );
      }
    }

    expect((await getArtifactText(issue.primary_artifact_id)).markdown).not.toContain("\\:");
  } finally {
    await context.close();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test.describe("a bare web address is a link in a document written through the API, as in a comment", () => {
  test("after punctuation or an escaped scheme colon", async ({ browser }) => {
    await expectLinksInCommentAndDocument(browser, afterPunctuation);
  });

  test("a www. or an uppercase scheme after an escape", async ({ browser }) => {
    await expectLinksInCommentAndDocument(browser, afterAnEscape);
  });
});
