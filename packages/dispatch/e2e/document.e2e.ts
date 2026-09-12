import { expect, type Page, test } from "@playwright/test";

import {
  createProject,
  createProjectDocument,
  getArtifact,
  getAsk,
  getProjectArtifact,
} from "./api";
import { barAction, documentEditor, markSpan, selectEditorText, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

async function setDocumentSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const thread = page.getByRole("dialog", { name: "Thread" });
  if (!open && (await thread.count()) > 0) {
    await thread.getByRole("button", { name: "Back" }).click();
  }
  const sheet = page.getByTestId("margin-sheet");
  if ((await sheet.getAttribute("data-expanded")) !== String(open)) {
    await page
      .getByRole("button", { name: open ? /Open review panel/ : /Close review panel/ })
      .click();
  }
  await expect(sheet).toHaveAttribute("data-expanded", String(open));
}
test.beforeEach(async () => {
  await resetDatabase();
});

test("documentCollaborationLive", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    await alicePage.goto("/projects/CORE/documents");
    await alicePage.getByRole("button", { name: "New document" }).click();
    await alicePage.getByRole("textbox", { name: "Title" }).fill("Design notes");
    await alicePage.getByRole("button", { name: "Create" }).click();
    await expect(alicePage).toHaveURL("/projects/CORE/documents/design-notes");

    const artifact = await getProjectArtifact("CORE", "design-notes");
    const bobPage = await bob.newPage();
    await bobPage.goto("/projects/CORE/documents/design-notes");
    await expect(documentEditor(alicePage)).toContainText("Design notes");
    await expect(documentEditor(bobPage)).toContainText("Design notes");
    await expect(alicePage.getByRole("status", { name: "connected" })).toHaveText("connected");
    await expect(bobPage.getByRole("status", { name: "connected" })).toHaveText("connected");

    await typeAtEnd(alicePage, "Alice wrote this.");
    await expect(documentEditor(bobPage)).toContainText("Alice wrote this.", { timeout: 1000 });
    await alicePage.reload();
    await expect(documentEditor(alicePage)).toContainText("Alice wrote this.");
    await expect
      .poll(() => getArtifact(artifact.id).then((current) => current.versions.length), {
        timeout: 10_000,
      })
      .toBeGreaterThan(1);
    const versionCount = await getArtifact(artifact.id).then((current) => current.versions.length);
    await expect(
      alicePage.getByRole("combobox", { name: "Version" }).locator("option")
    ).toHaveCount(versionCount + 1);
    await alicePage.screenshot({
      path: testInfo.outputPath("project-document-live.png"),
      fullPage: true,
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("comment, suggest, and ask anchor marks on a project document; accept edits the text; the ask flows through the Inbox", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const created = await createProjectDocument("CORE", {
    content: "# Design notes\n\nComment target.\n\nSuggestion target.\n\nAsk target.\n",
    name: "Design notes",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const path = "/projects/CORE/documents/design-notes";

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([alicePage.goto(path), bobPage.goto(path)]);
    await expect(documentEditor(alicePage)).toContainText("Comment target.");
    await expect(documentEditor(bobPage)).toContainText("Comment target.");

    await selectEditorText(alicePage, "Comment target");
    await barAction(alicePage, "Comment");
    const commentComposer = alicePage.getByRole("form", { name: "Comment composer" });
    await commentComposer.getByLabel("Comment").fill("This needs an explanation.");
    await commentComposer.getByRole("button", { exact: true, name: "Comment" }).click();
    const commentCard = alicePage.locator("[data-margin-item]", {
      hasText: "This needs an explanation.",
    });
    await expect(commentCard).toBeVisible();
    await expect(
      documentEditor(alicePage).locator("span[data-proof][data-id]", { hasText: "Comment target" })
    ).toBeVisible();
    await expect(
      documentEditor(bobPage).locator("span[data-proof][data-id]", { hasText: "Comment target" })
    ).toBeVisible({ timeout: 1000 });
    await setDocumentSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "Suggestion target");
    await barAction(alicePage, "Suggest");
    const suggestionComposer = alicePage.getByRole("form", { name: "Suggest composer" });
    await suggestionComposer.getByLabel("Replacement").fill("Accepted text");
    await suggestionComposer.getByRole("button", { exact: true, name: "Suggest" }).click();
    const suggestionCard = alicePage
      .locator("[data-margin-item]", { hasText: "Suggested replacement." })
      .last();
    await expect(suggestionCard).toBeVisible();
    const suggestionId = await suggestionCard.getAttribute("data-margin-item");
    if (suggestionId === null) {
      throw new Error("Document suggestion did not receive a margin id.");
    }
    const suggestionCardByID = alicePage.locator(`[data-margin-item="${suggestionId}"]`);
    await suggestionCardByID.getByRole("button").click();
    await suggestionCardByID.getByRole("button", { name: "Accept" }).click();
    await expect(documentEditor(alicePage)).toContainText("Accepted text");
    await expect(documentEditor(bobPage)).toContainText("Accepted text", { timeout: 1000 });
    await expect
      .poll(
        () =>
          getArtifact(created.artifact.id).then((artifact) =>
            artifact.versions.some(({ named }) => named)
          ),
        { timeout: 10_000 }
      )
      .toBe(true);

    await setDocumentSheet(alicePage, testInfo.project.name, false);
    await selectEditorText(alicePage, "Ask target");
    await barAction(alicePage, "Ask");
    const askComposer = alicePage.getByRole("form", { name: "Ask composer" });
    await askComposer.getByLabel("Question").fill("Should we publish this?");
    await askComposer.getByRole("button", { exact: true, name: "Ask" }).click();
    const askCard = alicePage.locator("[data-margin-item]", { hasText: "Should we publish this?" });
    await expect(askCard).toBeVisible();
    const askId = await askCard.getAttribute("data-margin-item");
    if (askId === null) {
      throw new Error("Document ask did not receive a margin id.");
    }
    const ask = await getAsk(askId);
    if (ask.ask.anchor === null) {
      throw new Error("Document ask did not retain its anchor.");
    }
    await expect(markSpan(bobPage, ask.ask.anchor.mark_id)).toContainText(ask.ask.anchor.quote, {
      timeout: 1000,
    });
    await bobPage.goto("/");
    const inboxDocument = bobPage.getByRole("link", { name: /CORE.*Design notes/ });
    await expect(inboxDocument).toBeVisible();
    const inboxAsk = bobPage.getByTestId(`ask-${askId}`);
    await inboxAsk.getByLabel("Your answer").fill("Yes, publish it.");
    await inboxAsk.getByRole("button", { name: "Answer" }).click();
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({
        ask: { answer: { text: "Yes, publish it.", user: "bob" }, state: "answered" },
      });
    await expect(inboxAsk).toHaveCount(0);
    await bobPage.goto(path);
    await setDocumentSheet(bobPage, testInfo.project.name, true);
    await expect(bobPage.getByText("Yes, publish it.")).toBeVisible();
    await bobPage.screenshot({
      path: testInfo.outputPath("project-document-margin.png"),
      fullPage: true,
    });
  } finally {
    await bob.close();
    await alice.close();
  }
});
