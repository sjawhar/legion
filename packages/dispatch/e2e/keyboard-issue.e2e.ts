import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** The keymap is bound only once sign-in resolves (`AuthGate` renders a skeleton until
 *  `/auth/whoami` answers), so a key pressed before the page renders reaches no handler. */
async function openIssue(page: Page, key: string, title: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expect(page.getByRole("heading", { level: 1, name: title })).toBeVisible();
  await page.locator("body").focus();
}

/** Leaves the control a shortcut just focused: a single letter never fires while an `INPUT`,
 *  `TEXTAREA` or `SELECT` holds focus, so each row starts from the page itself. `body.focus()`
 *  would not do it - `body` takes no focus, so the control keeps it - and the page is asserted
 *  back on `body` because that state is what the row after it tests. */
async function leaveFocus(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the issue scope reaches the header controls and the tab chords, and ? lists them under Issue", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Keyboard issue");
    const header = page.getByTestId("issue-header");
    const priority = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    const status = page.getByRole("combobox", { name: "Status" });

    // p and s reach their own control: two assertions, so a p that focused Status fails.
    await page.keyboard.press("p");
    await expect(priority).toBeFocused();
    await leaveFocus(page);
    await page.keyboard.press("s");
    await expect(status).toBeFocused();
    await leaveFocus(page);

    // A digit sets P0–P3 through the same write the picker uses, and it survives a reload.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await page.keyboard.press("2");
    expect((await patch).postDataJSON()).toEqual({ priority: 2 });
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await leaveFocus(page);

    // l opens the labels popover - the listbox itself, not just its trigger.
    await page.keyboard.press("l");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toHaveCount(0);
    await leaveFocus(page);

    // Shift+P pins through the header's own control, and the pin is saved.
    await page.keyboard.press("Shift+P");
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await page.reload();
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await leaveFocus(page);

    // e edits the title, where every single key is text again: typing "sp0" neither focused
    // Status or the priority nor set P0, and Escape discards the edit.
    await page.keyboard.press("e");
    const titleInput = page.getByLabel("Issue title");
    await expect(titleInput).toBeFocused();
    await page.keyboard.type("sp0");
    await expect(titleInput).toBeFocused();
    await expect(titleInput).toHaveValue(/sp0/);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("heading", { level: 1, name: "Keyboard issue" })).toBeVisible();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        priority: 2,
        title: "Keyboard issue",
      });
    await leaveFocus(page);

    // t is a chord: the indicator shows it waiting, then the second key picks that tab. c is a
    // global shortcut on its own (the create dialog), and completing the chord is not it.
    await page.keyboard.press("t");
    const indicator = page.getByTestId("chord-indicator");
    await expect(indicator).toHaveText(/t/);
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-chord-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("c");
    await expect(indicator).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));

    for (const [key, tab] of [
      ["h", "Children"],
      ["a", "Artifacts"],
      ["s", "Spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page.getByRole("tab", { name: tab })).toHaveAttribute("aria-selected", "true");
    }

    // ? lists the scope as "Issue"; the digits collapse to one 0 – 3 row.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const issueSection = dialog.getByRole("region", { name: "Issue", exact: true });
    await expect(issueSection).toContainText("Set priority P0–P3");
    await expect(issueSection).toContainText("Focus the status");
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Set priority P0–P3" }).locator("kbd")
    ).toHaveText(["0", "3"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Conversation tab" }).locator("kbd")
    ).toHaveText(["t", "c"]);
    await page.screenshot({
      path: testInfo.outputPath(`issue-shortcut-help-${testInfo.project.name}.png`),
    });
  } finally {
    await context.close();
  }
});

test("the project List view roves with j/k and opens the focused row with Enter and o", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string) => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    return issue;
  };
  const alpha = await seed("Alpha");
  const bravo = await seed("Bravo");
  await seed("Charlie");
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/projects/CORE/issues");
    const rows = page.getByRole("list", { name: "Todo issues" }).getByRole("listitem");
    // The seeded order is the order the rows rove in, so assert it before roving over it.
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();

    await page.keyboard.press("j");
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toBeFocused();
    await page.keyboard.press("k");
    await expect(rows.nth(0)).toBeFocused();
    await page.screenshot({
      path: testInfo.outputPath(`project-list-roving-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));

    // o opens the row the reader is on, not the first one.
    await page.goBack();
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await page.keyboard.press("j");
    await expect(rows.nth(1)).toBeFocused();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${bravo.key}$`));

    await page.goBack();
    await expect(rows).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await page.locator("body").focus();
    await page.keyboard.press("?");
    const projectSection = page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("region", { name: "Project", exact: true });
    await expect(projectSection).toContainText("Next issue");
    await expect(projectSection).toContainText("Open issue");
  } finally {
    await context.close();
  }
});
