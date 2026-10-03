import { expect, type Page, test } from "@playwright/test";

import { openAgents, seedAgents } from "./agents";
import {
  createAsk,
  createIssue,
  createProject,
  getIssue,
  patchIssue,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The keymap is bound only once sign-in resolves (`AuthGate` renders a skeleton until
// `/auth/whoami` answers), so a key pressed before the page renders reaches no handler.
async function openIssue(page: Page, issueKey: string, title: string): Promise<void> {
  await page.goto(`/issues/${issueKey}`);
  await expect(page.getByRole("heading", { level: 1, name: title })).toBeVisible();
  await page.locator("body").focus();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the palette lists the issue page's actions, guarded like their buttons, and runs one", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Palette actions" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Palette actions");
    await page.keyboard.press("Control+k");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    await expect(actions).toBeVisible();
    // An empty query still lists the actions, above the message the hits would answer.
    await expect(page.getByText("Type at least 2 characters")).toBeVisible();

    const close = actions.getByRole("option", { name: "Close issue" });
    await expect(close).toBeVisible();
    // A palette-only action has no key, so it offers no hint; a keyed one shows its key.
    await expect(close.locator("kbd")).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Create issue" }).locator("kbd")).toHaveText([
      "c",
    ]);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Reopen issue" })).toHaveCount(0);
    // An issue page names its project in its key, so `g d` has a row here.
    await expect(actions.getByRole("option", { name: "Go to Documents" })).toHaveCount(1);
    // `$mod+k` fires inside an input, as the palette's own close, so it is no row, and `/` would
    // only reopen this palette with its actions taken away. A binding that opens anything else is
    // a row: it runs once this palette has closed (the rows below).
    for (const absent of ["Search and actions", "Search only"]) {
      await expect(actions.getByRole("option", { exact: true, name: absent })).toHaveCount(0);
    }

    // Arrows move over action rows exactly as they move over hits, from the first row. Which row
    // that is belongs to `actions()`'s sort, so this names none.
    const rows = actions.getByRole("option");
    const firstId = await rows.first().getAttribute("id");
    expect(firstId).not.toBeNull();
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowDown");
    await expect(input).not.toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowUp");
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);

    // Arrow down to Close issue, wherever the sort put it, and Enter runs the highlighted row.
    const closeId = await close.getAttribute("id");
    const ids = await rows.evaluateAll((options) => options.map((option) => option.id));
    const closeIndex = ids.indexOf(closeId ?? "");
    expect(closeIndex).toBeGreaterThanOrEqual(0);
    for (let step = 0; step < closeIndex; step += 1) {
      await input.press("ArrowDown");
    }
    await expect(input).toHaveAttribute("aria-activedescendant", closeId as string);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect.poll(() => getIssue(issue.key).then((read) => read.status)).toBe("done");

    // The guards follow the buttons: the closed issue offers Reopen and neither Close nor a
    // priority, exactly as its header renders Reopen and disables the priority select.
    await page.reload();
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(actions.getByRole("option", { name: "Reopen issue" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toHaveCount(0);

    // Named, not positional: which row is highlighted first is `actions()`'s decision, and this
    // step is about Reopen running, not about where Reopen sits.
    await actions.getByRole("option", { name: "Reopen issue" }).click();
    await expect(dialog).toHaveCount(0);
    await expect.poll(() => getIssue(issue.key).then((read) => read.status)).toBe("backlog");

    // A filtered action runs the same write its control does. The rows are taken when the palette
    // opens, and the Set priority rows, like the priority select, are withheld until the page has
    // taken in the Reopen answer, which under load can come after the server already reads
    // `backlog`: so open once the select takes a pick again.
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toBeEnabled();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("priority p2");
    const setP2 = actions.getByRole("option", { name: "Set priority P2" });
    await expect(actions.getByRole("option")).toHaveCount(1);
    // `priority p2` sits inside the label rather than starting it, so the row is offered but not
    // highlighted: Enter alone runs nothing, even once the search has answered with no hits.
    await expect(dialog.getByText('No results for "priority p2"')).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await input.press("ArrowDown");
    await expect(input).toHaveAttribute(
      "aria-activedescendant",
      (await setP2.getAttribute("id")) as string
    );
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toHaveValue("2");
    await expect.poll(() => getIssue(issue.key).then((read) => read.priority)).toBe(2);
  } finally {
    await context.close();
  }
});

