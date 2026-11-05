import {
  type Browser,
  type BrowserContext,
  expect,
  type Page,
  type Request,
  test,
} from "@playwright/test";

import { CONTROL_LABELS, PRESET_NAMES } from "../web/src/features/issue/page-words";
import { createAsk, createIssue, createProject, getArtifact, getIssue, patchIssue } from "./api";
import { barAction, selectEditorText, selectTextIn } from "./editor";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { requirementBlockId } from "./issue-layout-seed-text";
import {
  activePreset,
  blockNodeId,
  caretInNode,
  childrenSection,
  conversationStateControl,
  drawnRows,
  filterBox,
  focusedNodeId,
  focusInside,
  focusThePage,
  gotoIssue,
  identityBlockBottom,
  issuePage,
  marginSlot,
  nodeBodies,
  nodeMenuButton,
  nodeRow,
  onScreen,
  openMenu,
  pinnedBodies,
  rowBody,
  tree,
  treeActiveDescendant,
} from "./issue-page";
import { conversationState, openPreset } from "./issue-presets";
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
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: title })
  ).toBeVisible();
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

    // A digit sets P0–P3 through the same write the picker uses, and Shift+P pins through the
    // header's own control. Both are optimistic, so each write is waited for rather than raced,
    // and one reload below carries both: a second one would prove nothing the first does not.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await page.keyboard.press("2");
    expect((await patch).postDataJSON()).toEqual({ priority: 2 });
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    await leaveFocus(page);

    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await page.keyboard.press("Shift+P");
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    expect((await pinned).ok()).toBe(true);
    await leaveFocus(page);

    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await leaveFocus(page);

    // l opens the labels popover - the listbox itself, not just its trigger.
    await page.keyboard.press("l");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toHaveCount(0);
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
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Keyboard issue" })
    ).toBeVisible();
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
    await expect(conversationState(page, "words")).toHaveAttribute("aria-selected", "true");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    // Every address the eight tabs answered still resolves (dispatch://INFRA-1196 R19), so a
    // chord opens its address; four of them are a preset's own and the rest open the Tree
    // preset, whose content the address names.
    for (const [key, address] of [
      ["k", "verification"],
      ["p", "plan"],
      ["h", "children"],
      ["a", "artifacts"],
      ["v", "activity"],
      ["s", "spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/${address}$`));
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
    // `t v` is Activity and `t c` the Conversation, so Verification takes its checks' `k`.
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Verification tab" }).locator("kbd")
    ).toHaveText(["t", "k"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Plan tab" }).locator("kbd")
    ).toHaveText(["t", "p"]);
    // Activity is the last tab and takes `t v`, since `t a` is Artifacts and `l` the labels.
    await expect(issueSection).toContainText("Activity tab");
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Activity tab" }).locator("kbd")
    ).toHaveText(["t", "v"]);
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
}) => {
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
}) => {
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
      // Dispatch's verdict: a 5xx would be tried again under the write's key, not refused.
      await route.fulfill({
        body: JSON.stringify({ code: "ISSUE_INPUT", error: "Dispatch refused the priority" }),
        contentType: "application/json",
        status: 400,
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
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Closed issue" });
  await patchIssue(issue.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Closed issue");
    // Reopen is behind the identity block's actions control now (dispatch://INFRA-1196 R11).
    await page.getByRole("button", { name: "Issue actions" }).click();
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
}) => {
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
}) => {
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
      await expect(conversationState(page, "words")).toHaveAttribute("aria-selected", "true");
      await openPreset(page, "Spec");
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
}) => {
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
  }) => {
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
        page
          .getByTestId("issue-header")
          .getByRole("heading", { level: 1, name: "Priority and options" })
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
      // The handler is synchronous, so a write it let through had left before a request the page
      // issues next could be answered: that answer is the barrier, not a wall-clock wait.
      await page.evaluate(() => fetch("/api/v1/auth/whoami").then(({ status }) => status));
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
      await page.evaluate(() => fetch("/api/v1/auth/whoami").then(({ status }) => status));
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

  // The ask card is not the only control a reader's hand rests on. The margin ask composer's
  // `Allow multiple` is an ordinary page checkbox, outside every ask card, and a digit or
  // `Shift+P` straight after ticking it must not write the issue's priority or its pin either -
  // the rule is that these two keys write only from the page itself, not from a control.
  test("a digit on a page checkbox writes no priority, and neither does Shift+P", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: "The quick brown fox",
      title: "Composer and priority",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/issues/${issue.key}/spec`);
      await selectEditorText(page, "brown");
      await barAction(page, "Ask");
      const multiple = page
        .getByRole("form", { name: "Comment composer" })
        .getByLabel("Allow multiple");
      await expect(multiple).toBeVisible();

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

      await multiple.focus();
      await expect(multiple).toBeFocused();
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

/**
 * The tree's own keyboard, which is c-keyboard of dispatch://INFRA-1196: every row of the key
 * table in the spec's Design section, the eight chords the page bound before the redesign, the
 * one Tab stop, the arrows, the focused row clear of the sticky header, `t f`, `$mod+Alt+m`, and
 * the WAI-ARIA tree pattern. Each row reads the page through `e2e/issue-page.ts` and spells no
 * selector of its own. The rows above about the project List and the Board are untouched: they
 * are not about this page.
 */

interface OpenedTree {
  readonly context: BrowserContext;
  readonly page: Page;
  readonly seeded: WholeIssueSeed;
}

/** The whole issue, opened by one signed-in reader with the focus on the page itself, which is
 *  where reading mode is. A key pressed before this returns reaches no handler, because the
 *  keymap binds only once sign-in resolves. It asserts nothing: each row below asserts the key
 *  or the tree property it is about first, and `expectTreeDrawn` after it. */
async function openTree(browser: Browser): Promise<OpenedTree> {
  const seeded = await seedWholeIssue();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });
  await gotoIssue(page, `/issues/${seeded.key}`);
  await focusThePage(page);
  return { context, page, seeded };
}

/** The redesigned page and its tree are on the screen, which every row asserts after its own
 *  subject: a page that answers a key but draws no tree fails here. */
async function expectTreeDrawn(page: Page): Promise<void> {
  expect(
    await issuePage(page).count(),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toBe(1);
  expect(await tree(page).count(), 'the page draws no role="tree" named "Issue tree"').toBe(1);
}

/** The ids of the rows a reader can see, in the order the keys move through them. */
async function visibleIds(page: Page): Promise<string[]> {
  return (await drawnRows(page)).filter((row) => row.visible).map((row) => row.id ?? "<no id>");
}

/** Where the keys have left the reader: the row holding the focus, or the one the tree names with
 *  `aria-activedescendant`. */
async function focusedRow(page: Page): Promise<string | null> {
  return (await focusedNodeId(page)) ?? (await treeActiveDescendant(page));
}

/** The focused row is on screen and clear of the sticky identity block, which R9 requires after
 *  every move and not only at the end. */
async function expectFocusedRowClear(page: Page, after: string): Promise<void> {
  const id = await focusedRow(page);
  expect(id, `${after} left the focus on no row`).not.toBeNull();
  const row = (await drawnRows(page)).find((candidate) => candidate.id === id);
  const box = row?.box ?? null;
  expect(box, `${after} left the focus on a row with no box`).not.toBeNull();
  const header = (await identityBlockBottom(page)) ?? 0;
  expect(
    box?.y ?? -1,
    `after ${after} the focused row sits under the sticky header`
  ).toBeGreaterThanOrEqual(header - 1);
  expect(
    (box?.y ?? 0) + (box?.height ?? 0),
    `after ${after} the focused row is below the viewport`
  ).toBeLessThanOrEqual(900);
}

test("the tree's keys move, fold, zoom and open the focused node", async ({ browser }) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    const order = await visibleIds(page);
    expect(
      order.length,
      "the keys have no rows to move through: the tree draws none"
    ).toBeGreaterThan(3);
    await expectTreeDrawn(page);

    // j, k and the arrows move to the next and previous visible row.
    await page.keyboard.press("Tab");
    expect(await focusedRow(page), "Tab did not put the reader on the first row").toBe(order[0]);
    for (const [index, step] of [
      [1, "j"],
      [2, "ArrowDown"],
      [1, "k"],
      [0, "ArrowUp"],
    ] as const) {
      await page.keyboard.press(step);
      expect(await focusedRow(page), `${step} did not move to row ${index}`).toBe(order[index]);
      await expectFocusedRowClear(page, step);
    }

    // Home and End reach the first and last visible row.
    await page.keyboard.press("End");
    expect(await focusedRow(page), "End did not reach the last visible row").toBe(
      order[order.length - 1]
    );
    await expectFocusedRowClear(page, "End");
    await page.keyboard.press("Home");
    expect(await focusedRow(page), "Home did not reach the first visible row").toBe(order[0]);

    // The right arrow unfolds the focused node and then moves into its first child; the left
    // arrow moves back to the parent and then folds it.
    const folded = (await drawnRows(page)).find((row) => row.expanded === "false");
    expect(
      folded,
      "no branch of the tree is folded, so the arrows have nothing to unfold"
    ).toBeDefined();
    const branch = folded?.id ?? "";
    await nodeRow(page, branch).first().focus();
    await page.keyboard.press("ArrowRight");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the right arrow did not unfold the focused node"
    ).toBe("true");
    expect(
      await focusedRow(page),
      "the right arrow left the focused node before unfolding it"
    ).toBe(branch);
    await page.keyboard.press("ArrowRight");
    expect(
      await focusedRow(page),
      "the second right arrow did not move into the branch's first child"
    ).not.toBe(branch);
    await page.keyboard.press("ArrowLeft");
    expect(await focusedRow(page), "the left arrow did not move to the parent").toBe(branch);
    await page.keyboard.press("ArrowLeft");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the second left arrow did not fold the focused node"
    ).toBe("false");

    // o pins the focused node's body open, and again closes it.
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o pinned no body open").toBe(1);
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o did not close the pinned body").toBe(0);

    // . zooms into the focused node and , comes back out one level.
    const whole = await visibleIds(page);
    await page.keyboard.press(".");
    const zoomed = await visibleIds(page);
    expect(
      zoomed.length,
      "the zoom key drew the same rows, so it zoomed into nothing"
    ).toBeLessThan(whole.length);
    expect(zoomed[0], "the zoom key did not make the focused node the tree's root").toBe(branch);
    await page.keyboard.press(",");
    expect(await visibleIds(page), "the key back out did not restore the level above").toEqual(
      whole
    );

    // q moves to the next open question, whatever its depth.
    await page.keyboard.press("q");
    const question = await focusedRow(page);
    expect(
      (await drawnRows(page)).find((row) => row.id === question)?.type,
      "the next-question key reached a row that is no open question"
    ).toBe("ask");

    // w moves into the decision behind the focused node.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(1)))
      .first()
      .focus();
    await page.keyboard.press("w");
    expect(await focusedRow(page), "w did not move into the decision behind the node").toBe(
      blockNodeId(decisionsSlug, seeded.decisions[0] ?? "")
    );

    // z cycles the whole tree: first lines, first lines with bodies to depth two, everything.
    await focusThePage(page);
    const firstLines = await visibleIds(page);
    await page.keyboard.press("z");
    expect(
      await nodeBodies(page).count(),
      "the second state of the cycle draws no bodies"
    ).toBeGreaterThan(1);
    await page.keyboard.press("z");
    expect(
      (await visibleIds(page)).length,
      "the third state of the cycle draws no more rows than the first"
    ).toBeGreaterThan(firstLines.length);
    await page.keyboard.press("z");
    expect(await visibleIds(page), "the cycle did not come back to the first lines").toEqual(
      firstLines
    );

    // Enter puts the caret in the focused node; Escape leaves the caret for the row, then the
    // tree. This extends the binding the page already has and adds none.
    const second = blockNodeId(specSlug, requirementBlockId(2));
    await nodeRow(page, second).first().focus();
    await page.keyboard.press("Enter");
    expect(await caretInNode(page), "Enter did not put the caret in the focused node").toBe(true);
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not leave the caret").toBe(false);
    expect(await focusedRow(page), "Escape left the row as well as the caret").toBe(second);
    await page.keyboard.press("Escape");
    expect(await focusedRow(page), "the second Escape did not leave the tree").toBeNull();
  } finally {
    await context.close();
  }
});

test("Tab enters and leaves the tree at one stop and every move keeps the row on screen", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    const order = await visibleIds(page);

    // One stop in: Tab reaches the tree, and the next Tab leaves it rather than stepping through
    // its rows, as the WAI-ARIA tree pattern requires.
    await page.keyboard.press("Tab");
    expect(await focusInside(tree(page)), "Tab did not enter the tree").toBe(true);
    await expectTreeDrawn(page);
    await page.keyboard.press("j");
    const inside = await focusedRow(page);
    expect(inside, "j moved to no row inside the tree").toBe(order[1]);
    await page.keyboard.press("Tab");
    expect(
      await focusInside(tree(page)),
      "Tab stepped to another row of the tree instead of leaving it"
    ).toBe(false);
    await page.keyboard.press("Shift+Tab");
    expect(await focusInside(tree(page)), "Shift+Tab did not come back into the tree").toBe(true);
    expect(await focusedRow(page), "Shift+Tab came back to another row than the one left").toBe(
      inside
    );

    // The focused row is inside the viewport and clear of the sticky header after every move,
    // including the ones that scroll the page.
    for (let step = 0; step < Math.min(order.length - 1, 12); step += 1) {
      await page.keyboard.press("j");
      await expectFocusedRowClear(page, `move ${step + 1} with j`);
    }
    await page.keyboard.press("End");
    await expectFocusedRowClear(page, "End");
    await page.keyboard.press("Home");
    await expectFocusedRowClear(page, "Home");
  } finally {
    await context.close();
  }
});

test("every chord the page bound before the redesign brings a target on screen with the focus in it", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    // The preset chords of the key table, which hold five of the eight the page bound before this
    // change: `t d`, `t k`, `t s` and `t p`, with `t t` the table's own addition.
    const chords = [
      { preset: PRESET_NAMES.tree, second: "t" },
      { preset: PRESET_NAMES.decisions, second: "d" },
      { preset: PRESET_NAMES.checks, second: "k" },
      { preset: PRESET_NAMES.spec, second: "s" },
      { preset: PRESET_NAMES.plan, second: "p" },
    ] as const;
    for (const chord of chords) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press(chord.second);
      expect(
        await activePreset(page),
        `t ${chord.second} does not open the ${chord.preset} preset`
      ).toBe(chord.preset);
      await expectTreeDrawn(page);
      expect(
        await onScreen(tree(page)),
        `t ${chord.second} does not bring the preset's content on screen`
      ).toBe(true);
      expect(
        await focusInside(tree(page)),
        `t ${chord.second} does not put the focus in the preset's content`
      ).toBe(true);
    }

    // The conversation in its words state and in its everything state: `t c` and `t v`.
    for (const [second, state] of [
      ["c", "words"],
      ["v", "everything"],
    ] as const) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await page.keyboard.press(second);
      expect(
        await conversationStateControl(page, state).getAttribute("aria-selected"),
        `t ${second} does not open the conversation in its ${state} state`
      ).toBe("true");
      expect(await onScreen(marginSlot(page)), `t ${second} leaves the panel off screen`).toBe(
        true
      );
      expect(
        await focusInside(marginSlot(page)),
        `t ${second} does not put the focus in the panel`
      ).toBe(true);
    }

    // The children section: `t h`.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    expect(await onScreen(childrenSection(page)), "t h leaves the children off screen").toBe(true);
    expect(
      await focusInside(childrenSection(page)),
      "t h does not put the focus in the children section"
    ).toBe(true);

    // The node menu: `t a`, which named the Artifacts tab before this change.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("a");
    expect(await openMenu(page).count(), "t a opens no menu").toBe(1);
    expect(await focusInside(openMenu(page)), "t a does not put the focus in the node menu").toBe(
      true
    );
    expect(
      await nodeMenuButton(page).count(),
      `the menu t a opened is not the one named "${CONTROL_LABELS.other_nodes}" beside the presets`
    ).toBe(1);
  } finally {
    await context.close();
  }
});

