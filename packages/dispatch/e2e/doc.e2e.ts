import { expect, test } from "@playwright/test";

import {
  createIssue,
  createMessage,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
} from "./api";
import { enterEditMode } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-doc-editor", origin: { tmux: "dispatch:1.3" } },
  as: "agent" as const,
};
const initialMarkdown = "## Database\nUse SQLite";

test.beforeEach(async () => {
  await resetDatabase();
});

test("document edits synchronize, version, and compare across users", async ({
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
    await alicePage.goto(`/issues/${issue.key}`);
    await alicePage.getByRole("tab", { name: "Spec" }).click();
    await enterEditMode(alicePage);
    const aliceEditor = alicePage.getByRole("textbox", { name: "Document editor" });
    await expect(aliceEditor).toContainText("Use SQLite");
    await aliceEditor.click();
    await aliceEditor.press("Control+End");
    await aliceEditor.press("Enter");
    await aliceEditor.type("hello");

    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 2);
        return version;
      })
      .toMatchObject({ authors: [{ id: "alice", kind: "user" }], named: false, number: 2 });
    await expect
      .poll(() => getArtifactVersion(artifactId, 2).then(({ markdown }) => markdown))
      .toContain("hello");

    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}`);
    await bobPage.getByRole("tab", { name: "Spec" }).click();
    await enterEditMode(bobPage);
    const bobEditor = bobPage.getByRole("textbox", { name: "Document editor" });
    await expect(bobEditor).toContainText("hello");
    await bobEditor.click();
    const bobCaret = alicePage.locator(".cm-ySelectionCaret");
    await expect(bobCaret).toContainText("bob");
    await bobCaret.hover();
    await expect(bobCaret.locator(".cm-ySelectionInfo")).toHaveCSS("opacity", "1");
    await alicePage.screenshot({
      path: testInfo.outputPath("live-document-edit.png"),
      fullPage: true,
    });

    await editArtifact(
      artifactId,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(aliceEditor).toContainText("Postgres");
    await expect(bobEditor).toContainText("Postgres");
    // A summary-less API edit still records an unnamed version and its event.
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 3);
        return version;
      })
      .toMatchObject({ named: false, number: 3, summary: null });
    await expect
      .poll(() => getArtifactVersion(artifactId, 3).then(({ markdown }) => markdown))
      .toContain("Postgres");

    alicePage.once("dialog", (dialog) => dialog.accept("Decided Postgres"));
    await alicePage.getByRole("button", { name: "Name version" }).click();
    await expect
      .poll(async () => {
        const version = (await getArtifact(artifactId)).versions.find(({ number }) => number === 4);
        return version;
      })
      .toMatchObject({ named: true, number: 4, summary: "Decided Postgres" });

    const versionPicker = alicePage.getByLabel("Version");
    await expect(versionPicker).toContainText("Version 4 — Decided Postgres");
    await versionPicker.selectOption("2");
    await expect(alicePage.getByTestId("version-view")).toContainText("Use SQLite");
    await alicePage.getByRole("button", { name: "Diff vs current" }).click();
    await expect(alicePage.getByTestId("version-diff").locator("del")).toContainText("Use SQLite");
    await expect(alicePage.getByTestId("version-diff").locator("ins")).toContainText(
      "Use Postgres"
    );
    await alicePage.screenshot({
      path: testInfo.outputPath("document-version-diff.png"),
      fullPage: true,
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("the spec renders as a formatted document by default, with no click required", async ({
  browser,
}) => {
  await createProject({ key: "READ", name: "Readable" });
  const issue = await createIssue({
    project: "READ",
    spec: "# Database\n\nUse SQLite",
    title: "Rendering check",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();

    const heading = page.getByRole("article").getByRole("heading", { level: 1 });
    const paragraph = page.getByRole("article").locator("p").first();
    await expect(heading).toBeVisible();
    await expect(paragraph).toBeVisible();

    const [headingSize, paragraphSize] = await Promise.all([
      heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
      paragraph.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
    ]);
    expect(headingSize).toBeGreaterThan(paragraphSize);
  } finally {
    await alice.close();
  }
});

test("a current-spec deep link renders its historical range in preview", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "LINK", name: "Linked documents" });
  const issue = await createIssue({
    project: "LINK",
    spec: "SQLite is local",
    title: "Deep link highlight",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec?from=0&to=5`);

    await expect(page.getByRole("button", { exact: true, name: "Edit" })).toBeVisible();
    const highlight = page.locator("mark.dispatch-anchor-history");
    await expect(highlight).toBeVisible();
    await expect(highlight).toHaveText("SQLit");
    await page.screenshot({
      path: testInfo.outputPath("spec-deep-link-highlight.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});

test("tab round-trips preserve document connection, text, and log scroll position", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Keep this document",
    title: "Mounted panels",
  });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let documentConnections = 0;
  page.on("websocket", (websocket) => {
    if (new URL(websocket.url()).pathname.startsWith("/ws/")) {
      documentConnections += 1;
    }
  });

  try {
    await page.goto(`/issues/${issue.key}/spec`);
    await enterEditMode(page);
    const editor = page.getByRole("textbox", { name: "Document editor" });
    await editor.click();
    await editor.press("Control+End");
    await editor.press("Enter");
    await editor.type("stay mounted");
    await expect(editor).toContainText("stay mounted");
    await expect.poll(() => documentConnections).toBe(1);

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

    await expect.poll(() => documentConnections).toBe(1);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(scrollPosition);
  } finally {
    await alice.close();
  }
});
