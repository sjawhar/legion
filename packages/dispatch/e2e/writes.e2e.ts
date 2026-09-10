import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject, listComments } from "./api";
import { barAction, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-writes", origin: { tmux: "dispatch:1.5" } },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("a failed pin shows a retryable error, never lies to the sidebar, and recovers on retry", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "WR", name: "Writes" });
  const issue = await createIssue({ project: "WR", title: "Pin me" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  let putAttempts = 0;
  let retryRequests = 0;
  await page.route("**/api/v1/me/issues/*/state", async (route) => {
    if (route.request().method() !== "PUT") {
      await route.continue();
      return;
    }
    putAttempts += 1;
    if (putAttempts === 1) {
      await route.abort();
      return;
    }
    retryRequests += 1;
    await route.continue();
  });

  await page.goto(`/issues/${issue.key}`);
  const header = page.locator("header");
  await expect(page.getByRole("heading", { name: "Pinned" })).toHaveCount(0);

  await header.getByRole("button", { name: "Pin issue" }).click();
  await expect(header.getByRole("alert")).toContainText("Could not save pin status.");
  const retry = header.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("pin-error-retry.png"), fullPage: true });
  // The failed write must never be reflected anywhere: still "Pin issue", still no Pinned group.
  await expect(header.getByRole("button", { name: "Pin issue" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByRole("heading", { name: "Pinned" })).toHaveCount(0);
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Close navigation" }).click();
  }

  // Two synthetic clicks in the same task exercise the retry guard before React can render
  // the button disabled.
  const retryResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      response.url().includes(`/api/v1/me/issues/${issue.key}/state`) &&
      response.ok()
  );
  await retry.evaluate((button: HTMLButtonElement) => {
    button.click();
    button.click();
  });
  await retryResponse;
  await expect.poll(() => retryRequests).toBe(1);
  await expect(header.getByRole("alert")).toHaveCount(0);
  await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();

  await page.goto("/");
  await page.goto(`/issues/${issue.key}`);
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Close navigation" }).click();
  }
  await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();

  await context.close();
});

test("double-clicking Submit posts exactly one comment", async ({ browser }) => {
  await createProject({ key: "WR", name: "Writes" });
  const issue = await createIssue({
    project: "WR",
    spec: "The quick brown fox",
    title: "Double submit",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  await selectEditorText(page, "brown");
  await barAction(page, "Comment");
  const composer = page.getByRole("form", { name: "Comment composer" });
  await composer.getByLabel("Comment").fill("dup check");
  const submit = composer.getByRole("button", { exact: true, name: "Comment" });

  // Two synthetic clicks in the same task reproduce the sub-millisecond double click the
  // audit observed; Playwright's own click() serializes actionability checks and cannot.
  await submit.evaluate((button: HTMLButtonElement) => {
    button.click();
    button.click();
  });

  await expect.poll(() => listComments(issue.key, issue.primary_artifact_id)).toHaveLength(1);
  await expect(composer).toHaveCount(0);

  await context.close();
});

test("Resolve shows a pending state before settling", async ({ browser }, testInfo) => {
  await createProject({ key: "WR", name: "Writes" });
  const issue = await createIssue({
    project: "WR",
    spec: "The quick brown fox",
    title: "Resolve pending",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "brown" }, body: "note" },
    session
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.route("**/api/v1/comments/*/resolve", async (route) => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 500);
    await promise;
    await route.continue();
  });

  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: /Open review panel/ }).click();
  }
  const card = page.getByTestId(`margin-comment-${comment.id}`);
  await card.getByRole("button", { name: "Resolve" }).click();

  // Pending: optimistic flip to "Resolved" plus an explicit in-flight indicator.
  await expect(card.getByRole("status")).toHaveText("Saving…");
  await expect(card.getByText("Resolved", { exact: true })).toBeVisible();
  await expect(card.getByRole("button", { name: "Resolve" })).toHaveCount(0);

  // Done: the in-flight indicator clears once the (delayed) request settles.
  await expect(card.getByRole("status")).toHaveCount(0, { timeout: 2000 });
  await expect(card.getByText("Resolved", { exact: true })).toBeVisible();
  await expect
    .poll(() => listComments(issue.key, issue.primary_artifact_id))
    .toContainEqual(expect.objectContaining({ id: comment.id, resolved: true }));

  await context.close();
});
