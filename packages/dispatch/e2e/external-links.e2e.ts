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

    // The page keeps nothing for a pointer alone (dispatch://INFRA-1196 R21): the address is
    // drawn whole, the refusal is drawn in words beside it, and neither is a link.
    const unsafeLink = page.getByText(unsafeUrl);
    await expect(unsafeLink).toBeVisible();
    await expect(unsafeLink).not.toHaveAttribute("href");
    await expect(unsafeLink.locator("..")).toContainText("(unsafe external link)");
  } finally {
    await context.close();
  }
});
