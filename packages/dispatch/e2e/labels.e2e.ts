import { expect, type Locator, test } from "@playwright/test";

import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("edits issue labels from a searchable multi-select and filters project issues by every selected label", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const edited = await createIssue({ project: "CORE", title: "Editable labels" });
  const matching = await createIssue({ project: "CORE", title: "Frontend documentation" });
  const other = await createIssue({ project: "CORE", title: "Backend documentation" });
  await patchIssue(edited.key, { labels: ["Frontend", "api"] });
  await patchIssue(matching.key, { labels: ["Frontend", "docs"] });
  await patchIssue(other.key, { labels: ["backend", "docs"] });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${edited.key}`);
    const editLabels = page.getByRole("button", { name: "Edit labels" });
    await editLabels.click();
    const labelSearch = page.getByRole("combobox", { name: "Search or create label" });
    await expect(page.getByRole("option", { name: "Frontend", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("option", { name: "api", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("option", { name: "backend", exact: true })).toHaveAttribute(
      "aria-selected",
      "false"
    );
    await labelSearch.fill("urgent");
    await expect(page.getByRole("option", { name: 'Create "urgent"' })).toBeVisible();
    await labelSearch.press("Enter");
    await expect(page.getByRole("option", { name: "urgent", exact: true })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    if (process.env.DISPATCH_LABEL_SCREENSHOT_DIR !== undefined) {
      const screenshotName =
        testInfo.project.name === "iphone" ? "labels-390.png" : "labels-1280.png";
      await page.screenshot({
        fullPage: true,
        path: `${process.env.DISPATCH_LABEL_SCREENSHOT_DIR}/${screenshotName}`,
      });
    }
    if (testInfo.project.name === "iphone") {
      await expectTouchTarget(editLabels);
      await expectTouchTarget(labelSearch);
    }

    await editLabels.click();
    await expect
      .poll(() => getIssue(edited.key))
      .toMatchObject({
        labels: ["Frontend", "api", "urgent"],
      });

    await page.goto("/projects/CORE?label=docs");
    await expect(page).toHaveURL(/\/projects\/CORE\?label=docs$/);
    await expect(page.getByRole("button", { name: "docs" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "docs" }).click();
    await expect(page).toHaveURL(/\/projects\/CORE$/);
    await page.getByRole("button", { name: "Frontend" }).click();
    await expect(page.getByText("Editable labels", { exact: true })).toBeVisible();
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "docs" }).click();
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);

    if (testInfo.project.name === "iphone") {
      await expectTouchTarget(page.getByRole("button", { name: "docs" }));
      await expectTouchTarget(page.getByRole("button", { name: "Clear labels" }));
      await expect(
        page.evaluate(
          () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
        )
      ).resolves.toBe(true);
    }
  } finally {
    await context.close();
  }
});
