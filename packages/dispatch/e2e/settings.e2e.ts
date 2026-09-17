import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
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

test("Refresh imports the architecture model and a rejected model keeps the previous import", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const files = {
    "api.md": "---\ntitle: HTTP API\ndepends_on: [store]\n---\nThe API surface.\n",
    "store.md": "---\ntitle: Store\n---\nThe durable state.\n",
  };
  await seedFakeGithub({ "legion/arch": { contents: "read", files, installation_id: 101 } });
  // The fake's commit sha is sha1 over the seeded files JSON, so the test can
  // assert the exact commit the row must show.
  const commit = createHash("sha1").update(JSON.stringify(files)).digest("hex");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto("/settings");
    await page.getByLabel("Source project").selectOption("CORE");
    await page.getByLabel("Source repository").fill("legion/arch");
    const sourceSaved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-source") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Add source" }).click();
    await sourceSaved;

    const synced = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-source/sync") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Refresh architecture source for CORE" }).click();
    await synced;
    await expect(page.getByText(`Synced ${commit.slice(0, 12)}`)).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-synced.png"),
      fullPage: true,
    });

    // A pushed model with a duplicate id (Api.md and api.md are both component
    // "api") is rejected whole: the row shows the reason and the previous
    // import — its commit — stays up.
    await seedFakeGithub({
      "legion/arch": {
        contents: "read",
        files: { "Api.md": "Duplicate casing.\n", "api.md": "Original.\n" },
        installation_id: 101,
      },
    });
    const rejected = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith("/api/v1/projects/CORE/architecture-source/sync") &&
        response.ok()
    );
    await page.getByRole("button", { name: "Refresh architecture source for CORE" }).click();
    await rejected;
    await expect(page.getByText(/duplicate component id/)).toBeVisible();
    await expect(page.getByText(`Previous model stays up at ${commit.slice(0, 12)}`)).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("architecture-source-sync-rejected.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
