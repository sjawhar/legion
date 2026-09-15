import { expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject, getIssue, listIssues, patchIssue, putIssueState } from "./api";
import { resetDatabase } from "./seed";
import { centerOf, pressFinger, touchDrag, touchHold } from "./touch";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The visible priority tag inside a card or row - the `<select>` options carry the same text. */
function priorityBadge(scope: Locator, label: string): Locator {
  return scope.locator("span", { hasText: new RegExp(`^${label}$`) });
}

function patchOf(page: Page, key: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH" &&
      new URL(response.url()).pathname === `/api/v1/issues/${key}`
  );
}

/**
 * Waits until no card is mid-flight. dnd-kit lands every reorder it did not animate itself - the
 * rollback and refetch after a rejected rank included - with a 200 ms transform transition, and a
 * card later in the DOM paints over the ones it crosses, so a press during that flight lifts
 * whichever card is passing the point rather than the one measured there.
 */
async function boardAtRest(page: Page): Promise<void> {
  await page.getByRole("region", { name: "Project board" }).evaluate(async (board) => {
    for (;;) {
      const running = board.getAnimations({ subtree: true });
      if (running.length === 0) {
        return;
      }
      await Promise.allSettled(running.map((animation) => animation.finished));
    }
  });
}

/** Drags with the mouse from the middle of `source` to `target` (a card or a column). */
async function mouseDrag(page: Page, source: Locator, target: Locator, yOffset?: number) {
  await boardAtRest(page);
  // `hover` presses only once the card holds still and is the element under the pointer; a bare
  // `mouse.move` to a measured box would press on whatever is painted there.
  await source.hover();
  const sourceBox = await source.boundingBox();
  if (sourceBox === null) {
    throw new Error("drag source is not visible");
  }
  await page.mouse.down();
  await page.mouse.move(sourceBox.x + sourceBox.width / 2 + 10, sourceBox.y + sourceBox.height / 2);
  await target.scrollIntoViewIfNeeded();
  const targetBox = await target.boundingBox();
  if (targetBox === null) {
    throw new Error("drag target is not visible");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    yOffset === undefined ? targetBox.y + targetBox.height / 2 : targetBox.y + yOffset,
    { steps: 24 }
  );
  await page.mouse.up();
}

