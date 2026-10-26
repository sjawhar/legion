import { expect, type Locator, type Page, type Route, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  patchIssue,
  resolveComment,
} from "./api";
import { type Box, boxesApart, expectUncovered } from "./layout";
import { resetDatabase } from "./seed";
import { answeredPost, holdPosts, refusePosts } from "./sends";
import { asUser } from "./users";

// The phone layout on every project: on a phone a comment's thread opens full-screen over the
// Conversation, and that is where each of these rows goes and comes back.
const phone = { height: 844, width: 390 };
// The iPhone 13 viewport INFRA-95 measured the landing fault at. At `phone` (390x844) the
// composer's top sits near y603, below a newest turn left at y≈515 at the top of the page, so a
// landing row could not fail there.
const iphone = { height: 664, width: 390 };
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

/** Scrolls the page to its foot, past the latest turn, as a reader browsing older turns does. */
async function scrollToOldest(page: Page): Promise<void> {
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
}

/** Waits until `card` has finished rendering: visible, then its box unchanged across 30
 *  consecutive samples 50 ms apart, all within 20 s. It only samples: nothing here scrolls,
 *  focuses, taps or hovers, so where the page put the card is where it is measured. */
async function settle(card: Locator, label: string): Promise<void> {
  const deadline = Date.now() + 20_000;
  await expect(card, `${label}: the newest card`).toBeVisible({ timeout: 20_000 });
  let last: Box | null = null;
  let unchanged = 0;
  while (unchanged < 30) {
    if (Date.now() > deadline) {
      throw new Error(
        `${label}: the newest card's box never held still for 30 samples within 20 s (last ${JSON.stringify(last)})`
      );
    }
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 50);
    await tick.promise;
    const box = await card.boundingBox();
    const same =
      box !== null &&
      last !== null &&
      box.x === last.x &&
      box.y === last.y &&
      box.width === last.width &&
      box.height === last.height;
    unchanged = same ? unchanged + 1 : 0;
    last = box;
  }
}

/** Where the page has the newest card now, measured once: whether it is wholly on screen, and
 *  whether it shares no point with the docked composer. */
async function landing(
  page: Page,
  card: Locator
): Promise<{ apart: boolean; inView: boolean; where: string }> {
  const box = await card.boundingBox();
  const composer = await page.getByRole("form", { name: "Comment composer" }).boundingBox();
  const view = await page.evaluate(() => ({
    height: window.innerHeight,
    scrollY: window.scrollY,
    width: window.innerWidth,
  }));
  if (box === null) throw new Error("expected the newest card to have a box");
  if (composer === null) throw new Error("expected the docked composer to have a box");
  const inView =
    box.x >= 0 &&
    box.y >= 0 &&
    box.x + box.width <= view.width &&
    box.y + box.height <= view.height;
  const apart = boxesApart(box, composer);
  const where = `the newest card at ${JSON.stringify(box)}, the composer at ${JSON.stringify(composer)}, the ${view.width}x${view.height} viewport at window.scrollY ${view.scrollY}`;
  return { apart, inView, where };
}

/** One-shot: the newest card is wholly on screen and clear of the docked composer. */
async function expectNewestInView(page: Page, card: Locator, label: string): Promise<void> {
  const { apart, inView, where } = await landing(page, card);
  expect(inView, `${label}: the newest card is not wholly in the viewport: ${where}`).toBe(true);
  expect(apart, `${label}: the newest card shares a point with the composer: ${where}`).toBe(true);
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

// The reader's own Resolve closes the thread once the server takes it, and the thread's reply stays
// theirs: its refusal, its draft and the comment it answers are there when the thread opens again.
test("on a phone, the reader's own Resolve keeps a thread reply's refusal and its comment", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Resolved over a refused reply" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = threadView(page);
    const field = thread
      .getByRole("form", { name: "Comment composer" })
      .getByRole("textbox", { name: "Comment" });
    const refusal = thread.getByText("Couldn't send — the server is down");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    refuse();
    await commentTurn(page, earlier.id).getByRole("button", { exact: true, name: "Reply" }).click();
    await field.fill("Refused, then resolved");
    await thread.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(refusal).toBeVisible();

    await thread.getByRole("button", { exact: true, name: "Resolve" }).click();
    await expect(threadView(page)).toHaveCount(0);
    await expect(commentTurn(page, earlier.id)).toBeVisible();
    await openThread(page, earlier.id);
    await expect(refusal).toBeVisible();
    await expect(field).toHaveValue("Refused, then resolved");
  } finally {
    await alice.close();
  }
});