test("stop-word searches show empty results without Retry", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stop word search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Stop word search");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });

    for (const query of ["down", "up", "is", "the"]) {
      await page.keyboard.press("Control+k");
      await input.fill(query);
      await expect(dialog.getByText(`No results for "${query}"`)).toBeVisible();
      await expect(dialog.getByRole("alert")).toHaveCount(0);
      await expect(dialog.getByRole("button", { name: "Retry" })).toHaveCount(0);
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});

test("a failed palette search keeps its Retry action", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Failed palette search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    let searches = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        searches += 1;
        await route.fulfill({
          body: JSON.stringify({ code: "INTERNAL", error: "search index unavailable" }),
          contentType: "application/json",
          status: 500,
        });
      }
    );
    await openIssue(page, issue.key, "Failed palette search");
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("astrolabe");

    const failure = dialog.getByRole("alert");
    await expect(failure).toHaveText(/Search failed\./);
    const retry = failure.getByRole("button", { name: "Retry" });
    await expect(retry).toBeVisible();
    const searchesBeforeRetry = searches;
    await retry.click();
    await expect.poll(() => searches).toBeGreaterThan(searchesBeforeRetry);
  } finally {
    await context.close();
  }
});

test("/ searches only, and a query that is no action's start lists the hits above the actions", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Astrolabe issue calibration" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Astrolabe issue calibration");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });

    await page.keyboard.press("/");
    await expect(dialog).toBeVisible();
    await expect(actions).toHaveCount(0);
    await input.fill("astrolabe");
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await expect(actions).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // `/` reopens on its last search, selected so that typing replaces it; the hits come back
    // with it. `⌘K` still opens empty, since a query left there would filter this page's actions.
    await page.keyboard.press("/");
    await expect(input).toHaveValue("astrolabe");
    await expect
      .poll(() =>
        input.evaluate((field: HTMLInputElement) => [field.selectionStart, field.selectionEnd])
      )
      .toEqual([0, "astrolabe".length]);
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // The same query under `$mod+k` lists the hits first. No action here starts with `issue`, so
    // the ones that hold it further in (`Close issue` among them) sit below the hits, and Enter
    // opens the first hit rather than closing the issue the reader is on.
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    await page.keyboard.press("Control+k");
    await expect(input).toHaveValue("");
    await input.fill("issue");
    const options = dialog.getByRole("option");
    await expect(options.first()).toHaveAttribute("id", /^search-option-issue-/);
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    await expect(options.last()).toHaveAttribute("id", /^search-option-action-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(
      page.getByRole("heading", { level: 1, name: "Astrolabe issue calibration" })
    ).toBeVisible();
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).not.toBe("done");
  } finally {
    await context.close();
  }
});

