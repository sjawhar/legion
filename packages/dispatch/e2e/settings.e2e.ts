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
    const mappingSaved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/settings/repo-projects/owner/repo") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Add mapping" }).click();
    await mappingSaved;
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

test("a human creates a project from Settings and immediately creates an issue under it", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(
      page.locator("main").getByRole("heading", { exact: true, name: "Projects" })
    ).toBeVisible();
    await page.getByLabel("Key").fill("qa");
    await page.getByLabel("Name").fill("Quality");
    await page.getByRole("button", { name: "New project" }).click();
    await expect(page.getByRole("cell", { exact: true, name: "QA" })).toBeVisible();
    await expect(
      page.getByRole("combobox", { name: "Project" }).locator('option[value="QA"]')
    ).toHaveCount(1);
    await page.screenshot({
      path: testInfo.outputPath("project-create-settings.png"),
      fullPage: true,
    });

    const issue = await createIssue({ project: "QA", title: "Track quality issues" });
    expect(issue.key).toBe("QA-1");

    await page.goto(`/issues/${issue.key}`);
    await expect(page).toHaveTitle(`${issue.key} · ${issue.title} · Dispatch`);
  } finally {
    await context.close();
  }
});
