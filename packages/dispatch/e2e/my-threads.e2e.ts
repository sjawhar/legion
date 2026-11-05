import { expect, test } from "@playwright/test";

import { createAsk, createComment, createIssue, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test("My threads lists all participation, keeps read threads and follows newest activity @phone", async ({
  browser,
}, testInfo) => {
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Participating in threads" });
  const first = await createComment(issue.key, { body: "The first thread I wrote in." });
  const second = await createComment(issue.key, { body: "Another thread I wrote in." });
  await createComment(issue.key, { body: "Not Alice's thread." }, { login: "bob" });
  await createComment(
    issue.key,
    { body: "Latest activity in the first thread.", reply_to: first.id },
    { login: "bob" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/threads");
    await expect(page.getByRole("heading", { name: "My threads", exact: true })).toBeVisible();
    const rows = page.getByRole("list", { name: "My threads" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0)).toContainText("The first thread I wrote in.");
    await expect(rows.nth(1)).toContainText("Another thread I wrote in.");
    await expect(rows.nth(0).getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${first.id}`
    );
    await expect(rows.nth(1).getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${second.id}`
    );
    await page.screenshot({ path: testInfo.outputPath("my-threads.png"), fullPage: true });
    await rows.nth(0).getByRole("link", { name: "Open" }).click();
    await expect(page).toHaveURL(`/issues/${issue.key}/comments/${first.id}`);
    await page.goto("/threads");
    await expect(rows).toHaveCount(2);
  } finally {
    await alice.close();
  }
  const bob = await asUser(browser, "bob");
  try {
    const page = await bob.newPage();
    await page.goto("/threads");
    const rows = page.getByRole("list", { name: "My threads" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    await expect(rows.filter({ hasText: "Another thread I wrote in." })).toHaveCount(0);
  } finally {
    await bob.close();
  }
});

test("My threads includes mentioned and addressed roots before the person writes in them @phone", async ({
  browser,
}) => {
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "New participation" });
  await patchIssue(issue.key, { assignee: "alice" });
  const mention = await createComment(
    issue.key,
    { body: "A thread for [Alice](dispatch://people/alice)." },
    { login: "bob" }
  );
  const ask = await createAsk(
    issue.key,
    { question: "An addressed thread without Alice writing." },
    { login: "bob" }
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/threads");
    const rows = page.getByRole("list", { name: "My threads" }).getByRole("listitem");
    await expect(rows).toHaveCount(2);
    const mentioned = rows.filter({ hasText: "A thread for Alice." });
    await expect(mentioned).toContainText("Mentioned");
    await expect(mentioned.getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/comments/${mention.id}`
    );
    const addressed = rows.filter({ hasText: "An addressed thread without Alice writing." });
    await expect(addressed).toContainText("Addressed");
    await expect(addressed.getByRole("link", { name: "Open" })).toHaveAttribute(
      "href",
      `/issues/${issue.key}/asks/${ask.id}`
    );
  } finally {
    await alice.close();
  }
});
