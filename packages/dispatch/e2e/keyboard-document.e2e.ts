import { expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject, createProjectDocument, listArtifactComments } from "./api";
import {
  composer,
  connectedDot,
  documentEditor,
  expectMark,
  placeCaret,
  selectEditorText,
} from "./editor";
import { commentWithBody } from "./margin-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const initialMarkdown = "The quick brown fox";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The provisional or recorded comment marks in `page`'s editor. */
function commentMarks(page: Page): Locator {
  return documentEditor(page).locator('span[data-proof="comment"][data-id]');
}

/** Waits two rendering frames, so a composer a key press opened has had its chance to render. */
function nextFrames(page: Page): Promise<void> {
  return page.evaluate(
    () =>
      new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done())))
  );
}

// Ctrl+Alt+M (Cmd+Option+M on a Mac) is Google Docs' chord for the bar's Comment button. The
// editor's own keymap must leave it alone - it reaches the page only once ProseMirror passes on
// it - and it belongs to the editor: from the composer it opens, it types and opens nothing.
test("Ctrl+Alt+M comments on the selected text as the bar's Comment does, and only from the editor", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: initialMarkdown, title: "Chord" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText(initialMarkdown);
    await expect(connectedDot(page)).toHaveText("connected");
    const form = composer(page);

    // A caret selects nothing, so the bar would show no Comment, and the chord opens nothing.
    await placeCaret(page, "after", "quick");
    await page.keyboard.press("Control+Alt+KeyM");
    await nextFrames(page);
    await expect(form).toHaveCount(0);
    await expect(commentMarks(page)).toHaveCount(0);
    await expect(documentEditor(page)).toHaveText(initialMarkdown);

    await selectEditorText(page, "brown");
    await page.keyboard.press("Control+Alt+KeyM");
    await expect(form).toContainText("brown");
    await expect(form.getByLabel("Comment")).toBeFocused();
    await expect(commentMarks(page)).toHaveCount(1);
    const markId = await commentMarks(page).getAttribute("data-id");

    // The caret is in the composer: the chord types nothing there and opens no other comment.
    await page.keyboard.type("Why brown?");
    await page.keyboard.press("Control+Alt+KeyM");
    await nextFrames(page);
    await expect(form.getByLabel("Comment")).toHaveValue("Why brown?");
    await expect(form).toHaveCount(1);
    await expect(commentMarks(page)).toHaveCount(1);
    await expect(commentMarks(page)).toHaveAttribute("data-id", markId ?? "");

    await form.getByRole("button", { exact: true, name: "Send" }).click();
    const comment = await commentWithBody(issue.key, issue.primary_artifact_id, "Why brown?");
    expect(comment.anchor).toMatchObject({ mark_id: markId, quote: "brown" });
    await expectMark(page, markId ?? "", "brown");
  } finally {
    await alice.close();
  }
});

// A project's document is a document page of its own, outside every issue; the chord is the
// document's, so it works there too.
test("Ctrl+Alt+M comments on a project document", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const created = await createProjectDocument("CORE", {
    content: "# Handbook\n\nThe quick brown fox\n",
    name: "Handbook",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/handbook");
    await expect(documentEditor(page)).toContainText("The quick brown fox");
    await expect(connectedDot(page)).toHaveText("connected");
    const form = composer(page);

    await selectEditorText(page, "quick");
    await page.keyboard.press("Control+Alt+KeyM");
    await form.getByLabel("Comment").fill("Which quick?");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect
      .poll(async () =>
        (await listArtifactComments(created.artifact.id)).map((item) => [
          item.body,
          item.anchor?.quote,
        ])
      )
      .toEqual([["Which quick?", "quick"]]);
  } finally {
    await alice.close();
  }
});

// Playwright's Linux keyboard cannot produce Mac Option's µ character. The iphone project has
// the iPhone user agent, so this raw event is the real browser path for Cmd+Option+M's key shape.
test("Cmd+Option+M comments on a project document on a Mac user agent", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the physical-key fallback is Mac-only");
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: "# Handbook\n\nThe quick brown fox\n",
    name: "Handbook",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/handbook");
    await expect(documentEditor(page)).toContainText("The quick brown fox");
    await expect(connectedDot(page)).toHaveText("connected");
    const form = composer(page);

    await selectEditorText(page, "fox");
    await documentEditor(page).dispatchEvent("keydown", {
      altKey: true,
      bubbles: true,
      cancelable: true,
      code: "KeyM",
      key: "µ",
      metaKey: true,
    });
    await expect(form).toContainText("fox");
    await expect(form.getByLabel("Comment")).toBeFocused();
  } finally {
    await alice.close();
  }
});
