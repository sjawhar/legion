import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  getIssue,
  patchIssue,
  putIssueState,
} from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

async function expectTouchTarget(locator: Locator): Promise<void> {
  const box = await locator.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
}

async function expectNoHorizontalOverflow(page: Page): Promise<void> {
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
    )
  ).toBe(true);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("project page groups issues by status in board order; filters narrow issues; documents stay scoped to the project", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const triage = await createIssue({ project: "CORE", title: "Triage work" });
  const icebox = await createIssue({ project: "CORE", title: "Icebox work" });
  const backlog = await createIssue({ project: "CORE", title: "Backlog work" });
  const parent = await createIssue({ project: "CORE", title: "Parent work" });
  await createIssueArtifact(parent.key, {
    content: "# Issue-only document\n",
    name: "Issue-only document",
  });
  const needsYou = await createIssue({ project: "CORE", title: "Needs attention" });
  const unread = await createIssue({ project: "CORE", title: "Unread work" });
  const child = await createIssue({ parent: parent.key, project: "CORE", title: "Child work" });
  const done = await createIssue({ project: "CORE", title: "Done work" });
  await patchIssue(triage.key, { status: "triage" });
  await patchIssue(icebox.key, { status: "icebox" });
  await patchIssue(backlog.key, { status: "backlog" });
  await patchIssue(parent.key, { status: "todo" });
  await patchIssue(needsYou.key, { labels: ["frontend"], status: "todo" });
  await patchIssue(unread.key, { labels: ["backend"], status: "in_progress" });
  await patchIssue(child.key, { status: "testing" });
  await patchIssue(done.key, { status: "done" });
  await createAsk(needsYou.key, { question: "Does this need review?" }, session);
  for (const issue of [triage, icebox, backlog, parent, needsYou, child, done]) {
    const latest = await getIssue(issue.key);
    await putIssueState(issue.key, { last_read_seq: latest.last_seq });
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ width: 1280, height: 800 });
  }

  try {
    await page.goto("/projects/CORE");
    await expect(page).toHaveTitle("CORE · Core · Dispatch");
    await expect(page.getByRole("heading", { name: "Core" })).toBeVisible();
    const blockedOnYou = page.getByRole("link", { name: "Blocked on you · 1" });
    await expect(blockedOnYou).toHaveAttribute("href", "/");
    if (testInfo.project.name === "chromium") {
      // The filter strip is the first content row under the dense header; the issue list
      // follows it. Density contract: that first row starts within 120px of the top.
      const filterStrip = page.getByRole("button", { name: /Filters · \d+ active/ });
      const stripBox = await filterStrip.boundingBox();
      const issueList = page.getByRole("region", { name: "Project issues" });
      const issueListBox = await issueList.boundingBox();
      if (stripBox === null || issueListBox === null) {
        throw new Error("project issue list is not visible");
      }
      expect(stripBox.y).toBeLessThanOrEqual(120);
      expect(issueListBox.y).toBeLessThanOrEqual(stripBox.y + stripBox.height + 24);
      const blockerBox = await blockedOnYou.boundingBox();
      const mainBox = await page.getByTestId("main-content").boundingBox();
      if (blockerBox === null || mainBox === null) {
        throw new Error("project header is not visible");
      }
      expect(blockerBox.x + blockerBox.width).toBeLessThanOrEqual(mainBox.x + mainBox.width);
    }
    await expect(page.getByRole("tab", { name: "Issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.locator("details > summary")).toHaveText([
      "Triage (1)",
      "Icebox (1)",
      "Backlog (1)",
      "Todo (2)",
      "In progress (1)",
      "Testing (1)",
      "Done (1)",
    ]);
    await expect(page.getByRole("group", { name: "Done (1)" })).not.toHaveAttribute("open", "");
    await expect(
      page
        .getByRole("listitem", { name: /CORE-.*Child work/ })
        .getByRole("link", { name: parent.key })
    ).toHaveAttribute("href", `/issues/${parent.key}`);

    // Status is a multi-select: one status narrows, a second widens (OR), each is a chip. Rows
    // and chips read the display label; the URL keeps the key.
    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await pickFilterOption(page, "Status", "Testing");
    await expect(page).toHaveURL(/\?status=testing$/);
    await expect(page.getByText("Child work")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await pickFilterOption(page, "Status", "Triage");
    await expect(page).toHaveURL(/\?status=testing&status=triage$/);
    await expect(page.getByText("Triage work")).toBeVisible();
    await expect(page.getByText("Backlog work")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Filters · 2 active" })).toBeVisible();
    await page.getByRole("button", { name: "Remove Status: Testing filter" }).click();
    await expect(page.getByText("Child work")).toHaveCount(0);
    await page.getByRole("button", { name: "Remove Status: Triage filter" }).click();
    await expect(page.getByText("Backlog work")).toBeVisible();

    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await page.getByRole("button", { exact: true, name: "Needs you" }).click();
    await expect(page.getByText("Needs attention")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await page.getByRole("button", { exact: true, name: "Needs you" }).click();

    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await page.getByRole("button", { exact: true, name: "Unread" }).click();
    await expect(page.getByText("Unread work")).toBeVisible();
    await expect(page.getByText("Needs attention")).toHaveCount(0);
    await page.getByRole("button", { exact: true, name: "Unread" }).click();

    await page.getByRole("button", { name: "Filters · 0 active" }).click();
    await pickFilterOption(page, "Labels", "frontend");
    await expect(page.getByText("Needs attention")).toBeVisible();
    await expect(page.getByText("Unread work")).toHaveCount(0);
    await page.getByRole("searchbox", { name: "Search issues" }).fill("attention");
    await expect(page.getByText("Needs attention")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("project-issues.png"), fullPage: true });

    await page.getByRole("tab", { name: "Documents" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents");
    await page.getByRole("button", { name: "New document" }).click();
    await page.getByRole("textbox", { name: "Title" }).fill("Design notes");
    await page.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("textbox", { name: "Document editor" })).toContainText(
      "Design notes"
    );
    await page.goto("/projects/CORE/documents");
    await expect(page.getByRole("link", { name: "Design notes", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("project-documents.png"), fullPage: true });
    await expect(page.getByText("Issue-only document", { exact: true })).toHaveCount(0);

    if (testInfo.project.name === "iphone") {
      await expectNoHorizontalOverflow(page);
      await expectTouchTarget(page.getByRole("tab", { name: "Issues" }));
      await expectTouchTarget(page.getByRole("button", { name: "New document" }));
      await expectTouchTarget(page.getByRole("button", { name: /Drop a file here/ }));
      await page.getByRole("tab", { name: "Issues" }).click();
      await page.getByRole("button", { name: "Filters · 0 active" }).click();
      await expectTouchTarget(filterPicker(page, "Status"));
      await expectTouchTarget(filterPicker(page, "Labels"));
      await expectTouchTarget(page.getByRole("button", { name: "Needs you" }));
      await expectTouchTarget(page.getByRole("button", { name: "Unread" }));
      await filterPicker(page, "Status").click();
      await expectTouchTarget(page.getByRole("option", { exact: true, name: "Todo" }));
      await page.keyboard.press("Escape");
      await expectNoHorizontalOverflow(page);
    }
  } finally {
    await context.close();
  }
});

test("an issue's Artifacts tab lists its reference closure and a document page lists its referrers", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reference source" });
  const related = await createIssue({ project: "CORE", title: "Referenced artifact" });
  await createIssueArtifact(issue.key, {
    content: "See dispatch://CORE/artifact/design-notes",
    name: "Source doc",
  });
  await createIssueArtifact(related.key, { content: "diagram", name: "Diagram" });
  await createProjectDocument("CORE", {
    content: `See dispatch://${related.key}/artifact/diagram`,
    name: "Design notes",
  });
  await createComment(issue.key, { body: "See dispatch://CORE/artifact/design-notes" }, session);
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/artifacts`);
    await page.getByText(/References \(\d+\)/).click();
    const references = page.getByRole("region", { name: "References" });
    await expect(references.getByRole("link", { name: "Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes"
    );
    await expect(references).toContainText("via artifact");
    await expect(references).toContainText("depth 1");
    await expect(references).toContainText("depth 2");
    const tabs = page.getByRole("main").getByRole("tab");
    await expect(tabs).toHaveCount(4);
    for (const [index, name] of ["Spec", "Conversation", "Children", "Artifacts (1)"].entries()) {
      await expect(tabs.nth(index)).toHaveAccessibleName(name);
    }
    await expect(page.getByRole("button", { name: /make primary/i })).toHaveCount(0);
    await page.getByRole("link", { name: "Design notes" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("region", { name: "Referenced by" })).toContainText(
      `Comment · ${issue.key}`
    );
    // A document mention deep-links to the block that carries it.
    await expect(page.getByRole("link", { name: `Artifact · ${issue.key}` })).toHaveAttribute(
      "href",
      new RegExp(`^/issues/${issue.key}/artifacts/source-doc#b-`)
    );
    await page.screenshot({
      path: testInfo.outputPath("project-document-references.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
