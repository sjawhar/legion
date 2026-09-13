import { expect, test } from "@playwright/test";

import { createIssue, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("project board persists reordering, lets humans close and reopen, and explains daemon-owned columns", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First card" });
  await createIssue({ project: "CORE", title: "Second card" });
  const third = await createIssue({ project: "CORE", title: "Third card" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ width: 1920, height: 900 });
  }
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const triage = page.getByRole("region", { name: "Triage" });
    await expect(triage.getByRole("article")).toHaveText([
      /First card/,
      /Second card/,
      /Third card/,
    ]);

    const handle = page.getByRole("button", { name: `Reorder ${third.key}` });
    const target = triage.getByRole("article", { name: `${first.key} First card` });
    const sourceBox = await handle.boundingBox();
    const targetBox = await target.boundingBox();
    if (sourceBox === null || targetBox === null) {
      throw new Error("board card is not visible for drag");
    }
    const reorderPatch = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${third.key}`
    );

    await page.mouse.move(sourceBox.x + sourceBox.width / 2, sourceBox.y + sourceBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(targetBox.x + targetBox.width / 2, targetBox.y + 4, { steps: 12 });
    await page.mouse.up();
    expect((await reorderPatch).status()).toBe(200);

    await expect(triage.getByRole("article")).toHaveText([
      /Third card/,
      /First card/,
      /Second card/,
    ]);

    await page.reload();
    await expect(triage.getByRole("article")).toHaveText([
      /Third card/,
      /First card/,
      /Second card/,
    ]);
    await page.getByRole("button", { name: "List" }).click();
    await expect(
      page.getByRole("list", { name: "triage issues" }).getByRole("listitem")
    ).toHaveText([/Third card/, /First card/, /Second card/]);

    await page.getByRole("button", { name: "Board" }).click();
    const daemonColumn = page.getByRole("region", { name: "In progress" });
    const daemonLock = daemonColumn.getByRole("img", { name: "Daemon controlled" });
    await expect(daemonLock).toHaveAttribute(
      "aria-description",
      "Only the Legion daemon can move issues to In progress."
    );
    const firstHandle = page.getByRole("button", { name: `Reorder ${first.key}` });
    const firstBox = await firstHandle.boundingBox();
    if (firstBox === null) {
      throw new Error("board card is not visible for refused status drag");
    }
    await page.mouse.move(firstBox.x + firstBox.width / 2, firstBox.y + firstBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(firstBox.x + firstBox.width / 2 + 10, firstBox.y + firstBox.height / 2, {
      steps: 2,
    });
    await expect(daemonColumn).toHaveAttribute("data-drop-disabled", "true");
    await daemonColumn.scrollIntoViewIfNeeded();
    const daemonBox = await daemonColumn.boundingBox();
    if (daemonBox === null) {
      throw new Error("board column is not visible for refused status drag");
    }
    await page.mouse.move(daemonBox.x + daemonBox.width / 2, daemonBox.y + daemonBox.height / 2, {
      steps: 24,
    });
    await page.mouse.up();
    await expect(page.getByRole("alert")).toHaveText(
      "Only the Legion daemon can move issues to In progress."
    );
    await expect(triage).toContainText("First card");
    await expect(daemonColumn).not.toContainText("First card");

    if (testInfo.project.name === "chromium") {
      const thirdHandle = page.getByRole("button", { name: `Reorder ${third.key}` });
      await thirdHandle.scrollIntoViewIfNeeded();
      const thirdBox = await thirdHandle.boundingBox();
      if (thirdBox === null) {
        throw new Error("board card is not visible for close drag");
      }
      const closePatch = page.waitForResponse(
        (response) =>
          response.request().method() === "PATCH" &&
          new URL(response.url()).pathname === `/api/v1/issues/${third.key}`
      );

      await page.mouse.move(thirdBox.x + thirdBox.width / 2, thirdBox.y + thirdBox.height / 2);
      await page.mouse.down();
      await page.mouse.move(thirdBox.x + thirdBox.width / 2 + 10, thirdBox.y + thirdBox.height / 2);
      const doneColumn = page.getByRole("region", { name: "Done" });
      await doneColumn.scrollIntoViewIfNeeded();
      const doneBox = await doneColumn.boundingBox();
      if (doneBox === null) {
        throw new Error("board column is not visible for close drop");
      }
      await page.mouse.move(doneBox.x + doneBox.width / 2, doneBox.y + doneBox.height / 2, {
        steps: 24,
      });
      await page.mouse.up();
      expect((await closePatch).status()).toBe(200);
      await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);
      await page.reload();
      await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);

      await doneColumn.scrollIntoViewIfNeeded();
      const closeHandle = page.getByRole("button", { name: `Reorder ${third.key}` });
      const closeBox = await closeHandle.boundingBox();
      if (closeBox === null) {
        throw new Error("board card is not visible for reopen drag");
      }
      const reopenPatch = page.waitForResponse(
        (response) =>
          response.request().method() === "PATCH" &&
          new URL(response.url()).pathname === `/api/v1/issues/${third.key}`
      );

      await page.mouse.move(closeBox.x + closeBox.width / 2, closeBox.y + closeBox.height / 2);
      await page.mouse.down();
      await page.mouse.move(closeBox.x + closeBox.width / 2 + 10, closeBox.y + closeBox.height / 2);
      const backlogColumn = page.getByRole("region", { name: "Backlog" });
      await backlogColumn.scrollIntoViewIfNeeded();
      const backlogBox = await backlogColumn.boundingBox();
      if (backlogBox === null) {
        throw new Error("board column is not visible for reopen drop");
      }
      await page.mouse.move(
        backlogBox.x + backlogBox.width / 2,
        backlogBox.y + backlogBox.height / 2,
        {
          steps: 24,
        }
      );
      await page.mouse.up();
      expect((await reopenPatch).status()).toBe(200);
      await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
      await page.reload();
      await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
    }

    if (testInfo.project.name === "iphone") {
      await expect(page.getByRole("button", { name: "Board" })).toHaveCSS("min-height", "44px");
    }
  } finally {
    await context.close();
  }
});

test("project list and board cards show an issue priority", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Prioritized card" });
  await patchIssue(issue.key, { priority: 1 });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    await expect(
      page.getByRole("list", { name: "triage issues" }).getByText("P1", { exact: true })
    ).toBeVisible();

    await page.getByRole("button", { name: "Board" }).click();
    await expect(
      page
        .getByRole("article", { name: `${issue.key} Prioritized card` })
        .getByText("P1", { exact: true })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("board scrolls horizontally inside its own container at 1100px", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the board scroll check exercises a desktop viewport"
  );
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Board scrolls without widening the page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.setViewportSize({ width: 1100, height: 800 });
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const boardScroller = page.getByTestId("board-scroll-container");
    await expect(boardScroller).toBeVisible();
    const dimensions = await boardScroller.evaluate((element) => ({
      clientWidth: element.clientWidth,
      pageWidth: document.documentElement.scrollWidth,
      scrollWidth: element.scrollWidth,
      viewportWidth: window.innerWidth,
    }));
    expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
    expect(dimensions.pageWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
  } finally {
    await context.close();
  }
});
