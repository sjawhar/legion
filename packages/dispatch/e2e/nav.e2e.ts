import { expect, type Page, test } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { actor: { id: "e2e-nav", kind: "session" as const }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

test("sidebar shows Inbox, Pinned, one row per project with its open-ask badge, and Settings — never an issue row — on desktop and in the phone drawer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const coreIssues = await Promise.all(
    Array.from({ length: 12 }, (_value, index) =>
      createIssue({ project: "CORE", title: `Core issue ${index + 1}` })
    )
  );
  const opsIssues = await Promise.all(
    Array.from({ length: 8 }, (_value, index) =>
      createIssue({ project: "OPS", title: `Operations issue ${index + 1}` })
    )
  );
  await Promise.all([
    ...coreIssues
      .slice(0, 2)
      .map((issue, index) => createAsk(issue.key, { question: `Core ask ${index + 1}` }, session)),
    ...opsIssues
      .slice(0, 1)
      .map((issue) => createAsk(issue.key, { question: "Operations ask" }, session)),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The sidebar must never fan out per issue. The one sanctioned per-issue request on the
  // Inbox is each ask card's `GET /issues/{key}/subscribers` (`AskRecipients`, for `Reaches N`),
  // which is the card's, not the navigation's, so the recorder leaves it out.
  const issueRequests: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" && url.pathname === "/api/v1/issues") {
      issueRequests.push(url.search);
    }
    if (
      request.method() === "GET" &&
      url.pathname.startsWith("/api/v1/issues/") &&
      !url.pathname.endsWith("/subscribers")
    ) {
      issueRequests.push(url.pathname);
    }
  });

  try {
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("3");
    await expect(navigation.getByRole("heading", { name: "Pinned" })).toHaveCount(0);
    await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("2");
    await expect(navigation.locator('a[href="/projects/OPS"]')).toContainText("1");
    await expect(navigation.getByRole("link", { name: "Settings" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: /^(CORE|OPS)-\d+/ })).toHaveCount(0);
    expect(issueRequests).toEqual(["?pinned=true"]);
    await page.screenshot({ path: testInfo.outputPath("projects-sidebar.png"), fullPage: true });
  } finally {
    await context.close();
  }
});

test("issue tabs are URL-driven and keyboard-navigable, and titles follow direct issue routes", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ship the release" });
  await createIssue({ parent: issue.key, project: "CORE", title: "Child task" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.goto(`/issues/${issue.key}`);
    await expect(page).toHaveTitle(`${issue.key} · ${issue.title} · Dispatch`);
    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");

    await page.getByRole("tab", { name: "Children" }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/children$`));
    await page.reload();
    await expect(page.getByRole("tab", { name: "Children" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("tabpanel")).toContainText("Child task");

    await page.getByRole("tab", { name: "Children" }).focus();
    await page.keyboard.press("ArrowLeft");
    await expect(page.getByRole("tab", { name: "Conversation" })).toBeFocused();
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));

    await page.goto("/issues/CORE-999");
    await expect(page.getByRole("heading", { name: "Issue not found" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();

    await page.goto(`/issues/${issue.key}/not-a-tab`);
    await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    await expect(page.getByRole("tablist", { name: "Issue detail" })).toHaveCount(0);
    await expect(page).toHaveTitle("Not found · Dispatch");
    await page.goto("/definitely-not-a-dispatch-route");
    await expect(page.getByRole("heading", { name: "Page not found" })).toBeVisible();
    await expect(page).toHaveTitle("Not found · Dispatch");
    await expect(page.getByRole("link", { name: "Back to inbox" })).toBeVisible();
  } finally {
    await context.close();
  }
});

// One count, one colour: the Inbox badge means the viewer must act, and a reader who learns it
// in the drawer must recognise it in the top bar.
test("the needs-you count is the same badge in the phone top bar and the drawer", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the compact top bar exists below the xl rail");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Badge colour" });
  await createAsk(issue.key, { question: "Which colour?" }, session);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    const fill = (locator: ReturnType<Page["getByText"]>) =>
      locator.evaluate((element) => getComputedStyle(element).backgroundColor);
    const topBar = page.getByText("Needs you 1", { exact: true });
    await expect(topBar).toBeVisible();
    const topBarFill = await fill(topBar);
    await page.getByRole("button", { name: "Open navigation" }).click();
    const drawer = page.getByRole("link", { name: /Inbox/ }).getByText("Needs you 1");
    await expect(drawer).toBeVisible();
    expect(topBarFill).toBe(await fill(drawer));
  } finally {
    await context.close();
  }
});
