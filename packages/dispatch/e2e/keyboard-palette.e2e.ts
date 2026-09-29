import { expect, type Page, test } from "@playwright/test";

import { createAsk, createIssue, createProject, getIssue } from "./api";
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
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
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
    // A binding whose whole job is to open a palette cannot be a palette row: selecting one runs
    // it and then closes the palette in the same batch, so the row would do nothing at all.
    for (const opensThePalette of [
      "Go to project…",
      "Search",
      "Search only",
      "Keyboard shortcuts",
    ]) {
      await expect(actions.getByRole("option", { name: opensThePalette })).toHaveCount(0);
    }
    await page.screenshot({
      path: testInfo.outputPath(`palette-actions-${testInfo.project.name}.png`),
    });

    // Arrows move over action rows exactly as they move over hits.
    const closeId = await close.getAttribute("id");
    expect(closeId).not.toBeNull();
    await expect(input).toHaveAttribute("aria-activedescendant", closeId as string);
    await input.press("ArrowDown");
    await expect(input).not.toHaveAttribute("aria-activedescendant", closeId as string);
    await input.press("ArrowUp");
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

    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect.poll(() => getIssue(issue.key).then((read) => read.status)).toBe("backlog");

    // A filtered action runs the same write its control does.
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("priority p2");
    await expect(actions.getByRole("option")).toHaveCount(1);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toHaveValue("2");
    await expect.poll(() => getIssue(issue.key).then((read) => read.priority)).toBe(2);
  } finally {
    await context.close();
  }
});

test("/ searches only, and a query lists matching actions above the hits", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
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

    // The same query under `$mod+k` puts what this page can do above what the query found.
    await page.keyboard.press("Control+k");
    await input.fill("issue");
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    const options = dialog.getByRole("option");
    await expect(options.first()).toHaveAttribute("id", /^search-option-action-/);
    await expect(options.last()).toHaveAttribute("id", /^search-option-issue-/);
    await page.screenshot({
      path: testInfo.outputPath(`palette-filtered-${testInfo.project.name}.png`),
    });
  } finally {
    await context.close();
  }
});

test("g d opens the project's Documents and g p picks a project", async ({ browser }, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const issue = await createIssue({ project: "CORE", title: "Documents from an issue" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues");
    await expect(page.getByRole("heading", { level: 1, name: "Core" })).toBeVisible();
    await page.locator("body").focus();

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
    await page.screenshot({
      path: testInfo.outputPath(`palette-projects-${testInfo.project.name}.png`),
    });
    await input.press("Enter");
    await expect(page).toHaveURL(/\/projects\/OPS\/issues/);

    // An issue page names its project too, in its key, so `g d` and the palette row work there.
    await openIssue(page, issue.key, "Documents from an issue");
    const issueActions = page
      .getByRole("dialog", { name: "Search" })
      .getByRole("group", { name: "Actions" });
    await page.keyboard.press("Control+k");
    await expect(issueActions.getByRole("option", { name: "Go to Documents" })).toBeVisible();
    await page.keyboard.press("Escape");
    await page.keyboard.press("g");
    await page.keyboard.press("d");
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
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

test("the Inbox offers its ask actions and not its movement keys", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
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
  } finally {
    await context.close();
  }
});

test("a fresh open starts on an empty query, so it lists the new page's actions", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
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
  test.skip(
    testInfo.project.name === "iphone",
    "desktop keyboard navigation is covered by chromium"
  );
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
