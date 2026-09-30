import { expect, type Page, type Request, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createProject, getIssue, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard-inbox" },
  as: "agent" as const,
};

/** The keymap is bound only once sign-in resolves (`AuthGate` renders a skeleton until
 *  `/auth/whoami` answers), so a key pressed before the page renders reaches no handler. */
async function openIssue(page: Page, key: string, title: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expect(page.getByRole("heading", { level: 1, name: title })).toBeVisible();
  await page.locator("body").focus();
}

/** Whether the page itself holds focus, which is where every row below starts from. */
function onBody(page: Page): Promise<boolean> {
  return page.evaluate(() => document.activeElement === document.body);
}

/** Leaves the control a shortcut just focused: a single letter never fires while an `INPUT`,
 *  `TEXTAREA` or `SELECT` holds focus, so each row starts from the page itself. `body.focus()`
 *  would not do it - `body` takes no focus, so the control keeps it - and the page is asserted
 *  back on `body` because that state is what the row after it tests. */
async function leaveFocus(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect.poll(() => onBody(page)).toBe(true);
}

/** Whether a request is a pin or unpin write. The Conversation tab marks the issue read through
 *  the same `PUT /me/issues/<key>/state` (`{ last_read_seq }`), and that write can land at any
 *  moment after the page opens, so a row counting pins counts only the bodies that set `pinned`. */