test("the board is the kanban: whole-card drag orders List and Board alike, Icebox and Done collapse behind a toggle", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First card" });
  await patchIssue(first.key, { status: "todo" });
  const second = await createIssue({ project: "CORE", title: "Second card" });
  await patchIssue(second.key, { priority: 0 });
  const third = await createIssue({ project: "CORE", title: "Third card" });
  await patchIssue(third.key, { priority: 3 });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ width: 1920, height: 900 });
  }
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const board = page.getByRole("region", { name: "Project board" });
    const triage = page.getByRole("region", { name: "Triage" });
    await expect(triage.getByRole("article")).toHaveText([/Second card/, /Third card/]);

    // The card itself is the handle: no Reorder button, no status pill on the card.
    await expect(board.getByRole("button", { name: /Reorder/ })).toHaveCount(0);
    const thirdCard = triage.getByRole("article", { name: `${third.key} Third card` });
    await expect(thirdCard).not.toContainText("Triage");
    // Icebox and Done are collapsed rails; the other seven are full columns.
    await expect(board.getByTestId("board-column-header")).toHaveCount(7);
    await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
    await expect(page.getByRole("region", { name: "Icebox (collapsed)" })).toBeVisible();
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toContainText("Done");

    // K1: the P3 card dragged above the P0 card stays first everywhere - rank is the only order.
    const secondCard = triage.getByRole("article", { name: `${second.key} Second card` });
    const reorderPatch = patchOf(page, third.key);
    await mouseDrag(page, thirdCard, secondCard, 4);
    expect((await reorderPatch).status()).toBe(200);
    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
    await expect(priorityBadge(thirdCard, "P3")).toBeVisible();
    await expect(priorityBadge(secondCard, "P0")).toBeVisible();

    await page.reload();
    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
    await page.getByRole("button", { name: "List" }).click();
    await expect(
      page.getByRole("list", { name: "triage issues" }).getByRole("listitem")
    ).toHaveText([/Third card/, /Second card/]);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      third.key,
      second.key,
      first.key,
    ]);

    // K2: a whole-card drag into another column changes the status.
    await page.getByRole("button", { name: "Board" }).click();
    const todoColumn = page.getByRole("region", { name: "Todo" });
    const inProgressColumn = page.getByRole("region", { name: "In progress" });
    const movePatch = patchOf(page, first.key);
    await mouseDrag(
      page,
      todoColumn.getByRole("article", { name: `${first.key} First card` }),
      inProgressColumn
    );
    expect((await movePatch).status()).toBe(200);
    await expect(todoColumn).not.toContainText("First card");
    await expect(inProgressColumn).toContainText("First card");
    await page.reload();
    await expect(inProgressColumn).toContainText("First card");

    if (testInfo.project.name === "chromium") {
      // K3: a Retro card dropped on the collapsed Done rail closes it.
      await patchIssue(third.key, { status: "retro" });
      const retroColumn = page.getByRole("region", { name: "Retro" });
      const retroCard = retroColumn.getByRole("article", { name: `${third.key} Third card` });
      await expect(retroCard).toBeVisible();
      const closePatch = patchOf(page, third.key);
      await mouseDrag(page, retroCard, doneRail);
      const closeResponse = await closePatch;
      expect(closeResponse.status()).toBe(200);
      expect(closeResponse.request().postDataJSON()).toEqual({ status: "done", rank: {} });
      await expect(retroColumn).not.toContainText("Third card");
      await expect(doneRail).toContainText("1");
      await expect.poll(() => getIssue(third.key)).toMatchObject({ status: "done" });
      expect((await getIssue(third.key)).closed_at).not.toBeNull();

      // The toggle expands both edges, persists per login across a reload...
      const showEdges = page.getByRole("button", { name: "Show Icebox & Done" });
      await expect(showEdges).toHaveAttribute("aria-pressed", "false");
      await showEdges.click();
      const hideEdges = page.getByRole("button", { name: "Hide Icebox & Done" });
      await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
      await expect(board.getByTestId("board-column-header")).toHaveCount(9);
      await expect(board.getByTestId("board-column-rail")).toHaveCount(0);
      const doneColumn = page.getByRole("region", { name: "Done" });
      await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);
      await page.reload();
      await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
      await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);

      // ...and dragging a Done card out reopens it.
      const backlogColumn = page.getByRole("region", { name: "Backlog" });
      const reopenPatch = patchOf(page, third.key);
      await mouseDrag(
        page,
        doneColumn.getByRole("article", { name: `${third.key} Third card` }),
        backlogColumn
      );
      expect((await reopenPatch).status()).toBe(200);
      await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
      await expect.poll(() => getIssue(third.key)).toMatchObject({ closed_at: null });
      await page.reload();
      await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);

      await hideEdges.click();
      await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
      await expect(page.getByRole("region", { name: "Done (collapsed)" })).toContainText("0");
    }

    if (testInfo.project.name === "iphone") {
      await expect(page.getByRole("button", { name: "Board" })).toHaveCSS("min-height", "44px");
      await expect(page.getByRole("button", { name: "Show Icebox & Done" })).toHaveCSS(
        "min-height",
        "44px"
      );
    }
  } finally {
    await context.close();
  }
});

