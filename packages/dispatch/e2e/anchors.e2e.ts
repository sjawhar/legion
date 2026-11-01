import { expect, type Page, test } from "@playwright/test";
import type { Comment } from "../web/src/api/types";

import { createIssue, createProject, listComments } from "./api";
import { barAction, deleteEditorText, documentEditor, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

async function openReviewPanel(page: Page): Promise<void> {
  const sheet = page.getByTestId("margin-sheet");
  if (
    (await sheet.getAttribute("data-expanded")) !== "true" &&
    (await page.getByRole("button", { name: /Open review panel/ }).count()) > 0
  ) {
    await page.getByRole("button", { name: /Open review panel/ }).click();
  }
}

// The server's anchor state after a live write is read back from Node, so the probe ladder is
// this row's own: the default one first sees a fact true at 120 ms at 350 ms.
const PROBE = { intervals: [50, 50, 100, 200, 500] };

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): one test body on a desktop and a phone layout, each run a document editor, a
// comment anchored to a block, a reword and a deletion - every one a real live write.
test("a block-anchored comment remains with its block through a reword and quote deletion @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Introduction.\n\nTarget passage.\n",
    title: "Block anchored review",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("Target passage.");

    await selectEditorText(page, "Target passage");
    await barAction(page, "Comment");
    const composer = page.getByRole("form", { name: "Comment composer" });
    await composer.getByLabel("Comment").fill("Keep this precise.");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();
    let comment: Comment | undefined;
    await expect
      .poll(async () => {
        const comments = await listComments(issue.key, issue.primary_artifact_id);
        comment = comments.find((candidate) => candidate.body === "Keep this precise.");
        return comment;
      }, PROBE)
      .toMatchObject({ anchor: { block_id: expect.any(String), orphaned: false } });
    if (comment === undefined || comment.anchor === null || comment.anchor.block_id === null) {
      throw new Error("The saved comment does not have a block anchor.");
    }
    const commentId = comment.id;
    const anchoredBlock = comment.anchor.block_id;

    const gutter = page.getByRole("button", { name: "1 references on block" });
    await expect(gutter).toBeVisible();
    await gutter.click();
    await openReviewPanel(page);
    await expect(page.getByText("References for this block")).toBeVisible();
    await selectEditorText(page, "Target passage");
    await page.keyboard.type("Reworded passage");
    await expect(documentEditor(page)).toContainText("Reworded passage");
    await expect
      .poll(async () => {
        const comments = await listComments(issue.key, issue.primary_artifact_id);
        return comments.find((candidate) => candidate.id === commentId)?.anchor;
      }, PROBE)
      .toMatchObject({ block_id: anchoredBlock, orphaned: false });

    await deleteEditorText(page, "Reworded passage");
    await expect
      .poll(async () => {
        const comments = await listComments(issue.key, issue.primary_artifact_id);
        return comments.find((candidate) => candidate.id === commentId)?.anchor;
      }, PROBE)
      .toMatchObject({ block_id: anchoredBlock, orphaned: true });
  } finally {
    await alice.close();
  }
});
