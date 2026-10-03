import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  patchIssue,
  resolveComment,
} from "./api";
import { resetDatabase } from "./seed";
import { holdPosts, refusePosts } from "./sends";
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

/** Fails unless `pill` and `region` come to share no point on screen: the pill covers none of it.
 *  It waits for a layout that settles after a resize. */
async function expectUncovered(pill: Locator, region: Locator): Promise<void> {
  await expect(async () => {
    const covering = await pill.boundingBox();
    const covered = await region.boundingBox();
    expect(covering).not.toBeNull();
    expect(covered).not.toBeNull();
    if (covering === null || covered === null) return;
    const apart =
      covering.x + covering.width <= covered.x ||
      covered.x + covered.width <= covering.x ||
      covering.y + covering.height <= covered.y ||
      covered.y + covered.height <= covering.y;
    expect(apart, `the pill at ${JSON.stringify(covering)} covers ${JSON.stringify(covered)}`).toBe(
      true
    );
  }).toPass({ timeout: 5_000 });
}

/** Scrolls the page to its foot, past the latest turn, as a reader browsing older turns does. */
async function scrollToOldest(page: Page): Promise<void> {
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
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

// A comment's Reply on a phone answers in the thread by the thread view's own composer, which
// takes the place of the card's reply composer. The card's composer stays mounted, hidden, so a
// refusal it holds - the draft it handed back and the reason - is there again once that reply ends.
test("on a phone, a thread's Reply over its card's refused reply keeps that draft and refusal for after", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply over a refused reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, earlier.id);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Card reply");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();
    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();

    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    const threadForm = thread.getByRole("form", { name: "Comment composer" });
    await expect(threadForm.getByText(/^Replying to .+ — Earlier comment$/)).toBeVisible();
    await expect(field).toHaveCount(0);
    await threadForm.getByRole("button", { name: "Cancel reply" }).click();

    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Card reply");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// The thread composer's refusal is the thread's while it shows: Back, Escape from the thread and
// Escape in the composer's field each drop it, its draft with it, as they drop the card's own
// reply's, so neither the thread opened again nor the next Reply brings it back.
test("on a phone, Back and Escape from a thread reply's refusal drop its draft and refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Leaving a refused thread reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = threadView(page);
    const threadForm = thread.getByRole("form", { name: "Comment composer" });
    const field = threadForm.getByRole("textbox", { name: "Comment" });
    const refusal = thread.getByText("Couldn't send — the server is down");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    refuse();
    const sendRefused = async (draft: string) => {
      await expect(field).toHaveValue("");
      await field.fill(draft);
      await threadForm.getByRole("button", { exact: true, name: "Send" }).click();
      await expect(refusal).toBeVisible();
      await expect(field).toHaveValue(draft);
    };
    const expectDropped = async () => {
      await expect(field).toHaveCount(0);
      await expect(refusal).toHaveCount(0);
      await expect(threadForm.getByRole("textbox", { name: "Reply" })).toHaveValue("");
    };

    await commentTurn(page, earlier.id).getByRole("button", { exact: true, name: "Reply" }).click();
    await sendRefused("Refused, then Back");
    await leaveThread(page);
    await openThread(page, earlier.id);
    await expectDropped();

    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    await sendRefused("Refused, then Escape");
    // Escape from outside the composer is the thread dialog's own close.
    await thread.getByRole("button", { name: /^Copy reference/ }).focus();
    await page.keyboard.press("Escape");
    await expect(threadView(page)).toHaveCount(0);
    await openThread(page, earlier.id);
    await expectDropped();

    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    await sendRefused("Refused, then Escape in the field");
    await field.press("Escape");
    await expect(thread).toBeVisible();
    await expectDropped();
    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    await expect(field).toHaveValue("");
    await expect(refusal).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A send from the thread view keeps it open past the phone layout: while that send is out the
// view stays full-screen at any width, with its card's reply composer - the draft, the send and
// that send's refusal - until the reader leaves it with Back.
test("on a phone, a thread's own reply out when the viewport widens keeps its thread, draft and refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Widened under a reply" });
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
    await field.fill("Reply through a widening");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await page.setViewportSize({ height: 900, width: 1280 });
    await expect(thread).toBeVisible();
    await expect(field).toHaveValue("Reply through a widening");
    await expect(back).toBeDisabled();

    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Reply through a widening");
    await expect(back).toBeEnabled();
    await back.click();
    await expect(threadView(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A thread the reader has open stays open whoever resolves its comment, so its own reply - the
// draft, the send it has out and that send's refusal - stays in front of them.
test("on a phone, a thread resolved while its own reply is out stays open with the reply and its refusal", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Resolved under a phone reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, earlier.id);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Reply under a resolve");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();

    await resolveComment(earlier.id, { login: "bob" });
    await expect(thread.getByText(/^Resolved by bob/)).toBeVisible();
    await expect(field).toHaveValue("Reply under a resolve");

    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Reply under a resolve");
    await expect(thread.getByRole("button", { name: "Back" })).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// Back, Escape and the thread's own controls wait for a send from the thread, so a request the
// server never answers must not keep the reader there: past the client's deadline they let go,
// and Back takes the reader out. The send is not refused - the server may still take it - so the
// thread offers no Retry, and the draft stays held.
test("on a phone, a thread reply the server never answers lets Back go at the deadline", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Hung thread reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, earlier.id);
    const form = thread.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    const back = thread.getByRole("button", { name: "Back" });
    // Held for good: the server never answers this one.
    const send = await holdPosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await field.fill("Reply into the void");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();
    await expect(back).toBeDisabled();

    await page.clock.fastForward(29_000);
    await expect(back).toBeDisabled();
    await page.clock.fastForward(1_500);
    await expect(form.getByText(/^Still sending/)).toBeVisible();
    await expect(form.getByRole("button", { name: "Retry" })).toHaveCount(0);
    await expect(field).toHaveValue("Reply into the void");
    await expect(field).toBeDisabled();
    await expect(back).toBeEnabled();
    expect(send.posts()).toBe(1);
    await back.click();
    await expect(threadView(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// On a phone the docked composer sits at the foot of the screen, and a refusal's row grows it
// upward. `Jump to latest` shows while the reader is scrolled down, which is when they reach for
// the refusal's Retry, or a closed issue's Discard draft, so it never sits over the composer.
test("on a phone, Jump to latest covers none of the docked composer's Retry or Discard draft", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pill over a refusal" });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Older turn ${index}` }, session);
  }
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Older turn 14")).toBeVisible();
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    const jump = page.getByTestId("jump-to-latest");
    const comments = `**/api/v1/issues/${issue.key}/comments`;
    const refuse = await refusePosts(page, comments);
    await field.fill("Kept through every refusal");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();

    await scrollToOldest(page);
    await expect(jump).toBeVisible();
    await expectUncovered(jump, form);
    // Retry, pressed while the pill shows, goes out; the issue closes under it, and the server's
    // refusal leaves Discard draft in its place.
    await page.unroute(comments);
    const send = await holdPosts(page, comments);
    await form.getByRole("button", { name: "Retry" }).click();
    await expect(field).toBeDisabled();
    expect(send.posts()).toBe(1);
    await patchIssue(issue.key, { status: "done" }, { login: "bob" });
    await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
    send.release();
    await expect(form.getByText("Couldn't send — issue is closed")).toBeVisible();

    await scrollToOldest(page);
    await expect(jump).toBeVisible();
    await expectUncovered(jump, form);
    await form.getByRole("button", { name: "Discard draft" }).click();
    await expect(form).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The shell's connection pill floats at the foot of the screen, where a phone's docked composer
// sits and grows upward with a refusal's row. In both of its states it sits above the composer,
// on the narrowest phone and a common one, so it covers neither Retry, nor Send, nor the line
// under Send.
test("on a phone, the connection pill covers none of the docked composer", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Connection pill over a refusal" });
  const alice = await asUser(browser, "alice");
  const narrow = { height: 568, width: 320 };

  try {
    const page = await alice.newPage();
    await page.setViewportSize(narrow);
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const pill = page.getByTestId("connection-pill");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await form.getByLabel("Comment").fill("A draft\nover\nfour lines\nof text");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(form.getByText("Couldn't send — the server is down")).toBeVisible();

    await alice.setOffline(true);
    await expect(pill).toHaveText("Reconnecting…");
    await expectUncovered(pill, form);
    await page.setViewportSize(phone);
    await expectUncovered(pill, form);

    // A stream the server refuses for good: the pill offers a reload instead.
    await page.route(/\/api\/v1\/events(\?|$)/, (route) =>
      route.fulfill({ body: "{}", contentType: "application/json", status: 410 })
    );
    await alice.setOffline(false);
    await expect(pill).toContainText("Live updates unavailable");
    await expectUncovered(pill, form);
    await page.setViewportSize(narrow);
    await expectUncovered(pill, form);
  } finally {
    await alice.close();
  }
});

// A thread opened full-screen on a phone has its own composers fixed at the foot of the screen:
// the one a comment's Reply opens there, and the thread card's own inline reply. The connection
// pill sits above whichever is on screen, grown by a refusal's row, in both of the pill's states.
test("on a phone, the connection pill covers none of a thread's composers", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Connection pill over a thread" });
  const root = await createComment(issue.key, { body: "Thread root" });
  const alice = await asUser(browser, "alice");
  const narrow = { height: 568, width: 320 };

  try {
    const page = await alice.newPage();
    await page.setViewportSize(narrow);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, root.id);
    const pill = page.getByTestId("connection-pill");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);

    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    const threadForm = thread
      .getByRole("form", { name: "Comment composer" })
      .filter({ has: page.getByRole("textbox", { name: "Comment" }) });
    await threadForm.getByLabel("Comment").fill("A draft\nover\nfour lines\nof text");
    await threadForm.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(threadForm.getByText("Couldn't send — the server is down")).toBeVisible();
    await alice.setOffline(true);
    await expect(pill).toHaveText("Reconnecting…");
    await expectUncovered(pill, threadForm);

    // A stream the server refuses for good: the pill offers a reload instead.
    await page.route(/\/api\/v1\/events(\?|$)/, (route) =>
      route.fulfill({ body: "{}", contentType: "application/json", status: 410 })
    );
    await alice.setOffline(false);
    await expect(pill).toContainText("Live updates unavailable");
    await expectUncovered(pill, threadForm);

    // Cancel reply gives the thread back its card's own reply, which a refusal grows too.
    await threadForm.getByRole("button", { name: "Cancel reply" }).click();
    const cardForm = thread
      .getByRole("form", { name: "Comment composer" })
      .filter({ has: page.getByRole("textbox", { name: "Reply" }) });
    await cardForm.getByRole("textbox", { name: "Reply" }).fill("A reply\nover\nfour lines");
    await cardForm.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(cardForm.getByText("Couldn't send — the server is down")).toBeVisible();
    await expectUncovered(pill, cardForm);
    await page.setViewportSize(phone);
    await expectUncovered(pill, cardForm);
  } finally {
    await alice.close();
  }
});

// A thread opened full-screen covers the Conversation, and the pill acts on the Conversation's
// place, which the reader cannot see from the thread: it stays off the thread view, where it would
// sit over the thread's own reply composer, and comes back with the Conversation on Back.
test("on a phone, Jump to latest stays off a thread opened over the Conversation", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pill over a thread" });
  const oldest = await createComment(issue.key, { body: "Oldest comment" });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Newer turn ${index}` }, session);
  }
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Newer turn 14")).toBeVisible();
    const jump = page.getByTestId("jump-to-latest");
    await scrollToOldest(page);
    await expect(jump).toBeVisible();

    await openThread(page, oldest.id);
    await expect(jump).toHaveCount(0);
    await leaveThread(page);
    await scrollToOldest(page);
    await expect(jump).toBeVisible();
  } finally {
    await alice.close();
  }
});

/** Where a tap at the centre of each control above the turns - the issue's header, its tabs and
 *  the filters - lands, for each one it does not reach as it should: a tab must take its own tap,
 *  and no control may lose its tap to `Jump to latest`. Controls whose centre is off screen are
 *  not tapped. */
async function tapsLostAboveTurns(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const turns = document.querySelector('[aria-label="Conversation turns"]');
    if (turns === null) throw new Error("expected the Conversation's turns");
    const pill = document.querySelector('[data-testid="jump-to-latest"]');
    const turnsTop = turns.getBoundingClientRect().top;
    const lost: string[] = [];
    for (const control of document.querySelectorAll<HTMLElement>(
      '[role="tab"], button, a[href], input'
    )) {
      const box = control.getBoundingClientRect();
      const x = box.left + box.width / 2;
      const y = box.top + box.height / 2;
      if (pill?.contains(control) || box.width === 0 || box.bottom > turnsTop) continue;
      if (x < 0 || x >= window.innerWidth || y < 0 || y >= window.innerHeight) continue;
      const hit = document.elementFromPoint(x, y);
      const reached = hit !== null && (control.contains(hit) || hit.contains(control));
      const lostToPill = hit !== null && pill?.contains(hit) === true;
      if (reached || (control.getAttribute("role") !== "tab" && !lostToPill)) continue;
      const name =
        control.getAttribute("aria-label") ??
        (control.closest("label") ?? control).textContent?.trim() ??
        "";
      lost.push(
        `${name} at ${Math.round(x)},${Math.round(y)} reached ${hit?.outerHTML.slice(0, 80)}`
      );
    }
    return lost;
  });
}

// On a phone the issue's header, its tabs and the filters sit above the turns, the latest turn
// just under them, so a reader scrolling back up passes a band where they are on screen while
// `Jump to latest` shows. The pill never rises above where the turns begin, so a tap on a tab, or
// on any control above the turns, reaches it everywhere in that band.
test("on a phone, Jump to latest covers none of the issue's tabs or the controls above the turns", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pill over the tabs" });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Older turn ${index}` }, session);
  }
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Older turn 14")).toBeVisible();
    const jump = page.getByTestId("jump-to-latest");
    const turns = page.getByRole("list", { name: "Conversation turns" });
    // Where the page settled on the latest turn, the one place in the band with no pill.
    const settled = await page.evaluate(() => window.scrollY);
    const turnsTop = await turns.evaluate(
      (list) => list.getBoundingClientRect().top + window.scrollY
    );
    const lost: string[] = [];
    for (let top = 1; top <= turnsTop; top += 4) {
      await page.evaluate((y) => window.scrollTo(0, y), top);
      if (Math.abs(top - settled) > 1) await expect(jump).toBeVisible();
      for (const tap of await tapsLostAboveTurns(page)) lost.push(`scrolled to ${top}: ${tap}`);
    }
    expect(lost).toEqual([]);
  } finally {
    await alice.close();
  }
});
