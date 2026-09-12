import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, listComments } from "./api";
import {
  barAction,
  deleteEditorText,
  documentEditor,
  marginCard,
  selectEditorText,
} from "./editor";
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

test.beforeEach(async () => {
  await resetDatabase();
});

test("a margin comment remains with its block through a reword and quote deletion", async ({
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
    await composer.getByRole("button", { exact: true, name: "Comment" }).click();
    await expect
      .poll(async () => {
        const comments = await listComments(issue.key, issue.primary_artifact_id);
        return comments.find((candidate) => candidate.body === "Keep this precise.");
      })
      .toMatchObject({ anchor: { block_id: expect.any(String), orphaned: false } });
    const comment = (await listComments(issue.key, issue.primary_artifact_id)).find(
      (candidate) => candidate.body === "Keep this precise."
    );
    if (comment === undefined || comment.anchor === null || comment.anchor.block_id === null) {
      throw new Error("The saved comment does not have a block anchor.");
    }

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
        return comments.find((candidate) => candidate.id === comment.id)?.anchor;
      })
      .toMatchObject({ block_id: comment.anchor.block_id, orphaned: false });

    await openReviewPanel(page);
    await expect(marginCard(page, comment.id)).toBeVisible();
    await deleteEditorText(page, "Reworded passage");
    await expect
      .poll(async () => {
        const comments = await listComments(issue.key, issue.primary_artifact_id);
        return comments.find((candidate) => candidate.id === comment.id)?.anchor;
      })
      .toMatchObject({ block_id: comment.anchor.block_id, orphaned: true });
    await marginCard(page, comment.id).getByRole("button").click();
    await expect(marginCard(page, comment.id)).toContainText("Text changed.");
  } finally {
    await alice.close();
  }
});
