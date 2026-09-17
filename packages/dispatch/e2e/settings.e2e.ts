import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** Replaces the fake GitHub's installations wholesale: repositories present are
 * "App installed" with the given Contents permission; absent ones answer 404. */
async function seedFakeGithub(
  repos: Record<string, { contents: string; installation_id: number }>
): Promise<void> {
  const port = process.env.FAKE_GITHUB_PORT ?? "9022";
  const response = await fetch(`http://127.0.0.1:${port}/__fixture/repos`, {
    body: JSON.stringify(repos),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`seed fake github: ${response.status}`);
  }
}

test("a repository mapping added in Settings assigns a new external issue to its selected project", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Repositories → Projects" })).toBeVisible();
    await page.getByLabel("Repository", { exact: true }).fill("Owner/Repo.git");
    await page.getByRole("combobox", { exact: true, name: "Project" }).selectOption("CORE");
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
      page.getByRole("combobox", { exact: true, name: "Project" }).locator('option[value="QA"]')
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

test("an architecture source saves after the GitHub App access check, fails inline once uninstalled, and deletes", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({ "legion/arch": { contents: "write", installation_id: 101 } });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: "Architecture sources" })).toBeVisible();
    await page.getByLabel("Source project").selectOption("CORE");
    await page.getByLabel("Source repository").fill("Legion/Arch");
    const sourceSaved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-source") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Add source" }).click();
    await sourceSaved;
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "main" })).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "Access verified" })).toBeVisible();

    // The App gets uninstalled; re-saving fails its access check inline and the
    // stored source survives.
    await seedFakeGithub({});
    await page.getByLabel("Source project").selectOption("CORE");
    await page.getByLabel("Source repository").fill("legion/arch");
    await page.getByRole("button", { name: "Add source" }).click();
    await expect(
      page.getByRole("alert").filter({ hasText: "the GitHub App is not installed on legion/arch" })
    ).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-settings.png"),
      fullPage: true,
    });

    await page.getByRole("button", { name: "Delete architecture source for CORE" }).click();
    await expect(page.getByText("No architecture sources yet.")).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).not.toBeVisible();
  } finally {
    await context.close();
  }
});
