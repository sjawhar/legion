import { expect, test } from "@playwright/test";

import { openAgents, seedAgents, setLiveSessions } from "./agents";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The issue picker's keyboard-step rule (`markKeyStep` in `AgentsPage.tsx`) holds only because
// every engine dispatches a closed select's `change` inside the key's own task, where the HTML spec
// queues it as a task of its own. These rows notice an engine that moves to the spec's queued task:
// its first arrow or letter would commit at once. So the `webkit` and `firefox` projects run this
// spec as well as Chromium, and its rows drive keys through `page.keyboard`, which every engine has.

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

test.describe("agents page", () => {
  // The arrows choose and Enter commits, so a reader can pass the first option to reach the
  // second - and the Enter that picks is the picker's, not a newline at the top of the message.
  test("the arrows move the issue selection and Enter commits it", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const picker = row.getByRole("combobox", { name: "Issue" });
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");

      // Escape after an arrow takes nothing: the selection was never committed. (The field is
      // emptied first, since Escape over a written composer is its own discard prompt.)
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowUp");
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(row).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // Type-ahead is a step as the arrows are: the letter moves the selection and Enter commits it.
  test("type-ahead from the keyboard moves the issue selection and Enter commits it", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = row.getByRole("combobox", { name: "Issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("c");
      await expect(picker).toHaveValue("CORE-1");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("@Planner");
    } finally {
      await context.close();
    }
  });
});
