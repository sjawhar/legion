import { expect, test } from "@playwright/test";

import { createIssue, createMessage, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

test("new events leave the reader's scroll position unchanged", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByText("Existing message 29")).toBeVisible();
  await page.waitForTimeout(1_100);
  await page.evaluate(() => window.scrollTo(0, 500));
  const before = await page.evaluate(() => window.scrollY);
  expect(before).toBeGreaterThan(0);

  await createMessage(issue.key, { body: "Arrived while reading" }, session);

  await expect(page.getByText("New")).toBeVisible();
  await expect(page.getByText("Arrived while reading")).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(before);

  await context.close();
});