test("v toggles List and Board; Shift+J/K rank a focused card through the one move path; o, Enter, p and Escape work the card; Space lifts nothing", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    cards.push(issue);
  }
  const [alpha, bravo, charlie] = cards;
  if (alpha === undefined || bravo === undefined || charlie === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ width: 1920, height: 900 });
    const patches: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patches.push(request.url());
      }
    });
    const viewPreference = () =>
      page.evaluate(() => window.localStorage.getItem("dispatch.project.issue-view:alice"));

    // B5: `v` flips List and Board from the keyboard and persists the choice; on the Documents
    // tab there is no view to flip and it is inert.
    await page.goto("/projects/CORE");
    const listButton = page.getByRole("button", { name: "List" });
    const boardButton = page.getByRole("button", { name: "Board" });
    await expect(listButton).toHaveAttribute("aria-pressed", "true");
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await expect(boardButton).toHaveAttribute("aria-pressed", "true");
    expect(await viewPreference()).toBe("board");
    await page.keyboard.press("v");
    await expect(listButton).toHaveAttribute("aria-pressed", "true");
    expect(await viewPreference()).toBe("list");
    await page.getByRole("tab", { name: "Documents" }).click();
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await page.waitForTimeout(300);
    expect(await viewPreference()).toBe("list");
    await expect(page.getByRole("button", { name: "Board" })).toHaveCount(0);
    await page.getByRole("tab", { name: "Issues" }).click();
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await expect(boardButton).toHaveAttribute("aria-pressed", "true");

    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
    const charlieCard = todo.getByRole("article", { name: `${charlie.key} Charlie` });
    // dnd-kit renders its own unnamed `role=status` region; the board's is the named one.
    const live = page.getByRole("status", { name: "Board announcements" });

    // B2: Shift+J sends the same PATCH a drop would, names the visible neighbours, and the
    // card keeps focus in its new place while the live region says where it went.
    await alphaCard.focus();
    await expect(alphaCard).toBeFocused();
    const movePatch = patchOf(page, alpha.key);
    await page.keyboard.press("Shift+J");
    const moveResponse = await movePatch;
    expect(moveResponse.status()).toBe(200);
    expect(moveResponse.request().postDataJSON()).toEqual({
      rank: { after: bravo.key, before: charlie.key },
    });
    await expect(todo.getByRole("article")).toHaveText([/Bravo/, /Alpha/, /Charlie/]);
    await expect(alphaCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 2 of 3`);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      alpha.key,
      charlie.key,
    ]);

    const backPatch = patchOf(page, alpha.key);
    await page.keyboard.press("Shift+K");
    expect((await backPatch).request().postDataJSON()).toEqual({ rank: { before: bravo.key } });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await expect(alphaCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 1 of 3`);

    // At the column's ends there is nothing to do: no request, no announcement change.
    patches.length = 0;
    await page.keyboard.press("Shift+K");
    await charlieCard.focus();
    await page.keyboard.press("Shift+J");
    await page.waitForTimeout(300);
    expect(patches).toEqual([]);
    await expect(charlieCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 1 of 3`);

    // B4: `o` opens the focused card's issue...
    await alphaCard.focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE$/);
    await expect(alphaCard).toBeVisible();

    // ...and so does Enter on the card itself - once: the title link's own Enter is left to
    // the browser, so the card's binding never doubles a navigation.
    let navigations = 0;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) {
        navigations += 1;
      }
    });
    await alphaCard.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.waitForTimeout(300);
    expect(navigations).toBe(1);
    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE$/);

    // `p` reaches the priority select through the focused card; a letter typed there is
    // typing, not a shortcut; Escape returns to the card, then out of the board.
    await alphaCard.focus();
    await page.keyboard.press("p");
    const select = alphaCard.getByLabel(`Priority of ${alpha.key}`);
    await expect(select).toBeFocused();
    await page.keyboard.press("j");
    await expect(select).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.activeElement === document.body))
      .toBe(true);

    // B5: dnd-kit's keyboard grammar is gone - Space on a focused card lifts nothing.
    patches.length = 0;
    await alphaCard.focus();
    await page.keyboard.press("Space");
    await page.waitForTimeout(300);
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Space");
    await page.waitForTimeout(300);
    await expect(page.locator("article.opacity-50")).toHaveCount(0);
    expect(patches).toEqual([]);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE");
  } finally {
    await context.close();
  }
});

test("a stale board refreshes and says so when the server rejects the rank", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "the stale-rank drag exercises a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    cards.push(issue);
  }
  const [alpha, bravo, charlie] = cards;
  if (alpha === undefined || bravo === undefined || charlie === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // Without the live stream the board does not learn about the API move below.
    await page.route("**/api/v1/events*", (route) => route.abort());
    await page.setViewportSize({ width: 1920, height: 900 });
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);

    await patchIssue(alpha.key, { rank: { after: charlie.key } });
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      charlie.key,
      alpha.key,
    ]);
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);

    // Charlie between Alpha and Bravo names neighbours the server now has in the other order.
    const stalePatch = patchOf(page, charlie.key);
    await mouseDrag(
      page,
      todo.getByRole("article", { name: `${charlie.key} Charlie` }),
      todo.getByRole("article", { name: `${bravo.key} Bravo` }),
      4
    );
    const staleResponse = await stalePatch;
    expect(staleResponse.status()).toBe(400);
    expect(staleResponse.request().postDataJSON()).toEqual({
      rank: { after: alpha.key, before: bravo.key },
    });
    await expect(page.getByRole("alert")).toHaveText(
      "The board changed while you were moving this card - refreshed, try again."
    );
    await expect(todo.getByRole("article")).toHaveText([/Bravo/, /Charlie/, /Alpha/]);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      charlie.key,
      alpha.key,
    ]);

    // Trying again from the refreshed board succeeds and clears the alert.
    const retryPatch = patchOf(page, charlie.key);
    await mouseDrag(
      page,
      todo.getByRole("article", { name: `${charlie.key} Charlie` }),
      todo.getByRole("article", { name: `${bravo.key} Bravo` }),
      4
    );
    expect((await retryPatch).status()).toBe(200);
    await expect(todo.getByRole("article")).toHaveText([/Charlie/, /Bravo/, /Alpha/]);
    await expect(page.getByRole("alert")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("board cards carry the unread dot the project list shows", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const unread = await createIssue({ project: "CORE", title: "Unread card" });
  const read = await createIssue({ project: "CORE", title: "Read card" });
  await putIssueState(read.key, { last_read_seq: (await getIssue(read.key)).last_seq });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/projects/CORE");
    const unreadRow = page.getByRole("listitem", { name: `${unread.key} Unread card` });
    await expect(unreadRow.locator('[title="Unread"]')).toHaveCount(1);
    await page.getByRole("button", { name: "Board" }).click();
    const unreadCard = page.getByRole("article", { name: `${unread.key} Unread card` });
    const readCard = page.getByRole("article", { name: `${read.key} Read card` });
    await expect(unreadCard.locator('[title="Unread"]')).toHaveCount(1);
    await expect(readCard).toBeVisible();
    await expect(readCard.locator('[title="Unread"]')).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("project list rows and board cards set an issue's priority in place", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Prioritized card" });
  await patchIssue(issue.key, { priority: 1 });
  const unset = await createIssue({ project: "CORE", title: "Unprioritized card" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    const listRow = page.getByRole("listitem", { name: `${issue.key} Prioritized card` });
    const listControl = listRow.getByLabel(`Priority of ${issue.key}`);
    await expect(priorityBadge(listRow, "P1")).toBeVisible();
    // The badge stays small; the tap target around it is 44 px on a phone (32 px from md).
    const listBox = await listControl.boundingBox();
    expect(listBox?.height ?? 0).toBeGreaterThanOrEqual(
      testInfo.project.name === "iphone" ? 44 : 32
    );
    const listPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await listControl.selectOption("3");
    expect((await listPatch).postDataJSON()).toEqual({ priority: 3 });
    await expect(priorityBadge(listRow, "P3")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 3 });

    await page.getByRole("button", { name: "Board" }).click();
    const card = page.getByRole("article", { name: `${issue.key} Prioritized card` });
    await expect(priorityBadge(card, "P3")).toBeVisible();
    const cardControl = card.getByLabel(`Priority of ${issue.key}`);
    await cardControl.focus();
    const cardPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await cardControl.selectOption("2");
    expect((await cardPatch).postDataJSON()).toEqual({ priority: 2 });
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    // Using the control neither followed the card's link nor dragged the card.
    expect(new URL(page.url()).pathname).toBe("/projects/CORE");
    await expect(card).not.toHaveClass(/opacity-50/);
    await expect(page.getByRole("region", { name: "Triage" }).getByRole("article")).toHaveText([
      /Prioritized card/,
      /Unprioritized card/,
    ]);

    const unsetCard = page.getByRole("article", { name: `${unset.key} Unprioritized card` });
    await expect(priorityBadge(unsetCard, "Priority")).toBeVisible();
    const unsetPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${unset.key}`
    );
    await unsetCard.getByLabel(`Priority of ${unset.key}`).selectOption("0");
    expect((await unsetPatch).postDataJSON()).toEqual({ priority: 0 });
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();

    // Tab order inside a card: the card itself (the drag activator), its title link, then the
    // priority control, so a keyboard shortcut can focus it too; the screenshot shows the
    // focus ring on the badge.
    await unsetCard.focus();
    await page.keyboard.press("Tab");
    await expect(unsetCard.getByRole("link")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(unsetCard.getByLabel(`Priority of ${unset.key}`)).toBeFocused();
    const shot = testInfo.outputPath(`board-priority-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`board priority (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    await page.reload();
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("board scrolls horizontally inside its own container at 1100px", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the board scroll check exercises a desktop viewport"
  );
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Board scrolls without widening the page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.setViewportSize({ width: 1100, height: 800 });
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const boardScroller = page.getByTestId("board-scroll-container");
    await expect(boardScroller).toBeVisible();
    const dimensions = await boardScroller.evaluate((element) => ({
      clientWidth: element.clientWidth,
      pageWidth: document.documentElement.scrollWidth,
      scrollWidth: element.scrollWidth,
      viewportWidth: window.innerWidth,
    }));
    expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
    expect(dimensions.pageWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
  } finally {
    await context.close();
  }
});

/** Where the scroller sits against each column's or rail's start edge, in scroll-content px. */
async function snapOffsets(scroller: Locator): Promise<{ lefts: number[]; scrollLeft: number }> {
  return scroller.evaluate((element) => {
    const origin = element.getBoundingClientRect().left - element.scrollLeft;
    return {
      lefts: [...element.querySelectorAll("section")].map(
        (section) => section.getBoundingClientRect().left - origin
      ),
      scrollLeft: element.scrollLeft,
    };
  });
}

test("on the phone a finger drives the board: hold lifts, tap opens, swipes scroll and snap", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "touch gestures exercise the phone project");
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo" });
    cards.push(issue);
  }
  const [alpha, bravo] = cards;
  if (alpha === undefined || bravo === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const patches: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "PATCH") {
      patches.push(new URL(request.url()).pathname);
    }
  });
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const scroller = page.getByTestId("board-scroll-container");
    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveCount(7);

    // K5: a horizontal swipe lands on a column or rail edge (snap-x snap-mandatory).
    const start = await snapOffsets(scroller);
    expect(start.scrollLeft).toBe(0);
    const box = await scroller.boundingBox();
    if (box === null) {
      throw new Error("board scroller is not visible");
    }
    const swipeY = box.y + 30;
    await touchDrag(page, { x: box.x + 340, y: swipeY }, { x: box.x + 60, y: swipeY }, 0, 8);
    await expect
      .poll(async () => {
        const { lefts, scrollLeft } = await snapOffsets(scroller);
        return scrollLeft > 100 && lefts.some((left) => Math.abs(left - scrollLeft) <= 2);
      })
      .toBe(true);

    // Bring Todo to the start (its own snap point) for the gestures on its cards.
    await scroller.evaluate((element, index) => {
      const section = element.querySelectorAll("section")[index];
      if (section === undefined) {
        throw new Error("no such column");
      }
      element.scrollLeft =
        section.getBoundingClientRect().left -
        (element.getBoundingClientRect().left - element.scrollLeft);
    }, 3);
    await expect
      .poll(async () => (await snapOffsets(scroller)).scrollLeft)
      .toBe((await snapOffsets(scroller)).lefts[3] ?? Number.NaN);
    const inProgress = page.getByRole("region", { name: "In progress" });

    // A vertical swipe scrolls the page and lifts nothing.
    const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
    const alphaCenter = await centerOf(alphaCard);
    await touchDrag(page, { x: alphaCenter.x, y: 600 }, { x: alphaCenter.x, y: 250 }, 0, 6);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
    await expect(todo.locator("article.opacity-50")).toHaveCount(0);
    expect(patches).toEqual([]);
    await page.evaluate(() => window.scrollTo(0, 0));

    // A tap on the priority badge opens the picker without lifting the card...
    const select = alphaCard.getByLabel(`Priority of ${alpha.key}`);
    await touchHold(page, await centerOf(select), 0);
    await expect(select).toBeFocused();
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    // ...and neither does a long press on it.
    await touchHold(page, await centerOf(select), 300);
    await page.waitForTimeout(200);
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    expect(patches).toEqual([]);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE");

    // A quick tap on the title opens the issue.
    await touchHold(page, await centerOf(alphaCard.getByRole("link")), 0);
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.goBack();
    await expect(todo.getByRole("article")).toHaveCount(7);

    // A long press on the title lifts the card instead of following the link (the accepted
    // trade-off of a whole-card handle); with no movement it drops back where it was.
    const link = alphaCard.getByRole("link");
    const lifted = expect(alphaCard).toHaveClass(/opacity-50/);
    await touchHold(page, await centerOf(link), 400);
    await lifted;
    await page.waitForTimeout(300);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE");
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    expect(patches).toEqual([]);

    // K2: hold to lift, carry the card into the right-hand auto-scroll zone (the board scrolls
    // faster the deeper the finger goes, so stay shallow) until In progress has arrived, step
    // back out of the zone and let go - the card changes column.
    const bravoCard = todo.getByRole("article", { name: `${bravo.key} Bravo` });
    const from = await centerOf(bravoCard);
    const movePatch = patchOf(page, bravo.key);
    const finger = await pressFinger(page, from);
    await page.waitForTimeout(250);
    await expect(bravoCard).toHaveClass(/opacity-50/);
    const scrollerBox = await scroller.boundingBox();
    if (scrollerBox === null) {
      throw new Error("board scroller is not visible");
    }
    const edge = scrollerBox.x + scrollerBox.width;
    await finger.moveTo({ x: edge - scrollerBox.width * 0.2 + 20, y: from.y }, 16);
    await expect
      .poll(async () => (await inProgress.boundingBox())?.x ?? 999, { intervals: [10] })
      .toBeLessThan(150);
    const landing = await inProgress.boundingBox();
    if (landing === null || landing.x < -50) {
      throw new Error(`In progress overshot the touch drop: ${JSON.stringify(landing)}`);
    }
    await finger.moveTo({ x: Math.max(60, landing.x + 60), y: from.y }, 4);
    await page.waitForTimeout(100);
    await finger.lift();
    const moveResponse = await movePatch;
    expect(moveResponse.status()).toBe(200);
    expect(moveResponse.request().postDataJSON()).toEqual({
      status: "in_progress",
      rank: {},
    });
    await expect(inProgress).toContainText("Bravo");
    await expect(todo).not.toContainText("Bravo");
  } finally {
    await context.close();
  }
});
