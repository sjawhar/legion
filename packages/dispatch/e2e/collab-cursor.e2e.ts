import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText } from "./api";
import { documentEditor, placeCaret } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Runs in WebKit as well as Chromium (playwright.config.ts): a caret drawn beside a collaborator's
// caret is where the two engines, and only they, drop or misplace typing.
const spec = "## Database\n\n```ts\nconst x = 1;\nlet y = 2;\n```\n\nEnd of spec.\n";

function bobsCursorInCode(page: Page) {
  return documentEditor(page).locator("pre .proof-collab-cursor__label", { hasText: "bob" });
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
  test(`typing at a collaborator's cursor keeps what was typed: ${name}`, async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name === "iphone", "the chromium project covers Chromium");
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", spec, title: "Shared cursor" });
    const alice = await asUser(browser, "alice");
    const bob = await asUser(browser, "bob");

    try {
      const alicePage = await alice.newPage();
      await alicePage.goto(`/issues/${issue.key}/spec`);
      const bobPage = await bob.newPage();
      await bobPage.goto(`/issues/${issue.key}/spec`);
      await expect(alicePage.getByRole("status")).toHaveText("connected");
      await expect(bobPage.getByRole("status")).toHaveText("connected");

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
