import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { createAsk, createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard-inbox" },
  as: "agent" as const,
};

/** Three asks on three issues, so each row's `Select <owner>` checkbox has its own name. */
async function seedInbox(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["First", "Second", "Third"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title} decision` }, session);
  }
}

// The keymap binds only once sign-in resolves (`AuthGate` renders a skeleton until
// `/auth/whoami` answers), so a key pressed before the page renders reaches no handler.
async function openInbox(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(3);
  await page.locator("body").focus();
}

async function openAgents(page: Page): Promise<void> {
  await page.goto("/agents");
  await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
  await page.locator("body").focus();
}

function snoozeRequest(page: Page, askId: string) {
  return page.waitForRequest(
    (request) =>
      request.method() === "PUT" &&
      new URL(request.url()).pathname === `/api/v1/me/asks/${askId}/snooze`
  );
}

async function askIdOf(page: Page, index: number): Promise<string> {
  const id = await page.locator("[data-inbox-row]").nth(index).getAttribute("data-inbox-row");
  expect(id).not.toBeNull();
  return id as string;
}

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

// Both describes run in `chromium` and `iphone`: marking asks and snoozing them in one step is
// exactly what a reader does on a phone, and the bar and the picker have to fit 390 px.
test.describe("inbox selection", () => {
  test("x marks rows, h snoozes every marked ask into Later, and Escape clears the selection", async ({
    browser,
  }, testInfo) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const first = await askIdOf(page, 0);
      const second = await askIdOf(page, 1);

      // x marks the focused row; j moves on and x marks the next one.
      await page.keyboard.press("j");
      await expect(rows.nth(0)).toBeFocused();
      await page.keyboard.press("x");
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await expect(rows.nth(1).getByRole("checkbox")).toBeChecked();
      await expect(rows.nth(2).getByRole("checkbox")).not.toBeChecked();
      await expect(rows.nth(0).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        /^Select CORE-\d$/
      );

      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(bar).toContainText("2 selected");
      const selectionShot = testInfo.outputPath(`inbox-selection-${testInfo.project.name}.png`);
      await page.screenshot({ fullPage: true, path: selectionShot });
      await testInfo.attach(`inbox selection (${testInfo.project.name})`, {
        contentType: "image/png",
        path: selectionShot,
      });

      // h with a selection focuses the bulk picker; one pick writes every marked ask.
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await page.keyboard.press("h");
      await expect(picker).toBeFocused();
      const writes = Promise.all([snoozeRequest(page, first), snoozeRequest(page, second)]);
      await picker.selectOption("later-today");
      for (const request of await writes) {
        expect(Date.parse(request.postDataJSON().snoozed_until)).toBeGreaterThan(Date.now());
      }

      // Both rows fold into Later, and a snooze that lands takes its rows off the selection.
      await expect(page.getByRole("button", { name: /^Later \(2\)/ })).toBeVisible();
      await expect(page.locator(`[data-inbox-row="${first}"]`)).toHaveCount(0);
      await expect(page.locator(`[data-inbox-row="${second}"]`)).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      // One row marked and no row focused: Escape clears the selection.
      await page.keyboard.press("j");
      await expect(rows.nth(0)).toBeFocused();
      await page.keyboard.press("x");
      await expect(bar).toContainText("1 selected");
      await page.keyboard.press("Escape");
      await expect(rows.nth(0)).not.toBeFocused();
      await expect(bar).toContainText("1 selected");
      await page.keyboard.press("Escape");
      await expect(bar).toHaveCount(0);
      await expect(rows.nth(0).getByRole("checkbox")).not.toBeChecked();

      // The row's own checkbox is the mouse path into the same selection.
      await rows.nth(0).getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");
    } finally {
      await context.close();
    }
  });

  test("a refused bulk snooze names the count and keeps the refused rows marked", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const first = await askIdOf(page, 0);

      await page.route(`**/api/v1/me/asks/${first}/snooze`, (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        })
      );

      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      const bar = page.getByRole("group", { name: "Selected asks" });
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

      await expect(bar).toContainText("Could not snooze 1 of 2.");
      // The refused row keeps its mark and its place; the one that landed left both.
      await expect(bar).toContainText("1 selected");
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", first);
      await expect(rows.nth(0).getByRole("checkbox")).toBeChecked();
      await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();

      // Clear is the pointer's way out of the selection, whatever a pick left marked.
      await bar.getByRole("button", { name: "Clear selection" }).click();
      await expect(bar).toHaveCount(0);
      await expect(rows.nth(0).getByRole("checkbox")).not.toBeChecked();
    } finally {
      await context.close();
    }
  });
});

test.describe("agents page", () => {
  // Both sessions are seen recently and have Dispatch activity of their own, so both are listed
  // rows rather than folded into `Inactive` or `No Dispatch activity`. The Planner's open ask
  // waits on the viewer, which is what puts it above the Reviewer.
  const planner: FakeSession = {
    capabilities: ["aside", "btw"],
    dir: "/srv/planner",
    last_seen: Date.now() - 30_000,
    machine_id: "box-1",
    roles: ["planner"],
    session_id: "planner-session",
    title: "Planner",
  };
  const reviewer: FakeSession = {
    capabilities: ["aside", "btw"],
    dir: "/srv/reviewer",
    last_seen: Date.now() - 30_000,
    machine_id: "box-1",
    roles: ["reviewer"],
    session_id: "reviewer-session",
    title: "Reviewer",
  };

  test("j/k rove the agent rows, Enter opens the composer, i the issue picker, x selects and Shift+P pins", async ({
    browser,
  }, testInfo) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
    await createAsk(
      issue.key,
      { question: "Which release?" },
      { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
    );
    await createComment(
      issue.key,
      { body: "Reviewing the diff." },
      { actor: { id: reviewer.session_id, kind: "session" }, as: "agent" }
    );
    await setLiveSessions([planner, reviewer]);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = page.locator("[data-agent-row]");
      await expect(rows).toHaveCount(2);
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);
      const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });

      await page.keyboard.press("j");
      await expect(planner).toBeFocused();
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "false");

      // Enter expands the collapsed row and hands focus to its composer; letters then type.
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "true");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(reviewer).not.toBeFocused();
      const agentsShot = testInfo.outputPath(`agents-keyboard-${testInfo.project.name}.png`);
      await page.screenshot({ fullPage: true, path: agentsShot });
      await testInfo.attach(`agents keyboard (${testInfo.project.name})`, {
        contentType: "image/png",
        path: agentsShot,
      });

      // Escape out of an untouched composer is one level out: back to the row it belongs to.
      await composer.fill("");
      await page.keyboard.press("Escape");
      await expect(planner).toBeFocused();

      await page.keyboard.press("x");
      await expect(
        planner.getByRole("checkbox", { name: "Select Planner for broadcast" })
      ).toBeChecked();

      // i expands a collapsed row too, and opens its issue picker.
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      const reviewerPicker = reviewer.getByRole("button", { name: "Choose issue" });
      await page.keyboard.press("i");
      await expect(reviewer.getByRole("button", { exact: true, name: "Reviewer" })).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      await expect(reviewerPicker).toHaveAttribute("aria-expanded", "true");
      await expect(reviewer.getByRole("combobox", { name: "Issue" })).toBeVisible();

      // Shift+P pins the focused agent, and a pin is the viewer's own remembered preference.
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await page.keyboard.press("Shift+P");
      await expect(planner.getByRole("button", { name: "Unpin Planner" })).toBeVisible();
      await page.reload();
      await expect(
        page.locator("[data-agent-row]").nth(0).getByRole("button", { name: "Unpin Planner" })
      ).toBeVisible();

      // `?` lists the new scope: a scope missing from `SCOPE_ORDER` is dropped silently.
      await page.locator("body").focus();
      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      const agentsSection = help.getByRole("region", { name: "Agents" });
      await expect(agentsSection).toBeVisible();
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Pin or unpin the focused agent" })
      ).toBeVisible();
    } finally {
      await context.close();
    }
  });
});