test("g d opens the route's project Documents, from an issue key or a project path, and g p picks a project", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const issue = await createIssue({ project: "CORE", title: "Documents from an issue" });
  const context = await asUser(browser, "alice");

  try {
    // One page load; every later step is an in-app navigation, which is what the keys do.
    const page = await context.newPage();
    await openIssue(page, issue.key, "Documents from an issue");

    // An issue page names its project only in its key.
    await page.keyboard.press("g");
    await page.keyboard.press("d");
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);

    await page.locator("body").focus();
    await page.keyboard.press("g");
    await page.keyboard.press("p");
    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(dialog.getByRole("group", { name: "Projects" })).toBeVisible();
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("operations");
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await input.press("Enter");
    await expect(page).toHaveURL(/\/projects\/OPS\/issues/);

    // A project path names it outright. The heading is the page the key reads its route from.
    await expect(page.getByRole("heading", { level: 1, name: "Operations" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("g");
    await page.keyboard.press("d");
    await expect(page).toHaveURL(/\/projects\/OPS\/documents$/);
  } finally {
    await context.close();
  }
});

test("Shift+S and Shift+M toggle the sidebar and the margin, and the choice survives a reload", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "the rails are a desktop layout");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Panel toggles" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await openIssue(page, issue.key, "Panel toggles");
    await expect(page.getByTestId("sidebar-rail")).toHaveCount(0);
    await expect(page.getByTestId("margin-rail")).toHaveCount(0);

    await page.keyboard.press("Shift+S");
    await expect(page.getByTestId("sidebar-rail")).toBeVisible();
    await page.keyboard.press("Shift+M");
    await expect(page.getByTestId("margin-rail")).toBeVisible();

    await page.reload();
    await expect(page.getByTestId("sidebar-rail")).toBeVisible();
    await expect(page.getByTestId("margin-rail")).toBeVisible();

    await page.locator("body").focus();
    await page.keyboard.press("Shift+S");
    await expect(page.getByTestId("sidebar-rail")).toHaveCount(0);
    await page.keyboard.press("Shift+M");
    await expect(page.getByTestId("margin-rail")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("the rail's Search control shows the same actions as the keyboard palette", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "the rail Search control is a desktop layout");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rail search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Rail search");
    await page.getByRole("button", { name: /^search/i }).click();

    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Go to Inbox" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a focus-dependent row is offered, survives filtering, and runs on the row that opened the palette", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inbox palette" });
  await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: { id: "e2e-palette", kind: "session" }, as: "agent" }
  );
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(1);
    await page.locator("body").focus();
    // Every other inbox binding is gated on a focused row, so `j` first: with one focused, the
    // movement rows are still absent while the row's own action is offered.
    await page.keyboard.press("j");
    await expect(page.locator("[data-inbox-row]").first()).toBeFocused();
    await page.keyboard.press("Control+k");

    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(
      actions.getByRole("option", { name: "Open the ask's issue or document" })
    ).toHaveCount(1);
    await expect(actions.getByRole("option", { name: "Next ask" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Previous ask" })).toHaveCount(0);

    // Filtering re-renders the palette; the list is the one the opener's focus produced, so a
    // row a reader can see must still be there once they have typed enough to reach it.
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("open the ask");
    const open = actions.getByRole("option", { name: "Open the ask's issue or document" });
    await expect(open).toHaveCount(1);

    // And it runs against that row, not against the palette input that has focus while it is
    // shown: `o` on the row goes to the ask, so the palette row must go to the same place.
    await input.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/asks/`));
  } finally {
    await context.close();
  }
});

test("a board card's row runs on the card that opened the palette", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Board palette" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto("/projects/CORE/issues");
    await page.getByRole("button", { name: "Board" }).click();
    const card = page.getByRole("article", { name: `${issue.key} Board palette` });
    await expect(card).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(card).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    // `Enter` on the card opens it too, and `Open issue` is already that action's row. Soft, so
    // one run names every row that should not be there.
    for (const absent of [
      "Next card",
      "Previous card",
      "Next column",
      "Previous column",
      "Open the focused card's issue",
    ]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await actions.getByRole("option", { name: "Open issue" }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}`));
  } finally {
    await context.close();
  }
});

