import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions, setSessionSendStatus } from "./agents";
import { createComment, createIssue, createProject, createProjectDocument } from "./api";
import { barAction, documentEditor, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  // The E7b retry assertion resends a steer-mode comment and expects it to succeed - a
  // real live session normally advertises steer, so the fixture must too.
  capabilities: ["aside", "btw", "steer"],
  dir: "/w/planner",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: ["role-x"],
  session_id: "planner",
  title: "Planner",
};

async function selectMention(page: Page, field: Locator, text: string, option: string) {
  await field.fill(text);
  await page
    .getByRole("listbox", { name: "Mention suggestions" })
    .getByRole("option", { name: option })
    .click();
}

async function issueCommentPayload(page: Page, issueKey: string): Promise<Record<string, unknown>> {
  const response = page.waitForResponse(
    (candidate) =>
      candidate.request().method() === "POST" &&
      candidate.url().endsWith(`/api/v1/issues/${issueKey}/comments`) &&
      candidate.status() === 201
  );
  await page
    .getByRole("form", { name: "Comment composer" })
    .getByRole("button", { name: "Send" })
    .click();
  return (await response).request().postDataJSON() as Record<string, unknown>;
}

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("E1 and E2: explicit role mentions deliver once while /btw without a mention stays a plain comment", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Mention behavior" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
    await selectMention(page, field, "/btw @", "role-x");
    expect(await issueCommentPayload(page, issue.key)).toEqual({
      body: "@role-x",
      delivery: "btw",
      mentions: [{ target: "role:role-x" }],
    });
    await expect.poll(() => getSentMessages()).toMatchObject([{ target_session: "planner" }]);

    await page
      .getByRole("form", { name: "Comment composer" })
      .getByLabel("Comment")
      .fill("/btw plain");
    expect(await issueCommentPayload(page, issue.key)).toEqual({ body: "/btw plain" });
  } finally {
    await alice.close();
  }
});

test("E2b: a project-document comment preserves the canonical mention and strips /aside", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", { content: "# Notes\n\nMention this.", name: "Notes" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/notes");
    await expect(documentEditor(page)).toContainText("Mention this.");
    await selectEditorText(page, "Mention this");
    await barAction(page, "Comment");
    const composer = page.getByRole("form", { name: "Comment composer" });
    await selectMention(page, composer.getByLabel("Comment"), "/aside @", "Planner");
    const response = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        /\/api\/v1\/artifacts\/[^/]+\/comments$/.test(candidate.url()) &&
        candidate.status() === 201
    );
    await composer.getByRole("button", { name: "Send" }).click();
    expect((await response).request().postDataJSON()).toMatchObject({
      body: "@Planner",
      delivery: "aside",
      mentions: [{ target: "session:planner" }],
    });
  } finally {
    await alice.close();
  }
});

test("E7b and Retry: removing a prefilled mention creates a plain reply, and retries name their target", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply behavior" });
  const parent = await createComment(issue.key, {
    body: "Parent",
    delivery: "steer",
    mentions: [{ target: "session:planner" }],
  });
  await setSessionSendStatus("planner", 404);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ hasText: "Parent" });
    await card.getByRole("button", { name: "Reply" }).click();
    const composer = page.getByRole("form", { name: "Comment composer" });
    await expect(composer.getByLabel("Comment")).toHaveValue("@Planner");
    await composer.getByLabel("Comment").fill("Plain reply");
    expect(await issueCommentPayload(page, issue.key)).toEqual({
      body: "Plain reply",
      reply_to: parent.id,
    });

    const retry = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith(`/api/v1/comments/${parent.id}/deliveries`)
    );
    await card.getByRole("button", { name: "Retry" }).click();
    expect((await retry).request().postDataJSON()).toEqual({
      delivery: "steer",
      target: "session:planner",
    });
  } finally {
    await alice.close();
  }
});