function isPinWrite(request: Request): boolean {
  return request.method() === "PUT" && "pinned" in (request.postDataJSON() ?? {});
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

    // Shift+P pins through the header's own control. The pin is optimistic, so the reload
    // below waits for the write itself rather than racing the navigation against it.
    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await page.keyboard.press("Shift+P");
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    expect((await pinned).ok()).toBe(true);
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
    // global shortcut on its own (the create dialog), and completing the chord is not it. The
    // chord's second key must follow within the keymap's 1000 ms window, so nothing slow sits
    // between the two keys: the screenshot is taken once the tab is reached.
    await page.keyboard.press("t");
    const indicator = page.getByTestId("chord-indicator");
    await expect(indicator).toHaveText(/t/);
    await page.keyboard.press("c");
    await expect(indicator).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-chord-${testInfo.project.name}.png`),
    });

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

test("j and k cross a collapsed band instead of dead-ending in it, and reach its rows once it opens", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string, status: "backlog" | "todo" | "in_progress" | "done") => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    return issue;
  };
  await seed("Alpha", "backlog");
  await seed("Bravo", "todo");
  await seed("Mike", "in_progress");
  await seed("Zulu", "done");
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/projects/CORE/issues");
    // A closed `details` keeps its content out of the accessibility tree, so these are DOM
    // locators: that a collapsed band's rows ARE in the DOM is the whole point here - rows the
    // page never rendered would satisfy the assertions below for another reason entirely.
    const rowsIn = (band: string) =>
      page.locator(`ul[aria-label="${band} issues"] [data-issue-row]`);
    const alpha = rowsIn("Backlog");
    const bravo = rowsIn("Todo");
    const mike = rowsIn("In progress");
    const zulu = rowsIn("Done");
    for (const row of [alpha, bravo, mike, zulu]) {
      await expect(row).toHaveCount(1);
    }
    // Done is the one band the page renders closed; Todo is closed here by the reader, which
    // puts a collapsed band between two open ones.
    await expect(zulu).toBeHidden();
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(bravo).toBeHidden();
    await leaveFocus(page);

    await page.keyboard.press("j");
    await expect(alpha).toBeFocused();
    await page.keyboard.press("j");
    await expect(mike).toBeFocused();
    await expect(bravo).not.toBeFocused();
    // Nothing visible past Mike: the collapsed Done band is not a destination, so j holds, as
    // it does at the end of the list.
    await page.keyboard.press("j");
    await expect(mike).toBeFocused();
    await page.keyboard.press("k");
    await expect(alpha).toBeFocused();

    // Opening a band puts its rows back in the order, between the two they sit between.
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(bravo).toBeVisible();
    await alpha.focus();
    await page.keyboard.press("j");
    await expect(bravo).toBeFocused();
    await page.getByRole("group", { name: "Done (1)" }).locator("summary").click();
    await expect(zulu).toBeVisible();
    await mike.focus();
    await page.keyboard.press("j");
    await expect(zulu).toBeFocused();
  } finally {
    await context.close();
  }
});

test("a priority the server refuses shows the header's own failure, whether the digit key or the picker sent it", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused priority" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Refused priority");
    const header = page.getByTestId("issue-header");
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      await route.fulfill({
        body: JSON.stringify({ error: { code: "INTERNAL", message: "nope" } }),
        contentType: "application/json",
        status: 500,
      });
    });

    // The digit key and the picker are one write, so a refusal of either is reported once, by
    // the control the reader is looking at, and the badge rolls back.
    await page.keyboard.press("2");
    const failure = header
      .getByRole("alert")
      .filter({ hasText: `Could not update the priority of ${issue.key}.` });
    await expect(failure).toBeVisible();
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });

    // The picker goes through that same write, so its refusal lands in the one failure the
    // digit key already showed instead of raising a second beside it.
    await header.getByRole("combobox", { name: `Priority of ${issue.key}` }).selectOption("3");
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
  } finally {
    await context.close();
  }
});

test("a closed issue greys the shortcuts its header disables, and keeps the ones it does not", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Closed issue" });
  await patchIssue(issue.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Closed issue");
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();

    await page.keyboard.press("?");
    const issueSection = page
      .getByRole("dialog", { name: "Keyboard shortcuts" })
      .getByRole("region", { name: "Issue", exact: true });
    const row = (label: string) => issueSection.getByRole("listitem").filter({ hasText: label });
    // The title heading takes no focus while the issue is closed, so `e` is greyed with the
    // controls the header disables; pinning a closed issue still works, so Shift+P is not.
    await expect(row("Edit the title")).toHaveAttribute("data-enabled", "false");
    await expect(row("Focus the status")).toHaveAttribute("data-enabled", "false");
    await expect(row("Edit labels")).toHaveAttribute("data-enabled", "false");
    await expect(row("Set priority P0–P3")).toHaveAttribute("data-enabled", "false");
    await expect(row("Pin or unpin the issue")).toHaveAttribute("data-enabled", "true");
    await expect(row("Conversation tab")).toHaveAttribute("data-enabled", "true");
  } finally {
    await context.close();
  }
});

test("with every band collapsed j and k do nothing and ? greys them, and opening one restores both", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  for (const [title, status] of [
    ["Alpha", "backlog"],
    ["Bravo", "todo"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
  }
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/projects/CORE/issues");
    const rows = page.locator("[data-issue-row]");
    await expect(rows).toHaveCount(2);
    for (const band of ["Backlog (1)", "Todo (1)"]) {
      await page.getByRole("group", { name: band }).locator("summary").click();
    }
    await expect(rows.first()).toBeHidden();
    await expect(rows.last()).toBeHidden();
    await leaveFocus(page);

    // Nothing the reader can see is a step, so the keys move nothing …
    await page.keyboard.press("j");
    await page.keyboard.press("k");
    expect(await onBody(page)).toBe(true);

    // … and `?` says so, as it does for the issue scope's disabled controls.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const project = dialog.getByRole("region", { name: "Project", exact: true });
    const roving = ["Next issue", "Previous issue"].map((label) =>
      project.getByRole("listitem").filter({ hasText: label }).first()
    );
    for (const row of roving) {
      await expect(row).toHaveAttribute("data-enabled", "false");
    }
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // Opening one band gives them somewhere to go again.
    await page.getByRole("group", { name: "Todo (1)" }).locator("summary").click();
    await expect(rows.last()).toBeVisible();
    await leaveFocus(page);
    await page.keyboard.press("j");
    await expect(rows.last()).toBeFocused();
    await page.keyboard.press("?");
    for (const row of roving) {
      await expect(row).toHaveAttribute("data-enabled", "true");
    }
  } finally {
    await context.close();
  }
});

test("Escape leaves a focused header select, so the next chord is a chord and not type-ahead", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Type-ahead" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Type-ahead");
    // Every PATCH this page sends, so a status the select's own type-ahead picked is caught
    // even though the pill would show it for only as long as the reader stayed.
    const patched: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patched.push(new URL(request.url()).pathname);
      }
    });

    // Three of the status labels begin with `t` (Triage, Todo, Testing), so a `t` that reached
    // the Status select would write one of them instead of starting the tab chord.
    for (const [key, control] of [
      ["s", page.getByRole("combobox", { name: "Status" })],
      ["p", page.getByRole("combobox", { name: `Priority of ${issue.key}` })],
    ] as const) {
      await page.keyboard.press(key);
      await expect(control).toBeFocused();
      await page.keyboard.press("Escape");
      await expect.poll(() => onBody(page)).toBe(true);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press("c");
      await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
        "aria-selected",
        "true"
      );
      await page.getByRole("tab", { name: "Spec" }).click();
      await leaveFocus(page);
    }

    expect(patched).toEqual([]);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        priority: null,
        status: "triage",
      });
  } finally {
    await context.close();
  }
});

test("the Board lists its own roving keys once: the List's are not registered while it shows", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Alpha" });
  await patchIssue(issue.key, { status: "todo" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/projects/CORE/issues");
    await expect(page.locator("[data-issue-row]")).toHaveCount(1);
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(1);
    await leaveFocus(page);

    // The List's rows are gone, so its keys are not the reader's here and `?` must not list
    // them: `o` and `Enter` would otherwise appear twice over, once per scope.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog.getByRole("region", { name: "Board", exact: true })).toBeVisible();
    const project = dialog.getByRole("region", { name: "Project", exact: true });
    for (const label of ["Next issue", "Previous issue", "Open the focused issue"]) {
      await expect(project.getByRole("listitem").filter({ hasText: label })).toHaveCount(0);
    }
    await expect(dialog.getByRole("listitem").filter({ hasText: "Open issue" })).toHaveCount(1);
    await expect(
      project.getByRole("listitem").filter({ hasText: "Toggle List / Board" })
    ).toHaveCount(1);
  } finally {
    await context.close();
  }
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

      // `Shift+P` writes without focusing anything either, so it needs the same guard, and the
      // same two halves prove it: nothing from the card, the pin itself from the page.
      let pinned = 0;
      await page.route(`**/api/v1/me/issues/${issue.key}/state`, (route) => {
        if (isPinWrite(route.request())) pinned += 1;
        return route.fallback();
      });
      await hold.click();
      await expect(hold).toBeFocused();
      await page.keyboard.press("Shift+P");
      await page.waitForTimeout(500);
      expect(pinned).toBe(0);
      await expect(page.getByRole("button", { name: "Pin issue" })).toBeVisible();

      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press("Shift+P");
      await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
      // The header shows the pin optimistically, before its `PUT` leaves the page, so the write
      // is waited for rather than read the moment the button flips.
      await expect.poll(() => pinned).toBe(1);
    } finally {
      await context.close();
    }
  });

  // The ask card is not the only control a reader's hand rests on. `Show activity` is an ordinary
  // page checkbox, outside every card, and a digit or `Shift+P` straight after ticking it must
  // not write the issue's priority or its pin either - the rule is that these two keys write only
  // from the page itself, not from a control.
  test("a digit on a page checkbox writes no priority, and neither does Shift+P", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Activity and priority" });
    await createComment(issue.key, { body: "Looking into it." }, session);
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/issues/${issue.key}/conversation`);
      const activity = page.getByRole("checkbox", { name: "Show activity" });
      await expect(activity).toBeVisible();

      let patched = 0;
      await page.route(`**/api/v1/issues/${issue.key}`, (route) => {
        if (route.request().method() === "PATCH") patched += 1;
        return route.fallback();
      });
      let pinned = 0;
      await page.route(`**/api/v1/me/issues/${issue.key}/state`, (route) => {
        if (isPinWrite(route.request())) pinned += 1;
        return route.fallback();
      });

      await activity.focus();
      await expect(activity).toBeFocused();
      await page.keyboard.press("1");
      await page.keyboard.press("Shift+P");
      await page.waitForTimeout(500);
      expect({ patched, pinned }).toEqual({ patched: 0, pinned: 0 });
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
      await expect(page.getByRole("button", { name: "Pin issue" })).toBeVisible();

      // From the page itself both keys still write, which is what makes the first half a guard.
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press("1");
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 1 });
      await page.keyboard.press("Shift+P");
      await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    } finally {
      await context.close();
    }
  });
});
