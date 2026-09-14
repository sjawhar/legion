import { expect, type Locator, test } from "@playwright/test";

import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The visible priority tag inside a card or row - the `<select>` options carry the same text. */
function priorityBadge(scope: Locator, label: string): Locator {
  return scope.locator("span", { hasText: new RegExp(`^${label}$`) });
}

test("project board persists reordering and lets humans move cards through every status", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First card" });
  await patchIssue(first.key, { status: "todo" });
  const second = await createIssue({ project: "CORE", title: "Second card" });
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
    await expect(triage.getByRole("article")).toHaveText([/Second card/, /Third card/]);

    const handle = page.getByRole("button", { name: `Reorder ${third.key}` });
    const target = triage.getByRole("article", { name: `${second.key} Second card` });
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

    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);

    await page.reload();
    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
    await page.getByRole("button", { name: "List" }).click();
    await expect(
      page.getByRole("list", { name: "triage issues" }).getByRole("listitem")
    ).toHaveText([/Third card/, /Second card/]);

    await page.getByRole("button", { name: "Board" }).click();
    const todoColumn = page.getByRole("region", { name: "Todo" });
    const inProgressColumn = page.getByRole("region", { name: "In progress" });
    const todoHandle = page.getByRole("button", { name: `Reorder ${first.key}` });
    await todoHandle.scrollIntoViewIfNeeded();
    const todoBox = await todoHandle.boundingBox();
    if (todoBox === null) {
      throw new Error("Todo board card is not visible for status drag");
    }
    const movePatch = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${first.key}`
    );
    await page.mouse.move(todoBox.x + todoBox.width / 2, todoBox.y + todoBox.height / 2);
    await page.mouse.down();
    await page.mouse.move(todoBox.x + todoBox.width / 2 + 10, todoBox.y + todoBox.height / 2);
    await inProgressColumn.scrollIntoViewIfNeeded();
    const inProgressBox = await inProgressColumn.boundingBox();
    if (inProgressBox === null) {
      throw new Error("In progress board column is not visible for status drag");
    }
    await page.mouse.move(
      inProgressBox.x + inProgressBox.width / 2,
      inProgressBox.y + inProgressBox.height / 2,
      { steps: 24 }
    );
    await page.mouse.up();
    expect((await movePatch).status()).toBe(200);
    await expect(todoColumn).not.toContainText("First card");
    await expect(inProgressColumn).toContainText("First card");
    await page.reload();
    await expect(inProgressColumn).toContainText("First card");
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

test("project list rows and board cards set an issue's priority in place", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Prioritized card" });
  await patchIssue(issue.key, { priority: 1 });
  const unset = await createIssue({ project: "CORE", title: "Unprioritized card" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    const listRow = page.getByRole("listitem", { name: `${issue.key} Prioritized card` });
    const listControl = listRow.getByLabel(`Priority of ${issue.key}`);
    await expect(priorityBadge(listRow, "P1")).toBeVisible();
    // The badge stays small; the tap target around it is 44 px on a phone (32 px from md).
    const listBox = await listControl.boundingBox();
    expect(listBox?.height ?? 0).toBeGreaterThanOrEqual(
      testInfo.project.name === "iphone" ? 44 : 32
    );
    const listPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await listControl.selectOption("3");
    expect((await listPatch).postDataJSON()).toEqual({ priority: 3 });
    await expect(priorityBadge(listRow, "P3")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 3 });

    await page.getByRole("button", { name: "Board" }).click();
    const card = page.getByRole("article", { name: `${issue.key} Prioritized card` });
    await expect(priorityBadge(card, "P3")).toBeVisible();
    const cardControl = card.getByLabel(`Priority of ${issue.key}`);
    await cardControl.focus();
    const cardPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await cardControl.selectOption("2");
    expect((await cardPatch).postDataJSON()).toEqual({ priority: 2 });
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    // Using the control neither followed the card's link nor dragged the card.
    expect(new URL(page.url()).pathname).toBe("/projects/CORE");
    await expect(card).not.toHaveClass(/opacity-50/);
    await expect(page.getByRole("region", { name: "Triage" }).getByRole("article")).toHaveText([
      /Prioritized card/,
      /Unprioritized card/,
    ]);

    const unsetCard = page.getByRole("article", { name: `${unset.key} Unprioritized card` });
    await expect(priorityBadge(unsetCard, "Priority")).toBeVisible();
    const unsetPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${unset.key}`
    );
    await unsetCard.getByLabel(`Priority of ${unset.key}`).selectOption("0");
    expect((await unsetPatch).postDataJSON()).toEqual({ priority: 0 });
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();

    // Tab reaches the control right after the card's Reorder handle, so a keyboard shortcut can
    // focus it too; the screenshot shows the focus ring on the badge.
    await page.getByRole("button", { name: `Reorder ${unset.key}` }).focus();
    await page.keyboard.press("Tab");
    await expect(unsetCard.getByLabel(`Priority of ${unset.key}`)).toBeFocused();
    const shot = testInfo.outputPath(`board-priority-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`board priority (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    await page.reload();
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();
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
