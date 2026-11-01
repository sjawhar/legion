import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText } from "./api";
import { connectedDot, documentEditor, placeCaret } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Runs in WebKit as well as Chromium (playwright.config.ts): a caret drawn beside a collaborator's
// caret is where the two engines, and only they, drop or misplace typing.
const spec = "## Database\n\n```ts\nconst x = 1;\nlet y = 2;\n```\n\nEnd of spec.\n";

function bobsCursorInCode(page: Page) {
  return documentEditor(page).locator("pre .proof-collab-cursor__label", { hasText: "bob" });
}

function bobsCursor(page: Page) {
  return documentEditor(page).locator(".proof-collab-cursor__label", { hasText: "bob" });
}

test.beforeEach(async () => {
  await resetDatabase();
});

const cases = [
  {
    name: "a new line at the end of a code block",
    edge: "after" as const,
    type: async (page: Page) => {
      await page.keyboard.press("Enter");
      await page.keyboard.type("hello");
    },
    code: "const x = 1;\nlet y = 2;\nhello",
  },
  {
    name: "the start of a code line",
    edge: "before" as const,
    type: (page: Page) => page.keyboard.type("Z"),
    code: "const x = 1;\nZlet y = 2;",
  },
];

for (const { name, edge, type, code } of cases) {
  // SLOW(>=2s): Two signed-in collaborators are two browser contexts with an editor each, in the
  // engine that misplaces typing beside a caret; the time is two WebKit editor mounts.
  test(`typing at a collaborator's cursor keeps what was typed: ${name}`, async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", spec, title: "Shared cursor" });
    const alice = await asUser(browser, "alice");
    const bob = await asUser(browser, "bob");

    try {
      const [alicePage, bobPage] = await Promise.all([alice.newPage(), bob.newPage()]);
      // The two readers' loads overlap: neither waits on the other to mount its editor.
      await Promise.all([
        alicePage.goto(`/issues/${issue.key}/spec`),
        bobPage.goto(`/issues/${issue.key}/spec`),
      ]);
      await expect(connectedDot(alicePage)).toHaveText("connected");
      await expect(connectedDot(bobPage)).toHaveText("connected");

      await placeCaret(bobPage, edge, "let y = 2;");
      await expect(bobsCursorInCode(alicePage)).toBeVisible();

      await placeCaret(alicePage, edge, "let y = 2;");
      await type(alicePage);
      await expect
        .poll(() => getArtifactText(issue.primary_artifact_id).then(({ markdown }) => markdown))
        .toContain(`\`\`\`ts\n${code}\n\`\`\``);

      // Once Alice's caret leaves, Bob's shows again where he left it.
      await placeCaret(alicePage, "after", "End of spec.");
      await expect(bobsCursorInCode(alicePage)).toBeVisible();
    } finally {
      await bob.close();
      await alice.close();
    }
  });
}

// SLOW(>=2s): Two contexts, one per collaborator, each with a mounted editor in WebKit; the row
// waits on no timer.
test("a reader outside the editor sees a collaborator's cursor even where their own stale selection sits", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "Shared cursor" });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const [alicePage, bobPage] = await Promise.all([alice.newPage(), bob.newPage()]);
    // The two readers' loads overlap: neither waits on the other to mount its editor.
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await expect(connectedDot(alicePage)).toHaveText("connected");
    await expect(connectedDot(bobPage)).toHaveText("connected");

    // Alice never clicked, so her selection is the start of the document.
    await placeCaret(bobPage, "before", "Database");
    await expect(bobsCursor(alicePage)).toBeVisible();

    // Alice clicked at the end, then left the editor; Bob's caret lands where hers was.
    await placeCaret(alicePage, "after", "End of spec.");
    await documentEditor(alicePage).evaluate((editor) => (editor as HTMLElement).blur());
    await placeCaret(bobPage, "after", "End of spec.");
    await expect(bobsCursor(alicePage)).toBeVisible();
  } finally {
    await bob.close();
    await alice.close();
  }
});
