import { expect, test } from "@playwright/test";

import { agentRow, openAgents, plannerSession, seedAgents } from "./agents";
import { createComment, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { answeredPost, navigateInApp, refusePosts } from "./sends";
import { asUser } from "./users";

// A composer holds its draft and its refusal while its send is out, whatever unmounts it. These
// rows take the reader away from a composer in the middle of its send - another page, a collapsed
// thread - let the server refuse it there, and expect the draft and the refusal back when the
// reader returns to the composer.
const refusedText = "Couldn't send — the server is down";

test.beforeEach(async () => {
  await resetDatabase();
});

test("a docked Conversation send refused while the reader is on the Inbox is there on the return", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Left the issue mid-send" });
  await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const docked = page.getByRole("form", { name: "Comment composer" }).last();
    const field = docked.getByLabel("Comment");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Sent before leaving");
    await docked.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await navigateInApp(page, "/");
    await expect(page.locator("main").getByRole("heading", { name: "Inbox" })).toBeVisible();
    const refused = answeredPost(page, `/api/v1/issues/${issue.key}/comments`, 503);
    refuse();
    await refused;

    await navigateInApp(page, `/issues/${issue.key}/conversation`);
    await expect(
      page.getByRole("heading", { level: 1, name: "Left the issue mid-send" })
    ).toBeVisible();
    const back = page.getByRole("form", { name: "Comment composer" }).last();
    await expect(back.getByText(refusedText)).toBeVisible();
    await expect(back.getByLabel("Comment")).toHaveValue("Sent before leaving");
  } finally {
    await alice.close();
  }
});

test("an Agents row send refused while the reader is on the Inbox is there on the return", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openAgents(page);
    const row = agentRow(page, plannerSession.session_id);
    const field = row.getByRole("textbox", { name: "Comment" });
    const refuse = await refusePosts(page, "**/api/v1/agents/*/messages");
    await page.keyboard.press("j");
    await page.keyboard.press("Enter");
    await expect(field).toBeFocused();
    await page.keyboard.type("Status please");
    await page.keyboard.press("Control+Enter");
    await expect(field).toBeDisabled();

    await page.locator("body").focus();
    await page.keyboard.press("g");
    await page.keyboard.press("i");
    await expect(row).toHaveCount(0);
    const refused = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().includes("/api/v1/agents/") &&
        response.status() === 503
    );
    refuse();
    await refused;
    await page.keyboard.press("g");
    await page.keyboard.press("a");
    const planner = row.getByRole("button", { exact: true, name: "Planner" });
    if ((await planner.getAttribute("aria-expanded")) !== "true") await planner.click();
    await expect(row.getByText(refusedText)).toBeVisible();
    await expect(field).toHaveValue("Status please");
  } finally {
    await alice.close();
  }
});

// At a desktop width a comment's Reply answers the same docked composer and send name a
// message's Reply answers at every width (`ConversationTab.tsx:1402,1422,1456`): a refusal it
// holds for one comment or message must redirect a later Reply onto it, not be silently
// overwritten, the same way the phone thread composer already redirects a Reply on another
// comment (`redirectedReplyTarget` in `held-sends.ts`, read by both `beginReply`s).
test("a docked Reply on a different comment redirects onto the one whose refusal it holds", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "a phone's comment Reply opens its own thread, covered by the thread-composer redirect row"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Two comments, one docked composer" });
  const a = await createComment(issue.key, { body: "Comment A" });
  const b = await createComment(issue.key, { body: "Comment B" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    const field = composer.getByLabel("Comment");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await page
      .locator(`[data-turn="comment:${a.id}"]`)
      .getByRole("button", { name: "Reply" })
      .click();
    await field.fill("Reply meant for A");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();
    const refused = answeredPost(page, `/api/v1/issues/${issue.key}/comments`, 503);
    refuse();
    await refused;
    await expect(composer.getByText(refusedText)).toBeVisible();

    await page
      .locator(`[data-turn="comment:${b.id}"]`)
      .getByRole("button", { name: "Reply" })
      .click();
    await expect(composer.getByText(/Comment A/)).toBeVisible();
    await expect(composer.getByText(/Comment B/)).toHaveCount(0);
    await expect(composer.getByText(refusedText)).toBeVisible();
    await expect(field).toHaveValue("Reply meant for A");
  } finally {
    await alice.close();
  }
});

// Past the send's deadline Collapse thread, and on a phone the thread's Back, let the reader leave
// a thread whose own reply is still out. The reply's composer goes with the thread it is in; the
// send does not, and the thread opened again has its draft and refusal.
test("a thread's own reply refused after the reader left it past the deadline is there when the thread opens again", async ({
  browser,
}, testInfo) => {
  const phone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Left a thread past the deadline" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page.locator(`[data-turn="comment:${earlier.id}"]`);
    const thread = phone ? page.getByRole("dialog", { name: "Thread" }) : turn;
    const openThread = async () => {
      await turn.getByRole("button", { name: "Expand thread" }).click();
      await expect(
        phone ? thread : turn.getByRole("button", { name: "Collapse thread" })
      ).toBeVisible();
    };
    await openThread();
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Reply past the deadline");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await page.clock.fastForward(30_500);
    await expect(form.getByText(/^Still sending/)).toBeVisible();
    if (phone) {
      await thread.getByRole("button", { name: "Back" }).click();
      await expect(thread).toHaveCount(0);
    } else {
      await turn.getByRole("button", { name: "Collapse thread" }).click();
      await expect(turn.getByRole("button", { name: "Expand thread" })).toBeVisible();
    }
    await expect(field).toHaveCount(0);
    const refused = answeredPost(page, `/api/v1/issues/${issue.key}/comments`, 503);
    refuse();
    await refused;

    await openThread();
    await expect(form.getByText(refusedText)).toBeVisible();
    await expect(field).toHaveValue("Reply past the deadline");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});