test("a query that holds a card move further in moves no card on Enter", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  // Two cards in one column, so `Move card down` from the first one has somewhere to go.
  const first = await createIssue({ project: "CORE", title: "Board query one" });
  const second = await createIssue({ project: "CORE", title: "Board query two" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const writes: string[] = [];
    page.on("request", (request) => {
      if (request.method() !== "GET" && new URL(request.url()).pathname.startsWith("/api/")) {
        writes.push(`${request.method()} ${new URL(request.url()).pathname}`);
      }
    });
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto("/projects/CORE/issues");
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.locator(`[data-board-card="${first.key}"]`)).toBeVisible();
    await expect(page.locator(`[data-board-card="${second.key}"]`)).toBeVisible();
    // `j` from the page takes the top card, the one `Move card down` would move.
    const card = page.locator("[data-board-card]").first();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(card).toBeFocused();

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    // Each query sits inside a move's label without starting it: the move is offered, and only
    // an arrow can highlight it, so Enter, even once the search has answered, moves nothing.
    // `down` has no searchable term, so it returns no hits, and the move stays unhighlighted
    // through that answer too.
    for (const [query, move] of [
      ["status", "Move card to the next status"],
      ["down", "Move card down"],
    ] as const) {
      await page.keyboard.press("Control+k");
      await input.fill(query);
      await expect(actions.getByRole("option", { name: move })).toHaveCount(1);
      await expect(dialog.getByText("Searching…")).toHaveCount(0);
      await expect(input).not.toHaveAttribute("aria-activedescendant");
      await input.press("Enter");
      await expect(dialog).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
      await expect(card).toBeFocused();
    }

    // Nor can an arrow pressed while the search is still out: `next` leads no label, and Down
    // then would have taken the move to the next status before any answer came.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await page.keyboard.press("Control+k");
    await input.fill("next");
    const nextMove = actions.getByRole("option", { name: "Move card to the next status" });
    await expect(nextMove).toHaveCount(1);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    expect(writes).toEqual([]);
    await expect.poll(() => held).toBeGreaterThan(0);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    release.resolve();
    await expect(dialog.getByText("Searching…")).toHaveCount(0);
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(card).toBeFocused();
    expect(writes).toEqual([]);
    const [after, other] = await Promise.all([getIssue(first.key), getIssue(second.key)]);
    expect(after.status).toBe(first.status);
    expect(other.status).toBe(second.status);
  } finally {
    await context.close();
  }
});

test("a fresh open starts on an empty query, so it lists the new page's actions", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Astrolabe calibration" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const input = page.getByRole("combobox", { name: "Search" });
    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });

    await page.goto("/projects/CORE/issues");
    await expect(page.getByRole("heading", { level: 1, name: "Core" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("astrolabe");
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // In-app navigation, never a reload: a reload would reset the palette's own state, which is
    // exactly what must not be what makes this work.
    await page
      .getByRole("link", { name: /Astrolabe calibration/ })
      .first()
      .click();
    await expect(
      page.getByRole("heading", { level: 1, name: "Astrolabe calibration" })
    ).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(input).toHaveValue("");
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a header write in flight withdraws the rows whose buttons it disables", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "In-flight guard" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the PATCH so the header's write stays in flight for the whole assertion.
    const release = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() === "PATCH") {
        await release.promise;
      }
      await route.continue();
    });
    await openIssue(page, issue.key, "In-flight guard");

    const closeButton = page.getByRole("button", { name: "Close issue" });
    await closeButton.click();
    await expect(closeButton).toBeDisabled();
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toBeDisabled();

    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Go to Inbox" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toHaveCount(0);
    release.resolve();
  } finally {
    await context.close();
  }
});

