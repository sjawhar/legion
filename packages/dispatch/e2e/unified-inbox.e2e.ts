import { expect, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test("the shadow page checks coverage and the unified inbox is opt-in @phone", async ({
  browser,
}, testInfo) => {
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unified inbox" });
  await createAsk(issue.key, { question: "May we ship?" });
  const root = await createComment(issue.key, { body: "Please check the release." });
  await createComment(
    issue.key,
    { body: "The release is checked.", reply_to: root.id },
    { as: "agent", actor: { kind: "session", id: "release-reader" } }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    if (testInfo.project.name === "chromium")
      await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox", exact: true })).toBeVisible();
    await page.getByRole("link", { name: "Inbox comparison" }).click();
    await expect(
      page.getByRole("heading", { name: "Does the new inbox miss anything?" })
    ).toBeVisible();
    await expect(page.getByTestId("inbox-shadow-mismatches")).toHaveText("0 differences");
    await expect(page.getByText("All active people checked", { exact: true })).toBeVisible();
    await expect(page.getByText("Not ready to replace your inbox", { exact: true })).toBeVisible();
    const geometry = await page.evaluate(() => ({
      width: window.innerWidth,
      height: window.innerHeight,
      pageWidth: document.documentElement.scrollWidth,
      pageHeight: document.body.scrollHeight,
    }));
    console.log(`Inbox comparison first screen: ${JSON.stringify(geometry)}`);
    expect(geometry.pageWidth).toBeLessThanOrEqual(geometry.width);
    expect(geometry.pageHeight).toBeLessThanOrEqual(geometry.height);
    if (testInfo.project.name === "iphone") {
      await page.setViewportSize({ width: 390, height: 844 });
      const tallHeight = await page.evaluate(() => document.body.scrollHeight);
      console.log(`Inbox comparison 390x844 page height: ${tallHeight}`);
      expect(tallHeight).toBeLessThanOrEqual(844);
    }
    await page.screenshot({ path: testInfo.outputPath("inbox-shadow.png"), fullPage: true });
    await page.getByRole("link", { name: "Try unified inbox" }).click();
    await expect(page.getByRole("heading", { name: "Unified inbox · preview" })).toBeVisible();
    const rows = page.getByRole("list", { name: "Unified inbox" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    await expect(rows.filter({ hasText: "May we ship?" })).toHaveCount(1);
    const reply = rows.filter({ hasText: "The release is checked." });
    await expect(reply).toHaveCount(1);
    await expect(reply.getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${root.id}`
    );
    await page.screenshot({ path: testInfo.outputPath("unified-inbox.png"), fullPage: true });
    await page.getByRole("link", { name: "Return to current inbox" }).click();
    await expect(page).toHaveURL((url) => url.pathname === "/");
    await expect(page.getByRole("heading", { name: "Inbox", exact: true })).toBeVisible();
  } finally {
    await alice.close();
  }
});
