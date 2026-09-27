import { expect, test } from "@playwright/test";

import { createIssue, createProject, getArtifactText } from "./api";
import { selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Runs in Firefox as well as Chromium (playwright.config.ts): Firefox's own editing puts text
// typed over the last line of a code block before that line's newline.
test.beforeEach(async () => {
  await resetDatabase();
});

test("typing over the whole last line of a code block replaces that line", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Database\n\n```ts\nconst x = 1;\nlet y = 2;\n```\n\nEnd of spec.\n",
    title: "Code line",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(page.getByRole("status")).toHaveText("connected");

    await selectEditorText(page, "let y = 2;");
    await page.keyboard.type("hello");
    await expect
      .poll(() => getArtifactText(issue.primary_artifact_id).then(({ markdown }) => markdown))
      .toContain("```ts\nconst x = 1;\nhello\n```");
  } finally {
    await alice.close();
  }
});