test("a Set priority row the server refuses is reported by the header, in the digit key's one failure", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused palette priority" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Refused palette priority");
    let patches = 0;
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      patches += 1;
      await route.fulfill({
        body: JSON.stringify({ error: { code: "INTERNAL", message: "nope" } }),
        contentType: "application/json",
        status: 500,
      });
    });

    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    await dialog
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Set priority P2" })
      .click();
    await expect(dialog).toHaveCount(0);

    // The row writes through the page's one priority write, so the header reports its refusal
    // and the badge rolls back, exactly as for the picker and the digit keys.
    const header = page.getByTestId("issue-header");
    const failure = header
      .getByRole("alert")
      .filter({ hasText: `Could not update the priority of ${issue.key}.` });
    await expect(failure).toBeVisible();
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
    expect(patches).toBe(1);

    // A digit is the same write, so its refusal lands in that failure rather than beside it.
    await page.locator("body").focus();
    await page.keyboard.press("3");
    await expect.poll(() => patches).toBe(2);
    await expect(failure).toHaveCount(1);
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("a List row's own row runs on the row that opened the palette, and its movement keys are no rows", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "List palette" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues");
    const row = page.locator(`[data-issue-row]`, { hasText: "List palette" });
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    // `Enter` from the row opens it too, and `Open issue` is already that action's row.
    // Soft, so one run names every row that should not be there.
    for (const absent of ["Next issue", "Previous issue", "Open the focused issue"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await actions.getByRole("option", { name: "Open issue" }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}$`));
  } finally {
    await context.close();
  }
});

test("the Inbox offers the selection's snooze but not the keys that mark and clear it", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inbox selection palette" });
  await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: { id: "e2e-palette", kind: "session" }, as: "agent" }
  );
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/");
    const row = page.locator("[data-inbox-row]");
    await expect(row).toHaveCount(1);
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();
    await page.keyboard.press("x");
    const bar = page.getByRole("group", { name: "Selected asks" });
    await expect(bar).toContainText("1 selected");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    const snooze = actions.getByRole("option", {
      name: "Snooze the focused ask, or every selected ask",
    });
    await page.keyboard.press("Control+k");
    await expect(snooze).toHaveCount(1);
    await expect
      .soft(actions.getByRole("option", { name: "Select or deselect the focused ask" }))
      .toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(row).toBeFocused();

    // Off the row, with the mark still made: Escape here clears the selection, and in the
    // palette Escape closes the palette, so that binding is no row. The snooze still is.
    await page.keyboard.press("Escape");
    await expect(row).not.toBeFocused();
    await expect(bar).toContainText("1 selected");
    await page.keyboard.press("Control+k");
    await expect(snooze).toHaveCount(1);
    await expect.soft(actions.getByRole("option", { name: "Clear the selection" })).toHaveCount(0);

    // It runs on the page the palette opened over: the bulk picker takes focus, the mark kept.
    await snooze.click();
    await expect(dialog).toHaveCount(0);
    await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).toBeFocused();
    await expect(bar).toContainText("1 selected");
  } finally {
    await context.close();
  }
});

test("the Agents page offers a row's picker, which then commits a keyboard pick on Enter", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await seedAgents();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openAgents(page);
    const row = page.locator("[data-agent-row]").nth(0);
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    for (const offered of [
      "Message the focused agent",
      "Pick an issue for the message",
      "Pin or unpin the focused agent",
    ]) {
      await expect(actions.getByRole("option", { name: offered })).toHaveCount(1);
    }
    for (const absent of ["Next agent", "Previous agent", "Select or deselect the focused agent"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }

    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("pick an issue");
    await expect(actions.getByRole("option")).toHaveCount(1);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);

    // The row lands in the picker exactly as `i` does, and the picker's own rule holds: the
    // arrows only move the selection, and Enter commits it and hands the reader to the composer.
    const picker = row.getByRole("combobox", { name: "Issue" });
    const toggle = row.getByRole("button", { name: "Choose issue" });
    await expect(picker).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(picker).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(toggle).toContainText("CORE-2");
    const field = row.getByRole("textbox", { name: "Comment" });
    await expect(field).toBeFocused();
    await expect(field).toHaveValue("@Planner");
  } finally {
    await context.close();
  }
});

test("hits that arrive after the reader has arrowed keep the highlight on the row they chose", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Go held search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the search the way a slow network would, so the reader arrows before the hits arrive.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await openIssue(page, issue.key, "Go held search");
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    // `go to` starts every `Go to …` label, so those rows lead and can be arrowed among while
    // the search is out.
    const rows = dialog.getByRole("group", { exact: true, name: "Actions" }).getByRole("option");
    await input.fill("go to");
    const documents = rows.filter({ hasText: "Go to Documents" });
    await expect(documents).toHaveCount(1);
    const documentsId = await documents.getAttribute("id");
    const ids = await rows.evaluateAll((options) => options.map((option) => option.id));
    await expect(input).toHaveAttribute("aria-activedescendant", ids[0] ?? "");
    for (let step = 0; step < ids.indexOf(documentsId ?? ""); step += 1) {
      await input.press("ArrowDown");
    }
    await expect(input).toHaveAttribute("aria-activedescendant", documentsId as string);
    await expect.poll(() => held).toBeGreaterThan(0);

    // The hits arrive below the rows the query leads to, and the chosen row keeps the highlight.
    release.resolve();
    await expect(dialog.getByRole("option", { name: /Go held search/ })).toHaveCount(1);
    await expect(input).toHaveAttribute("aria-activedescendant", documentsId as string);
    await input.press("Enter");
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
  } finally {
    await context.close();
  }
});

test("an arrow pressed before the search answers reaches no action the query finds mid-label", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Arrow before the issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await openIssue(page, issue.key, "Arrow before the issue search");
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("issue");
    await expect(dialog.getByRole("option", { name: "Close issue" })).toHaveCount(1);
    // Down and Enter right after typing, and again once the request is held: the list holds only
    // the actions `issue` finds mid-label, `Close issue` first, and the hits are still to arrive
    // above them, so neither burst highlights or runs anything.
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    expect(statusWrites).toEqual([]);
    await expect.poll(() => held).toBeGreaterThan(0);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");

    release.resolve();
    await expect(dialog.getByRole("option").first()).toHaveAttribute("id", /^search-option-issue-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
  } finally {
    await context.close();
  }
});

test("a stale answer refreshing for the same query keeps the arrows off the actions it finds mid-label", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stale answer for an issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    // The clock is the page's own, so the cached answer can go stale without a 30 s wait.
    await page.clock.install();
    await openIssue(page, issue.key, "Stale answer for an issue search");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const hit = dialog.getByRole("option").first();

    // The reader searched `issue` once: the answer is cached, with `Close issue` below the hit.
    await page.keyboard.press("Control+k");
    await input.fill("issue");
    await expect(hit).toHaveAttribute("id", /^search-option-issue-/);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // Past the answer's 30 s staleTime, the same query shows that answer and refetches it.
    await page.clock.fastForward("00:31");
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("issue");
    await expect.poll(() => held).toBeGreaterThan(0);
    await expect(hit).toHaveAttribute("id", /^search-option-issue-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    // The hits may still change above `Close issue`, so Down stays among them.
    await input.press("ArrowDown");
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    release.resolve();
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
  } finally {
    await context.close();
  }
});

test("a row whose control has gone since the palette opened does nothing", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stale row" });
  await patchIssue(issue.key, { status: "done" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    let patches = 0;
    page.on("request", (request) => {
      if (request.method() === "PATCH" && request.url().endsWith(`/api/v1/issues/${issue.key}`)) {
        patches += 1;
      }
    });
    await openIssue(page, issue.key, "Stale row");
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const reopen = dialog
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Reopen issue" });
    await expect(reopen).toBeVisible();

    // Someone else reopens the issue while the palette is open; the header's Reopen button goes.
    // A CSS locator, since what is asserted is the header behind the modal palette.
    await patchIssue(issue.key, { status: "in_progress" }, { login: "bob" });
    await expect(
      page.locator('[data-testid="issue-header"] button[aria-label="Reopen issue"]')
    ).toHaveCount(0);

    // The row checks its control again when it runs, so it writes nothing.
    await reopen.click();
    await expect(dialog).toHaveCount(0);
    await page.waitForTimeout(500);
    expect(patches).toBe(0);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
  } finally {
    await context.close();
  }
});

test("below xl the margin is the route's own sheet, so Shift+M and its row change nothing", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Compact margin" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 800, width: 1024 });
    await openIssue(page, issue.key, "Compact margin");
    const stored = () => page.evaluate(() => localStorage.getItem("dispatch.shell.margin:alice"));
    const before = await stored();

    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    await expect(actions.getByRole("option", { name: "Create issue" })).toBeVisible();
    // Soft, so one run names both rows if either is offered.
    for (const absent of ["Toggle margin", "Toggle sidebar"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    await page.locator("body").focus();
    await page.keyboard.press("Shift+M");
    await page.waitForTimeout(300);
    expect(await stored()).toBe(before);
  } finally {
    await context.close();
  }
});

test("the Architecture tab offers its focused component's Open row, and not its movement keys", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  expect((await syncArchitectureSource("CORE")).last_error).toBeNull();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/architecture");
    const row = page.locator('[data-component-row="platform"]');
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    const open = actions.getByRole("option", {
      name: "Open the focused component: its children, else its details",
    });
    await expect(open).toHaveCount(1);
    // Soft, so one run names every row that should not be there.
    for (const absent of ["Next component", "Previous component"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await open.click();
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture\?component=platform$/);
  } finally {
    await context.close();
  }
});

test("Go to project… and Keyboard shortcuts are rows, and each opens its own dialog", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const issue = await createIssue({ project: "CORE", title: "Openers" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Openers");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });

    // A row runs once the palette has closed, so a row that opens the projects list opens it.
    await page.keyboard.press("Control+k");
    await actions.getByRole("option", { name: "Go to project…" }).click();
    await expect(dialog.getByRole("group", { name: "Projects" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search" })).toHaveAttribute(
      "placeholder",
      "Go to project"
    );
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await actions.getByRole("option", { name: "Keyboard shortcuts" }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the arrows keep the highlighted row in view when the list is longer than the palette", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long list" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Long list");
    await page.keyboard.press("Control+k");
    const input = page.getByRole("combobox", { name: "Search" });
    const rows = page
      .getByRole("dialog", { name: "Search" })
      .getByRole("group", { name: "Actions" })
      .getByRole("option");
    await expect(rows.first()).toBeInViewport();
    // The issue page's rows run past what the list shows at once; were they to fit, this row
    // would prove nothing.
    const last = rows.last();
    await expect(last).not.toBeInViewport();

    // ArrowUp from the first row wraps to the last, which the list scrolls into view.
    await input.press("ArrowUp");
    await expect(input).toHaveAttribute(
      "aria-activedescendant",
      (await last.getAttribute("id")) as string
    );
    await expect(last).toBeInViewport();
  } finally {
    await context.close();
  }
});

test("a nested Architecture level offers Up one level, and not the keys that move into children", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: {
        "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
        "web.md": "---\ntitle: Web\nparent: platform\n---\nThe SPA.\n",
      },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  expect((await syncArchitectureSource("CORE")).last_error).toBeNull();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/architecture?component=platform");
    const row = page.locator('[data-component-row="web"]');
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    const up = actions.getByRole("option", { name: "Up one level" });
    await expect(up).toHaveCount(1);
    // `l` into children is `o`'s Open row where there are children and nothing on a leaf, so
    // `descend` stays out of the palette.
    await expect(
      actions.getByRole("option", { name: "Into the focused component's children" })
    ).toHaveCount(0);
    await up.click();
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture$/);
  } finally {
    await context.close();
  }
});

test("on a short window the palette fits the screen and each highlighted row shows whole", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Short window" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Short window");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    // The list keeps one row's height, so the shortest windows here cut the key hints and the
    // message, never the row: 240 px is the least that holds the input and one row from `xl`,
    // 200 px close to it below.
    for (const [width, height] of [
      [1440, 300],
      [1440, 240],
      [1279, 200],
    ] as const) {
      await page.setViewportSize({ height, width });
      await page.locator("body").focus();
      await page.keyboard.press("Control+k");
      await expect(dialog.getByRole("group", { name: "Actions" })).toBeVisible();
      await expect.soft(dialog, `${width}x${height}`).toBeInViewport({ ratio: 1 });
      for (let step = 0; step < 6; step += 1) {
        const active = await input.getAttribute("aria-activedescendant");
        await expect(page.locator(`[id="${active}"]`), `${width}x${height}`).toBeInViewport({
          ratio: 1,
        });
        await input.press("ArrowDown");
      }
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});

test("a window that shrinks under an open palette keeps the highlighted row in view", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "keyboard rows exercise a desktop viewport");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Shrinking window" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await openIssue(page, issue.key, "Shrinking window");
    await page.keyboard.press("Control+k");
    const input = page.getByRole("combobox", { name: "Search" });
    await expect(
      page.getByRole("dialog", { name: "Search" }).getByRole("group", { name: "Actions" })
    ).toBeVisible();
    // Down to the eleventh row, near the foot of the list at this height.
    for (let step = 0; step < 10; step += 1) {
      await input.press("ArrowDown");
    }
    const active = page.locator(`[id="${await input.getAttribute("aria-activedescendant")}"]`);
    await expect(active).toBeInViewport({ ratio: 1 });

    // No key is pressed after the resize: the list itself brings the row back into view.
    await page.setViewportSize({ height: 420, width: 1440 });
    await expect(active).toBeInViewport({ ratio: 1 });
  } finally {
    await context.close();
  }
});
