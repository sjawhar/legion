import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("issue tabs are URL-driven and keyboard-navigable, the sidebar marks the current issue, and titles follow the route", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ship the release" });
  await createIssue({ parent: issue.key, project: "CORE", title: "Child task" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.goto("/");
  await expect(page).toHaveTitle("Inbox · Dispatch");

  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  // Navigating via the sidebar link closes the drawer automatically (onNavigate).
  await page.getByRole("link", { name: new RegExp(issue.key) }).click();

  // D34 — the page title follows the route, including the issue's own title.
  await expect(page).toHaveTitle(`${issue.key} · ${issue.title} · Dispatch`);

  // D9 — the sidebar marks the issue you are on.
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByRole("link", { name: new RegExp(issue.key) })).toHaveAttribute(
    "aria-current",
    "page"
  );
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Close navigation" }).click();
  }

  // D10 — the active tab is a route segment and survives a reload.
  await page.getByRole("tab", { name: "Children" }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/children$`));
  await page.reload();
  await expect(page.getByRole("tab", { name: "Children" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page.getByRole("tabpanel")).toContainText("Child task");
  await page.screenshot({
    path: testInfo.outputPath("issue-children-tab-after-reload.png"),
    fullPage: true,
  });

  // D26 — roving tabindex: arrow keys move focus and activate the adjacent tab.
  await page.getByRole("tab", { name: "Children" }).focus();
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Log" })).toBeFocused();
  await expect(page.getByRole("tab", { name: "Log" })).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/log$`));
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: "Children" })).toBeFocused();
  await expect(page.getByRole("tab", { name: "Children" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/children$`));

  // D15 — a missing issue gets a real "not found" view with a way back, not a bare sentence.
  await page.goto("/issues/CORE-999");
  await expect(page.getByRole("heading", { name: "Issue not found" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();

  // An unrecognized tab suffix on an otherwise valid issue also gets the
  // not-found view (and the not-found title), not the Log tab it would
  // silently fall back to. (The route-gate short-circuits before any of
  // IssuePage's own queries run — proven directly, without the sidebar's
  // own unrelated issue fetch as noise, by the IssuePage unit test.)
  await page.goto(`/issues/${issue.key}/not-a-tab`);
  await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expect(page.getByRole("tablist", { name: "Issue detail" })).toHaveCount(0);
  await expect(page).toHaveTitle("Not found · Dispatch");

  // D36 — an unrecognized URL gets a real not-found view instead of a silent redirect.
  await page.goto("/definitely-not-a-dispatch-route");
  await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
  await expect(page).toHaveTitle("Not found · Dispatch");
  await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();

  await context.close();
});
