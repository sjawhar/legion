import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getInbox,
  getIssue,
  patchIssue,
} from "./api";
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
async function openInbox(page: Page, rows = 3): Promise<void> {
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows);
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
        /^Select CORE-\d: \w+ decision$/
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

      // The reason is the server's own, as the per-row control shows it - a refusal a reader
      // cannot tell from a sign-out is not a report.
      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment");
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

  // Two rows can fail for two reasons; the bar names the first and says there are others rather
  // than flattening a sign-out and a validation refusal into one sentence.
  test("a bulk snooze refused two different ways names the first reason and says so", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const first = await askIdOf(page, 0);
      const second = await askIdOf(page, 1);

      await page.route(`**/api/v1/me/asks/${first}/snooze`, (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        })
      );
      await page.route(`**/api/v1/me/asks/${second}/snooze`, (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "UNAUTHENTICATED", error: "sign in again" }),
          contentType: "application/json",
          status: 401,
        })
      );

      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      const bar = page.getByRole("group", { name: "Selected asks" });
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

      await expect(bar).toContainText(
        "Could not snooze 2 of 2: snoozed_until must be a moment, and 1 other reason."
      );
      await expect(bar).toContainText("2 selected");
    } finally {
      await context.close();
    }
  });

  // In a view that lists only what is waiting on the reader, the optimistic snooze takes every
  // marked row off the list at once, so the bar's own selection empties while its write is still
  // in the air. The refusal that comes back has to survive that.
  test("a refusal reaches the reader even when the optimistic snooze empties the view", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Needs you" });
    const refusedAsk = await createAsk(issue.key, { question: "First decision" }, session);
    await createAsk(issue.key, { question: "Second decision" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      // Only the asks waiting on the reader, from this one agent: a snoozed row leaves this list
      // altogether rather than folding into `Later`.
      await page.goto(`/?agent=${session.actor.id}&section=needs-you`);
      const rows = page.locator("[data-inbox-row]");
      await expect(rows).toHaveCount(2);
      await page.locator("body").focus();

      // Hold one refusal open, so the answer lands well after the optimistic write has emptied
      // the view.
      let refuse: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        refuse = resolve;
      });
      await page.route(`**/api/v1/me/asks/${refusedAsk.id}/snooze`, async (route) => {
        await held;
        return route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: "snoozed_until must be a moment" }),
          contentType: "application/json",
          status: 400,
        });
      });

      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      // Let go of the row: one under the reader's hand is held on screen whatever the list does,
      // and this test is about the write, not that mechanism.
      await page.keyboard.press("Escape");
      await page.mouse.move(0, 0);
      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(bar).toContainText("2 selected");
      await bar.getByRole("combobox", { name: "Snooze selected asks" }).selectOption("tomorrow");

      // Both rows go at once; the bar stays while its write is in the air.
      await expect(rows).toHaveCount(0);
      await expect(bar).toHaveCount(1);
      refuse?.();

      await expect(bar).toContainText("Could not snooze 1 of 2: snoozed_until must be a moment.");
      await expect(bar).toContainText("1 selected");
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", refusedAsk.id);
    } finally {
      await context.close();
    }
  });

  // Ticking a box with the pointer leaves focus on the `<input>`, and the registry's editable
  // policy decides whether the next key is a shortcut or typing. A checkbox takes no text.
  test("keys still work with a pointer-ticked checkbox holding focus", async ({ browser }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      await rows.nth(0).getByRole("checkbox").check();
      await rows.nth(1).getByRole("checkbox").check();
      await expect(rows.nth(1).getByRole("checkbox")).toBeFocused();
      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(bar).toContainText("2 selected");

      // j roves off the ticked row, and h reaches the bulk picker.
      await page.keyboard.press("j");
      await expect(rows.nth(2)).toBeFocused();
      await page.keyboard.press("h");
      await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // Escape is always one level out. The per-row picker honours it through `back`; the bulk picker
  // has no row ancestor, so it needs its own way back to the list.
  test("Escape leaves the bulk picker for the row it was opened from, keeping the marks", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");

      // Control: a row's own picker, which already returns to its row.
      await page.keyboard.press("j");
      await expect(rows.nth(0)).toBeFocused();
      await page.keyboard.press("h");
      await expect(rows.nth(0).locator("[data-inbox-snooze]")).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(rows.nth(0)).toBeFocused();

      // The bulk picker does the same, and leaves the selection alone on the way.
      await page.keyboard.press("j");
      await expect(rows.nth(1)).toBeFocused();
      await page.keyboard.press("x");
      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(bar).toContainText("1 selected");
      await page.keyboard.press("h");
      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await expect(picker).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(rows.nth(1)).toBeFocused();
      await expect(bar).toContainText("1 selected");

      // And from the row, Escape is the two levels it always was: off the row, then the marks.
      await page.keyboard.press("Escape");
      await page.keyboard.press("Escape");
      await expect(bar).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // Two asks on one issue are two different questions, and a control named after the issue alone
  // says the same thing twice.
  test("each row's controls are named after its own ask", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const twins = await createIssue({ project: "CORE", title: "Two asks on one issue" });
    // Two questions that agree far past any cut: only the position tells these two apart.
    await createAsk(
      twins.key,
      { question: "Which of the two long-running migration strategies should we adopt: A?" },
      session
    );
    await createAsk(
      twins.key,
      { question: "Which of the two long-running migration strategies should we adopt: B?" },
      session
    );
    const alone = await createIssue({ project: "CORE", title: "Release checklist" });
    await createAsk(alone.key, { question: "Ship it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 3);
      const rows = page.locator("[data-inbox-row]");
      const named = async (attribute: string) =>
        rows.evaluateAll(
          (items, selector: string) =>
            items.map((item) => item.querySelector(selector)?.getAttribute("aria-label") ?? "none"),
          attribute
        );

      // An issue with one listed ask keeps the short name; the twins carry their position.
      const checkboxes = await named("input[type=checkbox]");
      expect(checkboxes).toContain("Select CORE-2: Ship it?");
      expect(checkboxes.filter((name) => name.startsWith("Select CORE-1")).sort()).toEqual([
        "Select CORE-1, ask 1 of 2: Which of the two long-running migration strategi…",
        "Select CORE-1, ask 2 of 2: Which of the two long-running migration strategi…",
      ]);
      expect(new Set(checkboxes).size).toBe(checkboxes.length);

      // Snooze is named from the same place, so the two controls of a row agree.
      const pickers = await named("[data-inbox-snooze]");
      expect(pickers).toContain("Snooze CORE-2: Ship it?");
      expect(new Set(pickers).size).toBe(pickers.length);
      expect(pickers.map((name) => name.replace(/^Snooze /, "")).sort()).toEqual(
        checkboxes.map((name) => name.replace(/^Select /, "")).sort()
      );
    } finally {
      await context.close();
    }
  });

  // The bar speaks for the rows this view lists. A mark made under Everyone must not follow the
  // reader into Mine, where its row is not on screen and a bulk snooze would write unseen.
  test("a mark made in another view neither counts nor snoozes once its row is out of the list", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const mine = await createIssue({ project: "CORE", title: "Mine to answer" });
    await patchIssue(mine.key, { assignee: "alice" });
    await createAsk(mine.key, { question: "Ship it?" }, session);
    const theirs = await createIssue({ project: "CORE", title: "Bob answers this" });
    await patchIssue(theirs.key, { assignee: "bob" });
    const hidden = await createAsk(theirs.key, { question: "Hold it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.goto("/?view=everyone");
      const rows = page.locator("[data-inbox-row]");
      await expect(rows).toHaveCount(2);
      await page.locator("body").focus();
      const bar = page.getByRole("group", { name: "Selected asks" });

      const hiddenRow = page.locator(`[data-inbox-row="${hidden.id}"]`);
      await hiddenRow.getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");

      let wrote = false;
      await page.route(`**/api/v1/me/asks/${hidden.id}/snooze`, (route) => {
        wrote = true;
        return route.fallback();
      });
      await page.getByRole("button", { name: "Mine" }).click();
      await expect(rows).toHaveCount(1);
      await expect(hiddenRow).toHaveCount(0);
      await expect(bar).toHaveCount(0);

      // Nothing is marked here, so `h` reaches the focused row's own picker, not a bulk write.
      await page.keyboard.press("j");
      await page.keyboard.press("h");
      await expect(rows.nth(0).locator("[data-inbox-snooze]")).toBeFocused();
      expect(wrote).toBe(false);
    } finally {
      await context.close();
    }
  });

  // The `?agent=` chip narrows the list the same way the view switch does, and the Inbox stays
  // mounted across it: the chip's clear link and the browser's own Back are client-side moves.
  test("a mark made outside the agent filter does not count inside it", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Two agents, one issue" });
    await patchIssue(issue.key, { assignee: "alice" });
    const other = {
      actor: { id: "e2e-other-agent", kind: "session" as const },
      as: "agent" as const,
    };
    const fromOther = await createAsk(issue.key, { question: "From another agent?" }, other);
    const fromSession = await createAsk(issue.key, { question: "Ship it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.goto(`/?agent=${other.actor.id}`);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected asks" });
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", fromOther.id);

      await page.getByRole("link", { name: "Clear agent filter" }).click();
      await expect(rows).toHaveCount(2);
      await page.locator(`[data-inbox-row="${fromSession.id}"]`).getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");

      // Let go of the row first: a row under the reader's hand is held on screen even when the
      // list stops carrying it, which is the Inbox's own rule and not what this test is about.
      await page.mouse.move(0, 0);
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

      // Back into the filter: the marked ask is not one of its rows, so nothing counts here.
      await page.goBack();
      await expect(rows).toHaveCount(1);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", fromOther.id);
      await expect(bar).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // Escape is one level out even when the level below has nothing rendered in it.
  test("Escape leaves the bulk picker when the marked row is folded away", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "The only ask" });
    await createAsk(issue.key, { question: "Ship it?" }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 1);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected asks" });

      // Mark it, then snooze it from its own row control: it folds into Later, still marked.
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await expect(bar).toContainText("1 selected");
      await rows.nth(0).locator("[data-inbox-snooze]").selectOption("tomorrow");
      await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
      await expect(rows).toHaveCount(0);
      await expect(bar).toContainText("1 selected");

      const picker = bar.getByRole("combobox", { name: "Snooze selected asks" });
      await page.keyboard.press("h");
      await expect(picker).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(picker).not.toBeFocused();
      await expect(bar).toContainText("1 selected");
    } finally {
      await context.close();
    }
  });

  // Reaching the picker with the pointer leaves no `h` behind it, so Escape must not send the
  // reader to whatever row an earlier `h` happened to be on.
  test("Escape from a pointer-opened bulk picker goes to the list, not a stale row", async ({
    browser,
  }) => {
    await seedInbox();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page);
      const rows = page.locator("[data-inbox-row]");
      const bar = page.getByRole("group", { name: "Selected asks" });

      // An `h` from row 1 records that row, then the reader leaves and marks row 3 instead.
      await page.keyboard.press("j");
      await page.keyboard.press("x");
      await page.keyboard.press("h");
      await page.keyboard.press("Escape");
      await expect(rows.nth(0)).toBeFocused();
      await page.keyboard.press("x");
      await rows.nth(2).getByRole("checkbox").check();
      await expect(bar).toContainText("1 selected");

      await bar.getByRole("combobox", { name: "Snooze selected asks" }).focus();
      await page.keyboard.press("Escape");
      await expect(rows.nth(2)).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The ordinal is only usable if it counts the way the bands read: top to bottom. The server
  // orders by whose turn it is and how recent the activity is; `Later` is the SPA's own band and
  // sits last whatever the server said, so a snoozed ask is where the two orders come apart.
  test("ask positions follow the order the bands show them, not the server's", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Two asks, two bands" });
    const deferred = await createAsk(issue.key, { question: "Alpha decision" }, session);
    const handedBack = await createAsk(issue.key, { question: "Beta decision" }, session);
    // A human reply hands Beta's turn to the agents, so the server lists Alpha - still the
    // reader's turn - first.
    await createComment(issue.key, { ask_id: handedBack.id, body: "Looking into it." });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openInbox(page, 2);
      const rows = page.locator("[data-inbox-row]");

      // Defer Alpha: it drops into `Later`, which renders below every turn band.
      await page
        .locator(`[data-inbox-row="${deferred.id}"] [data-inbox-snooze]`)
        .selectOption("tomorrow");
      await page.getByRole("button", { name: /^Later \(1\)/ }).click();
      await expect(rows).toHaveCount(2);

      // The precondition this test exists for: the server's order is the opposite of the bands'.
      const served = (await getInbox({ login: "alice" })).map((row) => row.id);
      expect(served).toEqual([deferred.id, handedBack.id]);
      await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", handedBack.id);
      await expect(rows.nth(1)).toHaveAttribute("data-inbox-row", deferred.id);

      await expect(rows.nth(0).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        "Select CORE-1, ask 1 of 2: Beta decision"
      );
      await expect(rows.nth(1).getByRole("checkbox")).toHaveAttribute(
        "aria-label",
        "Select CORE-1, ask 2 of 2: Alpha decision"
      );
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

  /** Both sessions live, both with Dispatch activity, and one issue for the picker to offer. */
  async function seedAgents(): Promise<string> {
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
    return issue.key;
  }

  test("j/k rove the agent rows, Enter opens the composer, i the issue picker, x selects and Shift+P pins", async ({
    browser,
  }, testInfo) => {
    await seedAgents();
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
      // Escape out of a row's composer is real behaviour, so `?` says so: the Inbox, Board and
      // Architecture all list theirs, and a help screen that omits one is a help screen a reader
      // cannot trust.
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Back to the agent row" })
      ).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // The composer calls `onClose` after a successful send as well as on the way out, and only the
  // way out is "one level up". Focus on the row after a send would turn the next letters typed
  // into agent shortcuts.
  test("a sent message leaves focus in the composer, not on the row", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      await page.keyboard.press("j");
      await expect(row).toBeFocused();
      await page.keyboard.press("Enter");
      const composer = row.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();

      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      // The composer clears only once the server has taken the message.
      await expect(composer).toHaveValue("");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(row).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The disable takes focus to the document; only that focus is the composer's to take back. A
  // reader who moved on during the round trip keeps where they went.
  test("a send in flight does not steal back focus the reader has moved", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = page.locator("[data-agent-row]");
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);

      // Hold the send open so the reader has a window to click in, as a slow server would.
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/agents/*/messages", async (route) => {
        if (route.request().method() !== "POST") return route.fallback();
        await held;
        return route.fallback();
      });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");

      // Mid-send the reader ticks the other agent's box; that focus is theirs to keep.
      const reviewerBox = reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" });
      await reviewerBox.focus();
      await expect(reviewerBox).toBeFocused();
      release?.();
      await expect(composer).toHaveValue("");
      await expect(reviewerBox).toBeFocused();
      await expect(composer).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // `i` clicks a button that only renders while the row is not replying to a message, so it is
  // advertised only when that button is reachable - the rule this slice already applied to the
  // Inbox's own snooze.
  test("i is listed as unavailable while the row is answering a message", async ({ browser }) => {
    const issueKey = await seedAgents();
    const root = await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${planner.session_id}`,
    });
    expect(root.id).toBeTruthy();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = page.locator("[data-agent-row]").nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(row.getByRole("textbox", { name: "Comment" })).toBeFocused();
      await expect(row.getByRole("button", { name: "Choose issue" })).toBeVisible();

      await row.getByRole("button", { name: "Reply" }).first().click();
      await expect(row.getByRole("button", { name: "Choose issue" })).toHaveCount(0);
      await row.focus();

      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      await expect(
        help.getByRole("listitem").filter({ hasText: "Pick an issue for the message" })
      ).toHaveAttribute("data-enabled", "false");
    } finally {
      await context.close();
    }
  });
});

// The issue page's `0`-`3` write the issue's priority, and an ask card on that page carries option
// radios. A radio takes no typed text, so this slice's editable policy lets a digit through to the
// page - which must not turn "pick option 2" into "this issue is P2". The second half of this row
// is what makes the first mean anything: the same key, off the card, still writes.
test.describe("issue page", () => {
  test("a digit on an ask's option writes no priority, while the same digit on the page does", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Priority and options" });
    await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which way?" },
      session
    );
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page.getByRole("heading", { level: 1, name: "Priority and options" })
      ).toBeVisible();

      let patched = 0;
      await page.route(`**/api/v1/issues/${issue.key}`, (route) => {
        if (route.request().method() === "PATCH") patched += 1;
        return route.fallback();
      });

      // Focus an option radio, as a reader answering the ask would, and press a digit.
      const hold = page.getByRole("radio", { name: "Hold" });
      await hold.click();
      await expect(hold).toBeFocused();
      await page.keyboard.press("2");
      await page.waitForTimeout(500);
      expect(patched).toBe(0);
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });

      // Off the card, the same key is the header's own: this is what proves the guard is a guard
      // and not a key that stopped working.
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      const patch = page.waitForRequest(
        (request) =>
          request.method() === "PATCH" &&
          new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      );
      await page.keyboard.press("2");
      expect((await patch).postDataJSON()).toEqual({ priority: 2 });
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    } finally {
      await context.close();
    }
  });
});
