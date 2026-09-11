import { expect, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
} from "./api";
import { countDocumentSockets, cursorLabel, documentEditor, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-doc-editor", origin: { tmux: "dispatch:1.3" } },
  as: "agent" as const,
};
const initialMarkdown =
  "## Database\n\nUse SQLite\n\n| col | value |\n|---|---|\n| a | 1 |\n\n- [ ] task\n\n```ts\nconst x = 1;\n```\n";

test.beforeEach(async () => {
  await resetDatabase();
});

test("the spec opens as a formatted, editable document with no source pane", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "READ", name: "Readable" });
  const issue = await createIssue({
    project: "READ",
    spec: initialMarkdown,
    title: "Rendering check",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);

    const documentArticle = page.locator('article[aria-label="Document"]');
    const editor = documentEditor(page);
    await expect(editor).toBeVisible();
    await expect(editor).toHaveAttribute("contenteditable", "true");

    const heading = documentArticle.getByRole("heading", { level: 2, name: "Database" });
    const paragraph = documentArticle.locator("p").first();
    await expect(heading).toBeVisible();
    await expect(paragraph).toBeVisible();
    const [headingSize, paragraphSize] = await Promise.all([
      heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
      paragraph.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
    ]);
    expect(headingSize).toBeGreaterThan(paragraphSize);

    const table = editor.locator("table");
    await expect(table).toBeVisible();
    const headerCell = table.locator("th").first();
    const [headerWeight, borderWidth, paddingLeft] = await Promise.all([
      headerCell.evaluate((element) => Number.parseInt(getComputedStyle(element).fontWeight, 10)),
      table
        .locator("td")
        .first()
        .evaluate((element) => Number.parseFloat(getComputedStyle(element).borderTopWidth)),
      table
        .locator("td")
        .first()
        .evaluate((element) => Number.parseFloat(getComputedStyle(element).paddingLeft)),
    ]);
    expect(headerWeight).toBeGreaterThanOrEqual(600);
    expect(borderWidth).toBeGreaterThan(0);
    expect(paddingLeft).toBeGreaterThan(0);
    await expect(editor.locator('input[type="checkbox"]')).toBeVisible();
    await expect(editor.locator("pre code")).toBeVisible();
    await expect(page.getByRole("button", { name: /^(Edit|Preview)$/ })).toHaveCount(0);
    await expect(page.getByRole("tabpanel", { name: "Spec" }).locator("textarea")).toHaveCount(0);

    if (testInfo.project.name === "iphone") {
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
    }
    await page.screenshot({ path: testInfo.outputPath("rendered-document.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

test("two users edit the same spec, see each other's text and cursor within a second, and settle one version attributed to both", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Database decision",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    await alicePage.goto(`/issues/${issue.key}/spec`);
    const aliceEditor = documentEditor(alicePage);
    await expect(aliceEditor).toContainText("Use SQLite");

    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}/spec`);
    const bobEditor = documentEditor(bobPage);
    await expect(bobEditor).toContainText("Use SQLite");
    await expect(alicePage.getByRole("status")).toHaveText("connected");
    await expect(bobPage.getByRole("status")).toHaveText("connected");

    await typeAtEnd(alicePage, "hello from alice");
    await expect(bobEditor).toContainText("hello from alice", { timeout: 1000 });

    await typeAtEnd(bobPage, "hello from bob");
    await expect(aliceEditor).toContainText("hello from bob", { timeout: 1000 });

    if (testInfo.project.name === "chromium") {
      await bobEditor.click();
      await expect(cursorLabel(alicePage, "bob")).toBeVisible({ timeout: 1000 });
      await expect(cursorLabel(bobPage, "alice")).toBeVisible({ timeout: 1000 });
    }

    await expect
      .poll(
        () =>
          getArtifact(artifactId).then((artifact) =>
            artifact.versions
              .find(({ number }) => number === 2)
              ?.authors.map(({ id }) => id)
              .sort()
          ),
        { timeout: 10_000 }
      )
      .toEqual(["alice", "bob"]);
    await expect
      .poll(
        () =>
          getArtifact(artifactId).then((artifact) =>
            artifact.versions.find(({ number }) => number === 2)
          ),
        { timeout: 10_000 }
      )
      .toMatchObject({ named: false, number: 2, summary: null });
    await expect
      .poll(() => getArtifactVersion(artifactId, 2).then(({ markdown }) => markdown), {
        timeout: 10_000,
      })
      .toContain("hello from alice");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("named versions, the version picker, and the diff stay current across users", async ({
  browser,
}) => {
  await createProject({ key: "VERS", name: "Versions" });
  const issue = await createIssue({
    project: "VERS",
    spec: initialMarkdown,
    title: "Version naming",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await expect(editor).toContainText("Use SQLite");
    await expect.poll(sockets).toBe(1);

    await editArtifact(
      artifactId,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(editor).toContainText("Use Postgres", { timeout: 1000 });
    await expect
      .poll(
        () =>
          getArtifact(artifactId).then((artifact) =>
            artifact.versions.find(({ number }) => number === 2)
          ),
        { timeout: 10_000 }
      )
      .toMatchObject({ named: false, number: 2, summary: null });

    page.once("dialog", (dialog) => dialog.accept("Decided Postgres"));
    await page.getByRole("button", { name: "Name version" }).click();
    await expect
      .poll(
        () =>
          getArtifact(artifactId).then((artifact) =>
            artifact.versions.find(({ number }) => number === 3)
          ),
        { timeout: 10_000 }
      )
      .toMatchObject({ named: true, number: 3, summary: "Decided Postgres" });

    const versionPicker = page.getByRole("combobox", { name: "Version" });
    await expect(versionPicker).toContainText("Version 3 — Decided Postgres");
    await versionPicker.selectOption("1");
    const versionView = page.getByTestId("version-view");
    await expect(versionView).toContainText("Use SQLite");
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
    await expect.poll(sockets).toBe(1);

    await page.getByRole("button", { name: "Diff vs current" }).click();
    await expect(page.getByTestId("version-diff").locator("del")).toContainText("SQLite");
    await expect(page.getByTestId("version-diff").locator("ins")).toContainText("Postgres");

    await versionPicker.selectOption("");
    await expect(documentEditor(page)).toBeVisible();
    await expect.poll(sockets).toBe(1);
  } finally {
    await alice.close();
  }
});

test("an agent edit lands live in every open editor", async ({ browser }) => {
  await createProject({ key: "LIVE", name: "Live updates" });
  const issue = await createIssue({
    project: "LIVE",
    spec: initialMarkdown,
    title: "Agent edit",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const aliceEditor = documentEditor(alicePage);
    const bobEditor = documentEditor(bobPage);
    await expect(aliceEditor).toContainText("Use SQLite");
    await expect(bobEditor).toContainText("Use SQLite");

    await editArtifact(
      issue.primary_artifact_id,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(aliceEditor).toContainText("Use Postgres", { timeout: 1000 });
    await expect(bobEditor).toContainText("Use Postgres", { timeout: 1000 });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("reloading mid-edit reopens the same content", async ({ browser }) => {
  await createProject({ key: "LOAD", name: "Reloads" });
  const issue = await createIssue({
    project: "LOAD",
    spec: initialMarkdown,
    title: "Reload the document",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const bobEditor = documentEditor(bobPage);
    await expect(documentEditor(alicePage)).toContainText("Use SQLite");
    await expect(bobEditor).toContainText("Use SQLite");

    await typeAtEnd(alicePage, "before reload");
    await expect(bobEditor).toContainText("before reload", { timeout: 1000 });
    await alicePage.reload();
    await expect(documentEditor(alicePage)).toContainText("Use SQLite");
    await expect(documentEditor(alicePage)).toContainText("before reload");
    await expect(alicePage.getByRole("combobox", { name: "Version" })).toContainText("Version 1");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("tab round-trips keep one document connection, the typed text, and the log scroll position", async ({
  browser,
}) => {
  await createProject({ key: "TABS", name: "Tabs" });
  const issue = await createIssue({
    project: "TABS",
    spec: "# Keep this document",
    title: "Mounted panels",
  });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await typeAtEnd(page, "stay mounted");
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);

    await page.getByRole("tab", { name: "Log" }).click();
    await expect(page.getByRole("tab", { name: "Log" })).toBeFocused();
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 500));
    const scrollPosition = await page.evaluate(() => window.scrollY);
    expect(scrollPosition).toBeGreaterThan(0);

    await page.getByRole("tab", { name: "Log" }).press("ArrowLeft");
    await expect(editor).toContainText("stay mounted");
    await page.getByRole("tab", { name: "Spec" }).press("ArrowRight");
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await expect.poll(sockets).toBe(1);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(scrollPosition);
  } finally {
    await alice.close();
  }
});

test("a version deep link renders the comment's original quote highlighted in a read-only editor", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "LINK", name: "Linked documents" });
  const issue = await createIssue({
    project: "LINK",
    spec: "SQLite is local",
    title: "Deep link highlight",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "SQLit" }, body: "Use the embedded store." },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(
      `/issues/${issue.key}/artifact/spec?v=1&comment=${encodeURIComponent(comment.id)}`
    );

    const versionView = page.getByRole("region", { name: "Document version 1" });
    const highlight = versionView.locator(`[data-id="${comment.id}"]`);
    await expect(highlight).toHaveText("SQLit");
    await expect(versionView.locator(".dispatch-mark-pulse")).toHaveCount(1);
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
    await page.screenshot({
      path: testInfo.outputPath("spec-deep-link-highlight.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});
