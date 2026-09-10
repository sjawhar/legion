import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("tablet keeps the log readable and exposes the review sheet", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the tablet viewport uses the desktop browser project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this tablet issue.",
    title: "Review at tablet width",
  });
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", from: 0, to: 6 },
    body: "Open this review item.",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1024, width: 800 });
    await page.goto(`/issues/${issue.key}/log`);

    await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();
    const logBounds = await page.getByRole("tabpanel", { name: "Log" }).boundingBox();
    expect(logBounds?.width ?? 0).toBeGreaterThanOrEqual(400);
    const tabletScreenshot = testInfo.outputPath("tablet-800.png");
    await page.screenshot({ path: tabletScreenshot, fullPage: true });
    await testInfo.attach("tablet layout (800px)", {
      contentType: "image/png",
      path: tabletScreenshot,
    });

    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    await expect(page.getByRole("button", { name: /Close review panel/ })).toBeVisible();
    await expect(page.getByTestId(`margin-comment-${comment.id}`)).toBeVisible();
    await page.getByRole("button", { name: /Close review panel/ }).click();
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();

    await page.setViewportSize({ height: 1024, width: 1280 });
    await expect(page.getByRole("navigation", { name: "Issues" })).toBeVisible();
    await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
    const desktopScreenshot = testInfo.outputPath("desktop-1280.png");
    await page.screenshot({ path: desktopScreenshot, fullPage: true });
    await testInfo.attach("desktop layout (1280px)", {
      contentType: "image/png",
      path: desktopScreenshot,
    });
  } finally {
    await context.close();
  }
});
