import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("tablet keeps the Conversation readable and exposes the review sheet", async ({
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
    anchor: { artifact: "spec", quote: "Review" },
    body: "Open this review item.",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1024, width: 800 });
    await page.goto(`/issues/${issue.key}/conversation`);

    await expect(page.getByRole("button", { name: "Open navigation" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();
    const conversationBounds = await page
      .getByRole("tabpanel", { name: "Conversation" })
      .boundingBox();
    expect(conversationBounds?.width ?? 0).toBeGreaterThanOrEqual(400);
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
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
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

// The full "tablet keeps the Conversation readable..." test above drives a real
// Playwright .click() on this same button and has hung on it: a raw DOM .click() bypasses
// hit-testing and would pass whether or not the button is actually reachable by a real
// pointer, which is exactly how an overlapping element hid here. This test asserts the
// hit-testing fact directly and fails in milliseconds rather than a 30s timeout.
test("the review panel toggle button is the element hit at its own center point", async ({
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
    anchor: { artifact: "spec", quote: "Review" },
    body: "Open this review item.",
  });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 1024, width: 800 });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: /Open review panel/ })).toBeVisible();

    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const toggle = page.getByRole("button", { name: /Close review panel/ });
    await expect(toggle).toBeVisible();
    await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    const hitTest = () =>
      toggle.evaluate((button) => {
        const rect = button.getBoundingClientRect();
        const elementAtCenter = document.elementFromPoint(
          rect.x + rect.width / 2,
          rect.y + rect.height / 2
        );
        return (
          elementAtCenter !== null &&
          (elementAtCenter === button || button.contains(elementAtCenter))
        );
      });
    await expect.poll(hitTest).toBe(true);
  } finally {
    await context.close();
  }
});
