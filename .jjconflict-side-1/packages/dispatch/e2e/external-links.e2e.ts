import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { insertExternalLink, resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("an unsafe stored external link is rendered as inert text", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unsafe link" });
  const unsafeUrl = "javascript:alert(document.domain)";
  await insertExternalLink(issue.key, unsafeUrl);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/issues/${issue.key}`);

    const unsafeLink = page.getByText(unsafeUrl);
    await expect(unsafeLink).toHaveAttribute("title", "Unsafe external link");
    await expect(unsafeLink).not.toHaveAttribute("href");
  } finally {
    await context.close();
  }
});
