import { expect, type Locator, type Page, test } from "@playwright/test";
import {
  type FakeSession,
  getSentMessages,
  holdPosts,
  setLiveSessions,
  setSessionSendStatus,
} from "./agents";
import {
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  retryCommentDelivery,
} from "./api";
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

test("a conversation send holds Reply and refuses Ctrl+K from the same task", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Held conversation" });
  await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    const reply = page.getByRole("button", { name: "Reply" }).first();
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Status please");
    await form.evaluate((node) => {
      const sendButton = node.querySelector<HTMLButtonElement>('button[type="submit"]');
      const field = node.querySelector<HTMLTextAreaElement>('textarea[aria-label="Comment"]');
      if (sendButton === null || field === null) throw new Error("expected conversation controls");
      sendButton.click();
      field.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, ctrlKey: true, key: "k" }));
    });

    await expect(reply).toBeDisabled();
    await expect(page.getByRole("dialog", { name: "Reference picker" })).toHaveCount(0);
    send.release();
    await expect(field).toHaveValue("");
    await expect(page.getByRole("dialog", { name: "Reference picker" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a phone thread keeps Tab inside while its held composer is disabled", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "The full-screen thread dialog is phone-only.");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Held phone thread" });
  await createComment(issue.key, { body: "Thread root" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ hasText: "Thread root" })
      .getByRole("button", { name: "Reply" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Thread" });
    const form = dialog.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Phone reply");
    await form.getByRole("button", { name: "Send" }).click();
    await expect(field).toBeDisabled();

    await dialog.evaluate((node) => {
      const focusable = node.querySelectorAll<HTMLElement>(
        'a[href], button:not(:disabled), textarea:not(:disabled), input:not(:disabled), select:not(:disabled), [tabindex]:not([tabindex="-1"])'
      );
      const last = focusable.item(focusable.length - 1);
      if (last === null) throw new Error("expected an enabled thread control");
      last.focus();
    });
    await page.keyboard.press("Tab");
    await expect
      .poll(() => dialog.evaluate((node) => node.contains(document.activeElement)))
      .toBe(true);
    send.release();
  } finally {
    await alice.close();
  }
});

test("E7b and Retry: removing a prefilled mention creates a plain reply, and retries name their target", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  // The mention's first delivery has to FAIL for a retry to be offered at all: a same-mode
  // retry of a delivery that landed is a repeat that is recognised and dropped, so the row no
  // longer carries a Retry (LEGION-271).
  await setSessionSendStatus("planner", 404);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply behavior" });
  const parent = await createComment(issue.key, {
    body: "Parent",
    delivery: "steer",
    mentions: [{ target: "session:planner" }],
  });
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
    const phoneThread = page.getByRole("dialog", { name: "Thread" });
    const phoneThreadOpen = (await phoneThread.count()) > 0;
    if (phoneThreadOpen) {
      await expect(card.getByRole("button", { name: "Retry" })).toHaveCount(0);
    }
    const retry = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith(`/api/v1/comments/${parent.id}/deliveries`)
    );
    await (phoneThreadOpen ? phoneThread : card).getByRole("button", { name: "Retry" }).click();
    expect((await retry).request().postDataJSON()).toEqual({
      delivery: "steer",
      target: "session:planner",
    });
  } finally {
    await alice.close();
  }
});

// LEGION-271. A mention retried in its own mode inside the stream's duplicate window is one the
// stream already held: it stores nothing new, and the publish still reaches the agent's subject.
// Dispatch records the attempt `sent` with `duplicate`, and the row must say so, since the state
// alone would read as a fresh delivery. The stand-in listener answers `duplicate` for a repeated
// key as the listener does, so this renders the real path.
test("a duplicated mention delivery says the listener already had it, and offers no safe retry", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Duplicate mention" });
  const comment = await createComment(issue.key, {
    body: "@Planner take a look",
    delivery: "steer",
    mentions: [{ target: "session:planner" }],
  });
  // The same mention, the same mode: the stand-in recognises the repeated key exactly as
  // JetStream would and answers `duplicate`.
  await retryCommentDelivery(comment.id, { delivery: "steer", target: "session:planner" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const deliveries = page.getByRole("list", { name: "Mention deliveries" });
    await expect(deliveries).toContainText(
      "Delivered by an earlier attempt, so not delivered again"
    );
    // The promise belongs to a retry that can still be made safely; this attempt already
    // reached the listener, so neither the sentence nor the button may appear for it.
    await expect(deliveries).not.toContainText("Retry won't deliver it twice");
    await expect(deliveries.getByRole("button", { name: "Retry" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
