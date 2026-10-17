import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";

import { createIssue, createProject, getIssue, putRepoProject } from "./api";
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
    const repository = page.getByLabel("Repository", { exact: true });
    // The browser compiles `pattern` with the `v` flag and ignores one that does not compile, so a
    // refusal here says `ownerRepoPattern` compiles as the browser reads it.
    await repository.fill("a/b/c");
    expect(
      await repository.evaluate((input: HTMLInputElement) => input.validity.patternMismatch)
    ).toBe(true);
    await repository.fill("Owner/Repo.git");
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

test("a project's default repository moves with its Default radio, and a mapping an issue names stays", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject("acme/widgets", { default: true, project: "CORE" });
  await putRepoProject("acme/gadgets", { project: "CORE" });
  const pinned = await createIssue({ project: "CORE", repo: "acme/widgets", title: "Pinned" });
  const inheriting = await createIssue({ force: true, project: "CORE", title: "Inheriting" });
  expect(inheriting.resolved_repo).toBe("acme/widgets");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const radio = (repo: string) =>
    page.getByRole("radio", { name: `Default repository of CORE: ${repo}` });

  try {
    await page.goto("/settings");
    await expect(page.getByRole("columnheader", { name: "Default" })).toBeVisible();
    await expect(radio("acme/widgets")).toBeChecked();
    await expect(radio("acme/gadgets")).not.toBeChecked();

    const saved = page.waitForRequest(
      (request) =>
        request.method() === "PUT" &&
        new URL(request.url()).pathname === "/api/v1/settings/repo-projects/acme/gadgets"
    );
    await radio("acme/gadgets").click();
    expect((await saved).postDataJSON()).toEqual({ default: true, project: "CORE" });
    await expect(radio("acme/gadgets")).toBeChecked();
    await expect(radio("acme/widgets")).not.toBeChecked();
    // An issue with no repository of its own resolves to the new default; one with its own keeps it.
    await expect
      .poll(() => getIssue(inheriting.key))
      .toMatchObject({ resolved_repo: "acme/gadgets" });
    expect(await getIssue(pinned.key)).toMatchObject({
      repo: "acme/widgets",
      resolved_repo: "acme/widgets",
    });
    await page.reload();
    await expect(radio("acme/gadgets")).toBeChecked();

    // The pinned issue names acme/widgets, so its mapping is not deleted, and the page says why.
    await page.getByRole("button", { name: "Delete mapping for acme/widgets" }).click();
    await expect(
      page.getByText(
        "1 issue(s) of CORE name acme/widgets as their repository; change their repo first"
      )
    ).toBeVisible();
    await expect(page.getByRole("cell", { exact: true, name: "acme/widgets" })).toBeVisible();
    const shot = testInfo.outputPath(`repository-default-${testInfo.project.name}.png`);
    await page.screenshot({ fullPage: true, path: shot });
    await testInfo.attach(`repository default (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });
  } finally {
    await context.close();
  }
});

// A mapping write's answer describes one row, not the project's mappings when it arrives. Here the
// server saves the pick of acme/gadgets at once, another browser makes acme/widgets the default
// again (which this page hears and shows), and only then does the first answer reach the page: the
// page reads the mappings again rather than put acme/gadgets back.
test("a late answer to a Default pick does not undo a default another browser set since", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject("acme/widgets", { default: true, project: "CORE" });
  await putRepoProject("acme/gadgets", { project: "CORE" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const radio = (repo: string) =>
    page.getByRole("radio", { name: `Default repository of CORE: ${repo}` });

  try {
    let answer: () => void = () => {};
    const answerHeld = new Promise<void>((resolve) => {
      answer = resolve;
    });
    let saved: () => void = () => {};
    const savedOnServer = new Promise<void>((resolve) => {
      saved = resolve;
    });
    await page.route("**/api/v1/settings/repo-projects/acme/gadgets", async (route) => {
      if (route.request().method() !== "PUT") {
        await route.continue();
        return;
      }
      const response = await route.fetch();
      saved();
      await answerHeld;
      await route.fulfill({ response });
    });
    await page.goto("/settings");
    await expect(radio("acme/widgets")).toBeChecked();

    await radio("acme/gadgets").click();
    await savedOnServer;
    await putRepoProject("acme/widgets", { default: true, project: "CORE" });
    await expect(radio("acme/widgets")).toBeChecked();

    const answered = page.waitForResponse(
      (response) =>
        response.url().endsWith("/api/v1/settings/repo-projects/acme/gadgets") &&
        response.request().method() === "PUT"
    );
    const readAgain = answered.then(() =>
      page.waitForResponse(
        (response) =>
          new URL(response.url()).pathname === "/api/v1/settings/repo-projects" &&
          response.request().method() === "GET"
      )
    );
    answer();
    await answered;
    await readAgain;
    await expect(radio("acme/widgets")).toBeChecked();
    await expect(radio("acme/gadgets")).not.toBeChecked();
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
    await page.getByLabel("Key", { exact: true }).fill("qa");
    await page.getByLabel("Name", { exact: true }).fill("Quality");
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

// The Architecture sources table scrolls inside its own container, but its `sr-only` header
// cell is absolutely positioned: without a positioned ancestor inside that container it sits
// at the scrolled table's own x and widened the page to 417px at 390px.
test("Settings stays inside the viewport", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const width = testInfo.project.name === "chromium" ? 1280 : 390;
    if (testInfo.project.name === "chromium") {
      await page.setViewportSize({ height: 900, width });
    }
    await page.goto("/settings");
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Architecture sources" })).toBeVisible();

    const measured = await page.evaluate(() => ({
      client: document.documentElement.clientWidth,
      scroll: document.documentElement.scrollWidth,
    }));
    expect(measured.scroll).toBeLessThanOrEqual(measured.client);
  } finally {
    await alice.close();
  }
});
