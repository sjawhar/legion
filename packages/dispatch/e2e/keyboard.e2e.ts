import { expect, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, getIssue, patchIssue } from "./api";
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

test("? lists the registry with unavailable rows greyed, filters live, and Escape returns to the row", async ({
  browser,
}, testInfo) => {
  await seedInbox();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openInbox(page);
    const rows = page.locator("[data-inbox-row]");

    // With no row focused, row-bound shortcuts are registered but unavailable (`when()` is
    // false), so ? lists them greyed.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const answerRow = dialog.getByRole("listitem").filter({ hasText: "Answer the focused ask" });
    await expect(answerRow).toHaveAttribute("data-enabled", "false");
    await expect(answerRow.locator("kbd")).toHaveText(["Enter"]);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    await page.keyboard.press("j");
    await expect(rows.nth(0)).toBeFocused();
    await page.keyboard.press("?");
    await expect(dialog).toBeVisible();
    const filter = dialog.getByRole("searchbox", { name: "Filter shortcuts" });
    await expect(filter).toBeFocused();

    const goToInbox = dialog.getByRole("listitem").filter({ hasText: "Go to Inbox" });
    await expect(goToInbox.locator("kbd")).toHaveText(["g", "i"]);
    await expect(goToInbox).toHaveAttribute("data-enabled", "true");
    // Row-bound shortcuts describe the row that was focused when ? was pressed.
    await expect(answerRow).toHaveAttribute("data-enabled", "true");
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

/** Todo: Alpha, Bravo, Charlie; In progress: Delta; Retro: Echo, Foxtrot - the board seed. */
async function seedBoard() {
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string, status: "todo" | "in_progress" | "retro") => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    return issue;
  };
  return {
    alpha: await seed("Alpha", "todo"),
    bravo: await seed("Bravo", "todo"),
    charlie: await seed("Charlie", "todo"),
    delta: await seed("Delta", "in_progress"),
    echo: await seed("Echo", "retro"),
    foxtrot: await seed("Foxtrot", "retro"),
  };
}

async function openBoard(page: Page): Promise<void> {
  await page.goto("/projects/CORE");
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(3);
  await page.locator("body").focus();
}

function patchOf(page: Page, key: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH" &&
      new URL(response.url()).pathname === `/api/v1/issues/${key}`
  );
}

test("board focus roves with j/k/h/l over cards, empty columns and the Done rail, and ? lists the Board and Project scopes", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  const { alpha, bravo, delta } = await seedBoard();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openBoard(page);
    const card = (issue: { key: string }, title: string) =>
      page.getByRole("article", { name: `${issue.key} ${title}` });
    const alphaCard = card(alpha, "Alpha");
    const deltaCard = card(delta, "Delta");

    // B1: j/k rove within Todo and clamp at its ends.
    await page.keyboard.press("j");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("j");
    await expect(card(bravo, "Bravo")).toBeFocused();
    await page.keyboard.press("k");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("k");
    await expect(alphaCard).toBeFocused();

    // l steps into the next column at the same index, clamped; an empty column takes focus
    // itself, so h/l stay meaningful across it.
    await page.keyboard.press("l");
    await expect(deltaCard).toBeFocused();
    await page.keyboard.press("l");
    await expect(page.getByRole("region", { name: "Testing" })).toBeFocused();
    await page.keyboard.press("h");
    await expect(deltaCard).toBeFocused();
    await page.keyboard.press("h");
    await expect(alphaCard).toBeFocused();

    // l all the way reaches the collapsed Done rail and stays there.
    for (const _ of ["in_progress", "testing", "needs_review", "retro", "done"]) {
      await page.keyboard.press("l");
    }
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toBeFocused();
    await page.keyboard.press("l");
    await expect(doneRail).toBeFocused();

    // Escape leaves the board; with nothing focused k picks the last card of the last
    // non-empty full column - Foxtrot in Retro.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.activeElement === document.body))
      .toBe(true);
    await page.keyboard.press("k");
    const foxtrotCard = page.getByRole("article", { name: /Foxtrot/ });
    await expect(foxtrotCard).toBeFocused();

    // B5: ? groups the board's rows under Board and v under Project; the page's own
    // "Project board" region is outside the dialog.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const boardSection = dialog.getByRole("region", { name: "Board", exact: true });
    await expect(boardSection).toBeVisible();
    await expect(boardSection).toContainText("Move card down");
    await expect(
      boardSection.getByRole("listitem").filter({ hasText: "Move card down" }).locator("kbd")
    ).toHaveText(["Shift+J"]);
    await expect(dialog.getByRole("region", { name: "Project", exact: true })).toContainText(
      "Toggle List / Board"
    );
    await page.screenshot({
      path: testInfo.outputPath(`board-shortcut-help-${testInfo.project.name}.png`),
    });
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(foxtrotCard).toBeFocused();

    // Mounting straight into the Board (the persisted preference) registers `board` before
    // `project`; the help still lists Global, Project, Board.
    await page.reload();
    await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(3);
    await page.locator("body").focus();
    await page.keyboard.press("?");
    await expect(dialog.getByRole("heading", { level: 3 })).toHaveText([
      "Global",
      "Project",
      "Board",
    ]);
  } finally {
    await context.close();
  }
});

