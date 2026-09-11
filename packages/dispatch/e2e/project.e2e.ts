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

  try {
    await page.goto("/projects/CORE");
    await expect(page).toHaveTitle("CORE · Core · Dispatch");
    await expect(page.getByRole("heading", { name: "Core" })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.locator("details > summary")).toHaveText([
      "triage (1)",
      "icebox (1)",
      "backlog (1)",
      "todo (2)",
      "in_progress (1)",
      "testing (1)",
      "done (1)",
    ]);
    await expect(page.getByRole("group", { name: "done (1)" })).not.toHaveAttribute("open", "");
    await expect(
      page
        .getByRole("listitem", { name: /CORE-.*Child work/ })
        .getByRole("link", { name: parent.key })
    ).toHaveAttribute("href", `/issues/${parent.key}`);

    await page.getByRole("combobox", { name: "Status" }).selectOption("testing");
    await expect(page.getByText("Child work")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await page.getByRole("combobox", { name: "Status" }).selectOption("all");

    await page.getByRole("button", { name: "Needs you" }).click();
    await expect(page.getByText("Needs attention")).toBeVisible();
    await expect(page.getByText("Triage work")).toHaveCount(0);
    await page.getByRole("button", { name: "Needs you" }).click();

    await page.getByRole("button", { name: "Unread" }).click();
    await expect(page.getByText("Unread work")).toBeVisible();
    await expect(page.getByText("Needs attention")).toHaveCount(0);
    await page.getByRole("button", { name: "Unread" }).click();

    await page.getByRole("combobox", { name: "Label" }).selectOption("frontend");
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
      await expectTouchTarget(page.getByRole("combobox", { name: "Status" }));
      await expectTouchTarget(page.getByRole("button", { name: "Needs you" }));
      await expectTouchTarget(page.getByRole("button", { name: "Unread" }));
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
    await expect(page.getByRole("main").getByRole("tab")).toHaveText([
      "Spec",
      "Conversation",
      "Children",
      "Artifacts",
    ]);
    await expect(page.getByRole("button", { name: /make primary/i })).toHaveCount(0);
    await page.getByRole("link", { name: "Design notes" }).click();
    await expect(page).toHaveURL("/projects/CORE/documents/design-notes");
    await expect(page.getByRole("region", { name: "Referenced by" })).toContainText(
      `Comment · ${issue.key}`
    );
    await expect(page.getByRole("link", { name: `Artifact · ${issue.key}` })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/source-doc`
    );
    await page.screenshot({
      path: testInfo.outputPath("project-document-references.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
