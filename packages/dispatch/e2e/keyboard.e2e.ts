import { expect, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, getIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard" },
  as: "agent" as const,
};

async function seedInbox(): Promise<{ issueKey: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard decision" });
  const options = [{ label: "Ship" }, { label: "Hold" }, { label: "Wait" }];
  await createAsk(issue.key, { options, question: "Older ask" }, session);
  await createAsk(issue.key, { options, question: "Newest ask" }, session);
  return { issueKey: issue.key };
}

async function openInbox(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator("[data-testid^=ask-]")).toHaveCount(2);
  await page.locator("body").focus();
}

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

test("g then i goes to the Inbox, showing the pending chord until it completes", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/agents");
    await expect(page).toHaveURL(/\/agents$/);
    await page.locator("body").focus();

    const indicator = page.getByTestId("chord-indicator");
    await page.keyboard.press("g");
    await expect(indicator).toBeVisible();
    await expect(indicator).toHaveText(/g/);
    await page.screenshot({
      path: testInfo.outputPath(`chord-indicator-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("i");
    await expect(page).toHaveURL(/\/$/);
    await expect(indicator).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("an unfinished chord expires after a second and the next key stands alone", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/agents");
    await page.locator("body").focus();

    const indicator = page.getByTestId("chord-indicator");
    await page.keyboard.press("g");
    await expect(indicator).toBeVisible();
    await expect(indicator).toHaveCount(0, { timeout: 2000 });
    await page.keyboard.press("i");
    await page.waitForTimeout(300);
    await expect(page).toHaveURL(/\/agents$/);
  } finally {
    await context.close();
  }
});

test("? lists the registry with reserved keys greyed, filters live, and Escape returns to the row", async ({
  browser,
}, testInfo) => {
  await seedInbox();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openInbox(page);
    const rows = page.locator("[data-inbox-row]");
    await page.keyboard.press("j");
    await expect(rows.nth(0)).toBeFocused();

    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const filter = dialog.getByRole("searchbox", { name: "Filter shortcuts" });
    await expect(filter).toBeFocused();

    const goToInbox = dialog.getByRole("listitem").filter({ hasText: "Go to Inbox" });
    await expect(goToInbox.locator("kbd")).toHaveText(["g", "i"]);
    await expect(goToInbox).toHaveAttribute("data-enabled", "true");
    const reserved = dialog.getByRole("listitem").filter({ hasText: "bulk action" });
    await expect(reserved).toHaveAttribute("data-enabled", "false");
    await expect(reserved.locator("kbd")).toHaveText(["x"]);
    // Row-bound shortcuts describe the row that was focused when ? was pressed.
    await expect(
      dialog.getByRole("listitem").filter({ hasText: "Answer the focused ask" })
    ).toHaveAttribute("data-enabled", "true");
    await expect(dialog.getByRole("region", { name: "Global" })).toBeVisible();
    await expect(dialog.getByRole("region", { name: "Inbox" })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`shortcut-help-${testInfo.project.name}.png`),
    });

    await filter.fill("agents");
    await expect(dialog.getByRole("listitem")).toHaveCount(1);
    await expect(dialog.getByRole("listitem")).toContainText("Go to Agents");

    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(rows.nth(0)).toBeFocused();
  } finally {
    await context.close();
  }
});

test("Inbox rows rove with j/k, digits pick options, Enter and Escape move between row and answer, o opens the issue", async ({
  browser,
}, testInfo) => {
  const { issueKey } = await seedInbox();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openInbox(page);
    const rows = page.locator("[data-inbox-row]");
    const first = rows.nth(0);

    await page.keyboard.press("j");
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toBeFocused();
    await page.keyboard.press("k");
    await expect(first).toBeFocused();
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`inbox-focused-row-${testInfo.project.name}.png`),
    });

    await page.keyboard.press("2");
    await expect(first.getByRole("radio", { name: "Hold" })).toBeChecked();
    await expect(first.getByRole("radio", { name: "Ship" })).not.toBeChecked();

    await page.keyboard.press("Enter");
    const answer = first.getByRole("textbox", { name: "Your answer" });
    await expect(answer).toBeFocused();
    await page.keyboard.type("j?");
    await expect(answer).toHaveValue("j?");
    await expect(page).toHaveURL(/\/$/);
    await expect(page.getByRole("dialog")).toHaveCount(0);

    // The search toggle is the one shortcut that still works while typing.
    await page.keyboard.press("Control+k");
    const search = page.getByRole("dialog", { name: "Search" });
    await expect(search).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(search).toHaveCount(0);
    await expect(answer).toBeFocused();

    await page.keyboard.press("Escape");
    await expect(first).toBeFocused();
    await expect(answer).toHaveValue("j?");

    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${issueKey}`));
  } finally {
    await context.close();
  }
});

test("c opens the create dialog; project and title create an issue the API returns and the SPA opens", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
    await page.locator("body").focus();

    await page.keyboard.press("c");
    const dialog = page.getByRole("dialog", { name: "Create issue" });
    await expect(dialog).toBeVisible();
    const title = dialog.getByRole("textbox", { name: "Title" });
    await expect(title).toBeFocused();
    // `c` inside the dialog's own inputs types; it never reopens or steals the key.
    await title.fill("c");
    await expect(title).toHaveValue("c");
    await dialog.getByRole("combobox", { name: "Project" }).selectOption("OPS");
    await title.fill("Keyboard-created issue");
    await page.screenshot({
      path: testInfo.outputPath(`create-issue-${testInfo.project.name}.png`),
    });
    await title.press("Enter");

    await expect(page).toHaveURL(/\/issues\/OPS-1(\/|$)/);
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Keyboard-created issue" })).toBeVisible();
    await expect
      .poll(() => getIssue("OPS-1"))
      .toMatchObject({
        project: "OPS",
        title: "Keyboard-created issue",
      });

    // Escape closes without creating, and focus returns to the caller.
    await page.locator("main").focus();
    await page.keyboard.press("c");
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.locator("main")).toBeFocused();
  } finally {
    await context.close();
  }
});