test("Shift+L/H move a focused card to the top of the adjacent status through the one move path: into Done closes, out of Done reopens, Triage has no previous", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  const { bravo, delta, echo, foxtrot } = await seedBoard();
  const golf = await createIssue({ project: "CORE", title: "Golf" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1920, height: 900 });
    await openBoard(page);
    const live = page.getByRole("status", { name: "Board announcements" });
    const inProgress = page.getByRole("region", { name: "In progress" });
    const retro = page.getByRole("region", { name: "Retro" });

    // B3: Shift+L lands at the TOP of the next status (Linear's rule) and focus follows the
    // card into its new column.
    const bravoCard = page.getByRole("article", { name: `${bravo.key} Bravo` });
    await bravoCard.focus();
    const forwardPatch = patchOf(page, bravo.key);
    await page.keyboard.press("Shift+L");
    const forward = await forwardPatch;
    expect(forward.status()).toBe(200);
    expect(forward.request().postDataJSON()).toEqual({
      rank: { before: delta.key },
      status: "in_progress",
    });
    await expect(inProgress.getByRole("article")).toHaveText([/Bravo/, /Delta/]);
    await expect(inProgress.getByRole("article", { name: `${bravo.key} Bravo` })).toBeFocused();
    await expect(live).toHaveText(`${bravo.key} → In progress, position 1 of 2`);

    // Into the collapsed Done rail: the same PATCH a drop on the rail sends, the issue closes,
    // and focus lands on the rail since it renders no card.
    const echoCard = page.getByRole("article", { name: `${echo.key} Echo` });
    await echoCard.focus();
    const closePatch = patchOf(page, echo.key);
    await page.keyboard.press("Shift+L");
    expect((await closePatch).request().postDataJSON()).toEqual({ status: "done", rank: {} });
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toContainText("1");
    await expect(doneRail).toBeFocused();
    await expect(live).toHaveText(`${echo.key} → Done, closed`);
    await expect.poll(() => getIssue(echo.key)).toMatchObject({ status: "done" });
    expect((await getIssue(echo.key)).closed_at).not.toBeNull();

    // Out of Done reopens: with the edges shown, Shift+H from the Done column.
    await page.getByRole("button", { name: "Show Icebox & Done" }).click();
    const doneColumn = page.getByRole("region", { name: "Done", exact: true });
    const echoInDone = doneColumn.getByRole("article", { name: `${echo.key} Echo` });
    await expect(echoInDone).toBeVisible();
    await echoInDone.focus();
    const reopenPatch = patchOf(page, echo.key);
    await page.keyboard.press("Shift+H");
    expect((await reopenPatch).request().postDataJSON()).toEqual({
      rank: { before: foxtrot.key },
      status: "retro",
    });
    await expect(retro.getByRole("article")).toHaveText([/Echo/, /Foxtrot/]);
    await expect(retro.getByRole("article", { name: `${echo.key} Echo` })).toBeFocused();
    await expect(live).toHaveText(`${echo.key} → Retro, position 1 of 2`);
    await expect.poll(() => getIssue(echo.key)).toMatchObject({ closed_at: null, status: "retro" });

    // Triage has no previous status: nothing is sent and focus stays put.
    const patches: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patches.push(request.url());
      }
    });
    const golfCard = page.getByRole("article", { name: `${golf.key} Golf` });
    await golfCard.focus();
    await page.keyboard.press("Shift+H");
    await page.waitForTimeout(300);
    expect(patches).toEqual([]);
    await expect(golfCard).toBeFocused();
  } finally {
    await context.close();
  }
});
