import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("project board persists card reordering and explains daemon-owned columns", async ({
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
    await expect(daemonColumn).toContainText(
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
      const todoColumn = page.getByRole("region", { name: "Todo" });
      const thirdBox = await thirdHandle.boundingBox();
      const todoBox = await todoColumn.boundingBox();
      if (thirdBox === null || todoBox === null) {
        throw new Error("board column is not visible for status drag");
      }
      const statusPatch = page.waitForResponse(
        (response) =>
          response.request().method() === "PATCH" &&
          new URL(response.url()).pathname === `/api/v1/issues/${third.key}`
      );

      await page.mouse.move(thirdBox.x + thirdBox.width / 2, thirdBox.y + thirdBox.height / 2);
      await page.mouse.down();
      await page.mouse.move(todoBox.x + todoBox.width / 2, todoBox.y + todoBox.height / 2, {
        steps: 24,
      });
      await page.mouse.up();
      expect((await statusPatch).status()).toBe(200);
      await expect(todoColumn.getByRole("article")).toHaveText(/Third card/);
      await page.reload();
      await expect(todoColumn.getByRole("article")).toHaveText(/Third card/);
    }

    if (testInfo.project.name === "iphone") {
      await expect(page.getByRole("button", { name: "Board" })).toHaveCSS("min-height", "44px");
    }
  } finally {
    await context.close();
  }
});
