import { expect, test } from "@playwright/test";

import { createIssue, createProject, getIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("issue header closes an issue and reopens it into Backlog", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Close from the header" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const close = page.getByRole("button", { name: "Close issue" });
    await expect(close).toBeVisible();
    const closing = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await close.click();
    expect((await closing).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    const reopen = page.getByRole("button", { name: "Reopen issue" });
    await expect(reopen).toHaveAttribute("title", "Reopen into Backlog");
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        closed_at: expect.any(String),
        status: "done",
      });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const done = page.getByRole("region", { name: "Done" });
    await expect(
      done.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();

    await page.goto(`/issues/${issue.key}`);
    const reopening = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await reopen.click();
    expect((await reopening).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toHaveCount(0);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({ closed_at: null, status: "backlog" });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const backlog = page.getByRole("region", { name: "Backlog" });
    await expect(
      backlog.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});
