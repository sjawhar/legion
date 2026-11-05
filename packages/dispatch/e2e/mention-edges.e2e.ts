import { expect, test } from "@playwright/test";
import {
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

test("identity mentions from comments messages and document blocks appear together @phone", async ({
  browser,
}) => {
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Identity mentions" });
  await createComment(issue.key, { body: "Comment for dispatch://people/alice" }, { login: "bob" });
  await createMessage(issue.key, { body: "Message for dispatch://people/alice" }, { login: "bob" });
  await createIssueArtifact(
    issue.key,
    { name: "mentions.md", content: "Document for dispatch://people/alice" },
    { login: "bob" }
  );
  await createComment(issue.key, { body: "Unrelated Bob comment" }, { login: "bob" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/people/alice");
    const rows = page.getByRole("list", { name: "Person nodes" }).getByRole("listitem");
    await expect(rows.filter({ hasText: "Mentioned in" })).toHaveCount(3);
    await expect(rows.filter({ hasText: "Comment for" })).toContainText("Mentioned in");
    await expect(rows.filter({ hasText: "Message for" })).toContainText("Mentioned in");
    await expect(rows.filter({ hasText: "Document for" })).toContainText("Mentioned in");
    await expect(rows.filter({ hasText: "Unrelated Bob" })).toHaveCount(0);
    await rows.filter({ hasText: "Document for" }).getByRole("link", { name: "Open" }).click();
    await expect(page).toHaveURL(/\/issues\/CORE-1\/artifacts\/mentions-md#b-/);
    await expect(page.locator("main")).toContainText("Document for");
    await page.goto("/inbox/unified");
    const inboxRows = page.getByRole("list", { name: "Unified inbox" }).getByRole("listitem");
    await expect(inboxRows.filter({ hasText: "Mention · added to preview" })).toHaveCount(0);
    await page.getByRole("checkbox", { name: "Include mentions (additional items)" }).check();
    await expect(inboxRows.filter({ hasText: "Mention · added to preview" })).toHaveCount(3);
    await expect(inboxRows.filter({ hasText: "Comment for" })).toBeVisible();
    await expect(inboxRows.filter({ hasText: "Message for" })).toBeVisible();
    await expect(inboxRows.filter({ hasText: "Document for" })).toBeVisible();
    await expect(inboxRows.filter({ hasText: "Unrelated Bob" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
