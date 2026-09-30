import { expect, type Locator, type Page, test } from "@playwright/test";

import { openAgents, plannerSession, seedAgents, setLiveSessions } from "./agents";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The issue picker's keyboard-step rule (`markKeyStep` in `AgentsPage.tsx`) holds only because
// every engine dispatches a closed select's `change` inside the key's own task, where the HTML spec
// queues it as a task of its own. These rows notice an engine that moves to the spec's queued task:
// its first arrow or letter would commit at once. So the `webkit` and `firefox` projects run this
// spec as well as Chromium, and its rows drive keys through `page.keyboard`, which every engine has.
// The rows that step and then leave the select without Enter run in the same engines, since each
// engine takes focus out of a select its own way.

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

/** The send the rows below make, and the path of the one `POST` it produces. A comment and a
 *  direct message share this shape, so the row that calls this names the path it expects. */
async function sendAndCapturePath(page: Page, field: Locator): Promise<string> {
  const sent = page.waitForRequest(
    (request) =>
      request.method() === "POST" && /\/(comments|messages)$/.test(new URL(request.url()).pathname)
  );
  await field.fill("Status please");
  await field.press("Control+Enter");
  return new URL((await sent).url()).pathname;
}

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

  // A step is not a pick until Enter, so a reader who steps and then leaves the select any other
  // way has picked nothing: the open select goes back to the issue the message is addressed to,
  // and the toggle, the select and the send all name that one issue.
  for (const [way, leave] of [
    ["Tab", (page: Page) => page.keyboard.press("Tab")],
    ["Shift+Tab", (page: Page) => page.keyboard.press("Shift+Tab")],
    ["a click into the message", (_page: Page, field: Locator) => field.click()],
  ] as const) {
    test(`a step left by ${way} is dropped, so the select, the toggle and the send name one issue`, async ({
      browser,
    }) => {
      const issueKey = await seedAgents();
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
        await page.keyboard.press("ArrowDown");
        await page.keyboard.press("Enter");
        await expect(toggle).toContainText(issueKey);
        await field.fill("");
        await page.keyboard.press("Escape");
        await expect(row).toBeFocused();

        await page.keyboard.press("i");
        await expect(picker).toBeFocused();
        await page.keyboard.press("ArrowDown");
        await expect(picker).not.toHaveValue(issueKey);
        await leave(page, field);
        await expect(picker).not.toBeFocused();
        await expect(picker).toHaveValue(issueKey);
        await expect(toggle).toContainText(issueKey);

        expect(await sendAndCapturePath(page, field)).toBe(`/api/v1/issues/${issueKey}/comments`);
      } finally {
        await context.close();
      }
    });
  }

  // From `No issue` the message is a direct one, so a step left by Tab must not show an issue.
  test("a step from No issue left by Tab is dropped, and the message goes to the agent directly", async ({
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
      await page.keyboard.press("ArrowDown");
      await expect(picker).not.toHaveValue("");
      await page.keyboard.press("Tab");
      await expect(picker).not.toBeFocused();
      await expect(picker).toHaveValue("");
      await expect(toggle).toContainText("No issue");

      expect(await sendAndCapturePath(page, field)).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
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