test("t f filters inside the preset, $mod+Alt+m comments on a selection, and / stays the global search", async ({
  browser,
}) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    // t f filters inside the preset: the filter takes the focus and narrows the rows.
    const before = await visibleIds(page);
    await page.keyboard.press("t");
    await page.keyboard.press("f");
    expect(await filterBox(page).count(), "t f opens no filter").toBe(1);
    await expectTreeDrawn(page);
    expect(await focusInside(filterBox(page)), "t f does not put the focus in the filter").toBe(
      true
    );
    await page.keyboard.type("Requirement 2");
    await expect.poll(async () => (await visibleIds(page)).length).toBeLessThan(before.length);
    expect(
      (await drawnRows(page))
        .filter((row) => row.visible && !row.context && !row.text.includes("Requirement 2"))
        .map((row) => row.id),
      "the filter kept rows that do not match what was typed"
    ).toEqual([]);

    // `/` is the application's global search, which this page does not shadow.
    await page.keyboard.press("Escape");
    await focusThePage(page);
    await page.keyboard.press("/");
    await expect(page.getByRole("dialog", { name: /search/i })).toBeVisible();
    await page.keyboard.press("Escape");

    // $mod+Alt+m still comments on a selection inside a node's own text.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const requirement = nodeRow(page, blockNodeId(specSlug, requirementBlockId(2))).first();
    await requirement.focus();
    await page.keyboard.press("Enter");
    expect(await caretInNode(page), "Enter did not put the caret in the node").toBe(true);
    await selectTextIn(page, rowBody(requirement), "requirement");
    await page.keyboard.press("ControlOrMeta+Alt+m");
    await expect(page.getByRole("form", { name: "Comment composer" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the tree answers the WAI-ARIA tree pattern's roles and properties", async ({ browser }) => {
  const { context, page } = await openTree(browser);
  try {
    const rows = await drawnRows(page);
    expect(
      rows.length,
      'the tree pattern has nothing to answer for: no role="treeitem" row is drawn'
    ).toBeGreaterThan(0);
    await expectTreeDrawn(page);
    expect(
      rows.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expect(
      rows.filter((row) => !/^[1-9]\d*$/.test(row.level ?? "")).map((row) => row.id),
      "these rows carry no aria-level"
    ).toEqual([]);

    // A row that holds children says whether it is open; a row that holds none says nothing.
    const levels = rows.map((row) => Number(row.level ?? "0"));
    const holdsChildren = rows.map(
      (row, index) => (levels[index + 1] ?? 0) > (levels[index] ?? 0) || row.foldedCount !== null
    );
    expect(
      rows
        .filter((row, index) => holdsChildren[index] === true && row.expanded === null)
        .map((row) => row.id),
      "these rows hold children and carry no aria-expanded"
    ).toEqual([]);
    expect(
      rows
        .filter((row, index) => holdsChildren[index] !== true && row.expanded !== null)
        .map((row) => row.id),
      "these rows hold no children and carry aria-expanded all the same"
    ).toEqual([]);

    // The tree is one tab stop: it roves with one tabbable row, or it names the row it is on.
    const tabbable = rows.filter((row) => row.tabIndex === 0);
    const named = await treeActiveDescendant(page);
    expect(
      `${tabbable.length}/${named === null ? "none" : "named"}`,
      `the tree has ${tabbable.length} tabbable rows and ${named === null ? "no" : "an"} aria-activedescendant, so it is not one stop`
    ).toMatch(/^(1\/none|0\/named|1\/named)$/);
  } finally {
    await context.close();
  }
});
