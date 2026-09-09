import { expect, test } from "@playwright/test";

import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("an authenticated user sees their empty inbox", async ({ browser }, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto("/");

  await expect(page.getByText("Signed in as alice")).toBeVisible();
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("empty-inbox.png"), fullPage: true });

  await context.close();
});

test("an anonymous user is sent to GitHub sign-in", async ({ browser }) => {
  const context = await browser.newContext();
  const page = await context.newPage();

  await page.goto("/");

  await expect(page.getByRole("link", { name: "Sign in with GitHub" })).toHaveAttribute(
    "href",
    "/auth/start"
  );

  await context.close();
});