// The thread composer's refusal is its comment's: left with Back while its send was out, it comes
// back in that comment's thread when the reader replies to another comment, rather than under the
// other comment's Reply.
test("on a phone, a thread reply refused after Back comes back in its own thread on another Reply", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused after Back" });
  const first = await createComment(issue.key, { body: "First comment" });
  const second = await createComment(issue.key, { body: "Second comment" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = threadView(page);
    const field = thread
      .getByRole("form", { name: "Comment composer" })
      .getByRole("textbox", { name: "Comment" });
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await commentTurn(page, first.id).getByRole("button", { exact: true, name: "Reply" }).click();
    await field.fill("Meant for the first");
    await thread.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(field).toBeDisabled();
    await leaveThread(page);
    const refused = answeredPost(page, `/api/v1/issues/${issue.key}/comments`, 503);
    refuse();
    await refused;

    await commentTurn(page, second.id).getByRole("button", { exact: true, name: "Reply" }).click();
    await expect(thread.locator(`[data-turn="comment:${first.id}"]`)).toBeVisible();
    await expect(thread.locator(`[data-turn="comment:${second.id}"]`)).toHaveCount(0);
    await expect(thread.getByText("Couldn't send — the server is down")).toBeVisible();
    await expect(field).toHaveValue("Meant for the first");
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
 *  the All / Open / Closed filter - lands, for each one it does not reach as it should: a tab
 *  must take its own tap, and no control may lose its tap to `Jump to latest`. Controls whose
 *  centre is off screen, or clipped out of view by a scrolling ancestor (the issue tablist
 *  scrolls sideways at this width, so a tab past its edge is not on screen to tap at all), are
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
      let clipped = false;
      for (
        let ancestor = control.parentElement;
        ancestor !== null && !clipped;
        ancestor = ancestor.parentElement
      ) {
        const style = getComputedStyle(ancestor);
        if (style.overflowX === "visible" && style.overflowY === "visible") continue;
        const bounds = ancestor.getBoundingClientRect();
        clipped = x < bounds.left || x > bounds.right || y < bounds.top || y > bounds.bottom;
      }
      if (clipped) continue;
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

// INFRA-303: the Conversation's list is built from three reads, asks, comments and messages, and
// shows nothing until all three answer. The first placement on the newest turn must not be spent
// while one of them is still pending: a newest message known from the messages read before the
// asks read answers otherwise lands the reader at the top of the page, the newest message under
// the docked composer, and the asks answering later changes nothing that brings them back.
test("on a phone, the Conversation opens with its newest message in view whichever read answers last", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Landing on the newest message" });
  await createAsk(issue.key, { question: "Which landing should the reader get?" }, session);
  await createComment(issue.key, { body: "A root comment under the ask" });
  for (let index = 0; index < 8; index += 1) {
    await createMessage(issue.key, { body: `Older message ${index}` }, session);
  }
  // Created last, so it is newer than every ask and comment: the held asks read cannot change
  // which conversation is newest, only whether the list is on the page yet.
  const newest = await createMessage(issue.key, { body: "The newest message lands here" }, session);
  const conversation = `/issues/${issue.key}/conversation`;
  const newestCard = (page: Page) =>
    page.locator(`ol[aria-label="Conversation turns"] [data-turn="message:${newest.id}"]`);
  const alice = await asUser(browser, "alice");

  try {
    // Forced: the messages and comments reads answer while the asks read is held, then it answers.
    const forced = await alice.newPage();
    await forced.setViewportSize(iphone);
    const heldAsks: Route[] = [];
    await forced.route(
      (url) =>
        url.pathname === `/api/v1/issues/${issue.key}/asks` &&
        url.searchParams.get("thread") === "1",
      (route) => {
        heldAsks.push(route);
      }
    );
    const reads = ["messages", "comments"].map((read) =>
      forced.waitForEvent("requestfinished", {
        predicate: (request) =>
          new URL(request.url()).pathname === `/api/v1/issues/${issue.key}/${read}`,
      })
    );
    await forced.goto(conversation);
    await Promise.all(reads);
    await expect.poll(() => heldAsks.length).toBe(1);
    // No in-page signal says the page has seen the partial data, so this fixed margin - two
    // macrotasks, two frames and 300 ms - is the defended choice; the repeat-each red against
    // the unfixed page is its proof.
    await forced.evaluate(async () => {
      for (let i = 0; i < 2; i++) {
        const turn = Promise.withResolvers<void>();
        setTimeout(turn.resolve, 0);
        await turn.promise;
      }
      for (let i = 0; i < 2; i++) {
        const frame = Promise.withResolvers<void>();
        requestAnimationFrame(() => frame.resolve());
        await frame.promise;
      }
      const margin = Promise.withResolvers<void>();
      setTimeout(margin.resolve, 300);
      await margin.promise;
    });
    expect(heldAsks, "the tab made exactly one asks read while it was held").toHaveLength(1);
    await heldAsks[0]?.continue();
    await settle(newestCard(forced), "forced open");
    await expectNewestInView(forced, newestCard(forced), "forced open");
    await forced.close();

    // Unforced: each open a fresh page, the reads answering in whatever order they do.
    let page: Page | undefined;
    for (let open = 1; open <= 20; open += 1) {
      const label = `unforced open ${open}`;
      page = await alice.newPage();
      await page.setViewportSize(iphone);
      // A new page has no history entry to restore a scroll position from: it starts at the top.
      expect(
        await page.evaluate(() => ({ history: history.length, scrollY: window.scrollY })),
        `${label}: the new page before it navigates`
      ).toEqual({ history: 1, scrollY: 0 });
      await page.goto(conversation);
      await settle(newestCard(page), label);
      await expectNewestInView(page, newestCard(page), label);
      if (open < 20) await page.close();
    }

    // The fixture still hides the newest message at the top of the page, which is where the
    // fault leaves the reader; otherwise every assertion above would pass whether or not it did.
    if (page === undefined) throw new Error("expected the last unforced open's page");
    await page.evaluate(() => window.scrollTo(0, 0));
    await settle(newestCard(page), "at the top of the page");
    const top = await landing(page, newestCard(page));
    expect(
      top.inView && top.apart,
      `the fixture no longer hides the newest message at the top of the page, so this row cannot catch the landing fault: ${top.where}`
    ).toBe(false);
  } finally {
    await alice.close();
  }
});
