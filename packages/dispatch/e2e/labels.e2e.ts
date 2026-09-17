import { expect, type Locator, test } from "@playwright/test";

import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { filterPicker, pickFilterOption } from "./filters";
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
    await expect(page).toHaveURL(/\/projects\/CORE\/issues\?label=docs$/);
    await filterPicker(page, "Labels").click();
    await expect(page.getByRole("option", { exact: true, name: "docs" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);
    if (testInfo.project.name === "iphone") {
      await expectTouchTarget(filterPicker(page, "Labels"));
      await expectTouchTarget(page.getByRole("option", { exact: true, name: "docs" }));
      await expectTouchTarget(page.getByRole("button", { name: "Clear labels" }));
    }
    await pickFilterOption(page, "Labels", "docs");
    await expect(page).toHaveURL(/\/projects\/CORE\/issues$/);
    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await pickFilterOption(page, "Labels", "Frontend");
    await expect(page.getByText("Editable labels", { exact: true })).toBeVisible();
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Backend documentation", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Labels", "docs");
    await expect(page.getByText("Frontend documentation", { exact: true })).toBeVisible();
    await expect(page.getByText("Editable labels", { exact: true })).toHaveCount(0);

    if (testInfo.project.name === "iphone") {
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

test("eight thirty-character labels scroll inside the header's details line without widening the page", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the viewports are set explicitly in the desktop browser project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long labels" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const title = page.getByRole("heading", { level: 1, name: "Long labels" });
    const rail = page.getByTestId("issue-metadata-rail");
    for (const [width, height] of [
      [1280, 900],
      [390, 844],
    ]) {
      await page.setViewportSize({ height, width });
      await patchIssue(issue.key, { labels: [] });
      await page.goto(`/issues/${issue.key}`);
      await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
      const titleBefore = await title.boundingBox();

      await patchIssue(issue.key, {
        labels: Array.from({ length: 8 }, (_, index) => `label-${index}-`.padEnd(30, "x")),
      });
      await page.reload();
      await expect(page.getByText("label-7-".padEnd(30, "x"), { exact: true })).toBeVisible();

      // The title box is untouched and the details line is the scroll container: it overflows
      // sideways inside the card while the document itself is no wider than the window.
      expect(await title.boundingBox()).toEqual(titleBefore);
      const metrics = await rail.evaluate((element) => ({
        clientWidth: element.clientWidth,
        overflowX: getComputedStyle(element).overflowX,
        scrollWidth: element.scrollWidth,
      }));
      expect(metrics.overflowX).toBe("auto");
      expect(metrics.scrollWidth).toBeGreaterThan(metrics.clientWidth);
      expect(metrics.clientWidth).toBeLessThanOrEqual(width);
      const pageWidths = await page.evaluate(() => ({
        clientWidth: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(pageWidths.scrollWidth).toBe(pageWidths.clientWidth);
      expect(pageWidths.clientWidth).toBeLessThanOrEqual(width);
      const shot = testInfo.outputPath(`labels-overflow-${width}.png`);
      await page.screenshot({ path: shot });
      await testInfo.attach(`eight long labels (${width}px)`, {
        contentType: "image/png",
        path: shot,
      });
    }
  } finally {
    await context.close();
  }
});
