import { expect, type Locator, type Page, test } from "@playwright/test";

import { holdPosts, refusePosts } from "./agents";
import { createComment, createIssue, createMessage, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The phone layout on every project: on a phone a comment's thread opens full-screen over the
// Conversation, and that is where each of these rows goes and comes back.
const phone = { height: 844, width: 390 };
const session = {
  actor: { kind: "session" as const, id: "e2e-phone-conversation" },
  as: "agent" as const,
};

function commentTurn(page: Page, commentId: string): Locator {
  return page.locator(`[data-turn="comment:${commentId}"]`);
}

function threadView(page: Page): Locator {
  return page.getByRole("dialog", { name: "Thread" });
}

async function openThread(page: Page, commentId: string): Promise<Locator> {
  await commentTurn(page, commentId).getByRole("button", { name: "Expand thread" }).click();
  const thread = threadView(page);
  await expect(thread).toBeVisible();
  return thread;
}

async function leaveThread(page: Page): Promise<void> {
  await threadView(page).getByRole("button", { name: "Back" }).click();
  await expect(threadView(page)).toHaveCount(0);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// A send's composer is the one that shows its outcome, and the one on screen: a thread opened over
// it while the message is on its way leaves it where it was, holding the draft until the answer,
// and then it takes the next message.
test("on a phone, a docked send keeps its composer through a thread and back, which then takes typing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread round trip" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("First message");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await openThread(page, earlier.id);
    await leaveThread(page);
    await expect(field).toHaveValue("First message");
    await expect(field).toBeDisabled();

    send.release();
    await expect(field).toHaveValue("");
    await field.click();
    await page.keyboard.type("Second");
    await expect(field).toHaveValue("Second");
  } finally {
    await alice.close();
  }
});

// A refusal is the sending composer's, and a thread opened over it keeps both the refusal and the
// draft it hands back for the reader to find on Back.
test("on a phone, Expand thread while a send is out keeps the draft and its refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread refusal" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Keep this draft");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await openThread(page, earlier.id);
    const refused = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/issues/${issue.key}/comments`) &&
        response.status() === 503
    );
    refuse();
    await refused;
    await leaveThread(page);

    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Keep this draft");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// A comment's Reply on a phone answers in the comment's thread, by a composer of its own. Back
// while that reply is on its way leaves the composer with its thread, and reopening the thread
// finds it, holding the reply until the answer.
test("on a phone, a thread reply keeps its composer through Back, and reopening the thread finds it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await commentTurn(page, earlier.id).getByRole("button", { exact: true, name: "Reply" }).click();
    const thread = threadView(page);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    await expect(form.getByRole("button", { name: "Cancel reply" })).toBeVisible();
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Thread reply");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await leaveThread(page);
    await openThread(page, earlier.id);
    await expect(field).toHaveValue("Thread reply");
    await expect(field).toBeDisabled();

    send.release();
    await expect(thread.getByRole("list", { name: "Replies" })).toContainText("Thread reply");
    const next = thread
      .getByRole("form", { name: "Comment composer" })
      .getByRole("textbox", { name: "Reply" });
    await next.click();
    await page.keyboard.type("And another");
    await expect(next).toHaveValue("And another");
  } finally {
    await alice.close();
  }
});

// Each composer has its own reply: a thread's reply, sent and left behind with Back, neither takes
// the docked composer's reply and draft away when the thread opens nor clears them when it lands.
test("on a phone, a thread reply leaves the docked composer's own reply and draft where they were", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread and docked replies" });
  await createMessage(issue.key, { body: "Agent status" }, session);
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const docked = page.getByRole("form", { name: "Comment composer" });
    const dockedField = docked.getByLabel("Comment");
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li", { hasText: "Agent status" })
      .getByRole("button", { exact: true, name: "Reply" })
      .click();
    await expect(docked.getByText(/^Replying to .+ — Agent status$/)).toBeVisible();
    await dockedField.fill("Docked draft");

    await commentTurn(page, earlier.id).getByRole("button", { exact: true, name: "Reply" }).click();
    const thread = threadView(page);
    const threadForm = thread.getByRole("form", { name: "Comment composer" });
    await expect(threadForm.getByText(/^Replying to .+ — Earlier comment$/)).toBeVisible();
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await threadForm.getByLabel("Comment").fill("Thread reply");
    await threadForm.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(threadForm.getByLabel("Comment")).toBeDisabled();

    await leaveThread(page);
    await expect(docked.getByText(/^Replying to .+ — Agent status$/)).toBeVisible();
    await expect(dockedField).toHaveValue("Docked draft");

    send.release();
    await openThread(page, earlier.id);
    await expect(thread.getByRole("list", { name: "Replies" })).toContainText("Thread reply");
    await leaveThread(page);
    await expect(docked.getByText(/^Replying to .+ — Agent status$/)).toBeVisible();
    await expect(dockedField).toHaveValue("Docked draft");
  } finally {
    await alice.close();
  }
});

// A thread card's own reply composer names its send as the tab's composers do, so the thread's
// Reply holds while it is out - in Send's own task too - rather than swapping the reply on its
// way for another composer, which on a phone closed the thread.
test("on a phone, a thread's own reply holds the thread's Reply while it is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread inline reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, earlier.id);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const rootReply = thread.getByRole("button", { exact: true, name: "Reply" });
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Inline reply");
    const replyButton = await rootReply.elementHandle();
    await form.evaluate((node, reply) => {
      const sendButton = node.querySelector<HTMLButtonElement>('button[type="submit"]');
      if (sendButton === null || !(reply instanceof HTMLButtonElement))
        throw new Error("expected Send and the thread's Reply");
      sendButton.click();
      reply.click();
    }, replyButton);

    await expect(thread).toBeVisible();
    await expect(field).toHaveValue("Inline reply");
    await expect(field).toBeDisabled();
    await expect(rootReply).toBeDisabled();

    send.release();
    await expect(thread.getByRole("list", { name: "Replies" })).toContainText("Inline reply");
    await expect(field).toHaveValue("");
    await expect(rootReply).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// A thread card's own reply composer lives in the thread view, so the view holds open while its
// reply is out - Back and Escape both - and the reader sees the reply's refusal and draft there.
test("on a phone, a thread's own reply holds Back and Escape while it is out, and its refusal stays in the thread", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone thread inline refusal" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, earlier.id);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const back = thread.getByRole("button", { name: "Back" });
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Inline reply");
    const backButton = await back.elementHandle();
    await form.evaluate((node, backControl) => {
      const sendButton = node.querySelector<HTMLButtonElement>('button[type="submit"]');
      if (sendButton === null || !(backControl instanceof HTMLButtonElement))
        throw new Error("expected Send and the thread's Back");
      sendButton.click();
      backControl.click();
    }, backButton);

    await expect(thread).toBeVisible();
    await expect(field).toBeDisabled();
    await expect(back).toBeDisabled();
    // Escape from outside the composer is the thread dialog's own close.
    await thread.getByRole("button", { name: /^Copy reference/ }).focus();
    await page.keyboard.press("Escape");
    await expect(thread).toBeVisible();

    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Inline reply");
    await expect(back).toBeEnabled();
  } finally {
    await alice.close();
  }
});
