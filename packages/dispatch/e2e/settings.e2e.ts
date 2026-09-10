import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("a repository mapping added in Settings assigns a new external issue to its selected project", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Repositories → Projects" })).toBeVisible();
    await page.getByLabel("Repository").fill("Owner/Repo.git");
    await page.getByRole("combobox", { name: "Project" }).selectOption("CORE");
    await page.getByRole("button", { name: "Add mapping" }).click();
    await expect(page.getByRole("cell", { exact: true, name: "owner/repo" })).toBeVisible();

    const issue = await createIssue({ external: "owner/repo#1" });
    expect(issue.key).toBe("CORE-1");
    await page.screenshot({
      path: testInfo.outputPath("repository-project-settings.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
