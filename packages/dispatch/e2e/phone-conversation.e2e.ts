import { expect, type Locator, type Page, test } from "@playwright/test";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";
import { DWELL_MS } from "../web/src/features/shell/useDwell";
import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  getComment,
  getIssue,
  getMyState,
  patchIssue,
  resolveComment,
} from "./api";
import { expectUncovered } from "./layout";
import { resetDatabase } from "./seed";
import { answeredPost, holdPosts, refusePosts, WRITE_REFUSED, WRITE_REFUSED_TEXT } from "./sends";
import { asUser } from "./users";

// The phone layout on every project: on a phone a comment's thread opens full-screen over the
// Conversation, and that is where each of these rows goes and comes back.
const phone = { height: 844, width: 390 };
const session = {
  actor: { kind: "session" as const, id: "e2e-phone-conversation" },
  as: "agent" as const,
};

/** Every fact these rows poll for lands in milliseconds: Playwright's default 100/250/500/1000
 *  ladder would first see it a quarter-second late. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

/** Spends the live socket's reconnect delay - a draw the attempt count grows
 *  (`web/src/api/live-socket.ts`) - on the page's own clock, until the pill is gone. */
async function reconnected(page: Page, pill: Locator): Promise<void> {
  await expect
    .poll(async () => {
      await page.clock.fastForward(RECONNECT_SPREAD_MS);
      return pill.count();
    }, PROBE)
    .toBe(0);
}

/** Holds the page's clock still, so a dwell cannot be spent by the time a row's next step takes.
 *  `install()` replaces the page's timers without stopping them - it leaves the clock running at
 *  real speed - and `pauseAt` is what stops it; from there only a `runFor` spends page time
 *  (`create-issue-related.e2e.ts:32` pauses the same way). It takes a time on the page's own
 *  clock, which the Node process's `Date.now()` can be behind, and a clock refuses to move
 *  backwards, so the lead of a second is past any skew. It fires every page timer due inside
 *  that jump, so every call site below makes it before the page is loaded, where none is armed. */
async function pauseClock(page: Page): Promise<void> {
  await page.clock.pauseAt(Date.now() + 1000);
}

/** Spends a paused clock a millisecond at a time until `locator` matches `count` elements. A
 *  page whose clock is paused draws no answer of its own accord: TanStack batches every query's
 *  delivery on a zero-delay page timer, and a paused clock fires no timer at all. A millisecond
 *  of page time fires those and leaves the dwell's second, which is what the rows below hold
 *  still, as good as unspent: a page that needs thirty of these steps to draw has spent 30 ms. */
async function drawn(locator: Locator, count: number): Promise<void> {
  await expect
    .poll(async () => {
      await locator.page().clock.runFor(1);
      return locator.count();
    }, PROBE)
    .toBe(count);
}

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

/** The page's network, taken away and given back. While it is away every opening of the live
 *  socket is refused, as a real outage refuses it: Playwright's WebKit keeps a WebSocket's
 *  handshake on the network through `setOffline`, so without the refusal the page's next opening,
 *  which the backoff makes within the first attempt's spread, succeeds there while the page is
 *  offline and the pill leaves. Routed before the page loads, so every opening goes through it. */
async function pageNetwork(
  page: Page
): Promise<{ offline: () => Promise<void>; online: () => Promise<void> }> {
  let away = false;
  await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
    if (away) {
      await route.close({ code: 1006, reason: "the network is down" });
      return;
    }
    route.connectToServer();
  });
  return {
    offline: async () => {
      away = true;
      await page.context().setOffline(true);
    },
    online: async () => {
      away = false;
      await page.context().setOffline(false);
    },
  };
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
test("on a phone, a docked send keeps its composer through a thread and back, which then takes typing @phone", async ({
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
test("on a phone, Expand thread while a send is out keeps the draft and its refusal @phone", async ({
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
        response.status() === WRITE_REFUSED.status
    );
    refuse();
    await refused;
    await leaveThread(page);

    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(field).toHaveValue("Keep this draft");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// A comment's Reply on a phone answers in the comment's thread, by a composer of its own. Back
// while that reply is on its way leaves the composer with its thread, and reopening the thread
// finds it, holding the reply until the answer.
test("on a phone, a thread reply keeps its composer through Back, and reopening the thread finds it @phone", async ({
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
test("on a phone, a thread reply leaves the docked composer's own reply and draft where they were @phone", async ({
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
test("on a phone, a thread's own reply holds the thread's Reply while it is out @phone", async ({
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
test("on a phone, a thread's own reply holds Back and Escape while it is out, and its refusal stays in the thread @phone", async ({
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
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(field).toHaveValue("Inline reply");
    await expect(back).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// A comment's Reply on a phone answers in the thread by the thread view's own composer, which
// takes the place of the card's reply composer. The card's composer stays mounted, hidden, so a
// refusal it holds - the draft it handed back and the reason - is there again once that reply ends.
test("on a phone, a thread's Reply over its card's refused reply keeps that draft and refusal for after @phone", async ({
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
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();

    await thread.getByRole("button", { exact: true, name: "Reply" }).click();
    const threadForm = thread.getByRole("form", { name: "Comment composer" });
    await expect(threadForm.getByText(/^Replying to .+ — Earlier comment$/)).toBeVisible();
    await expect(field).toHaveCount(0);
    await threadForm.getByRole("button", { name: "Cancel reply" }).click();

    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(field).toHaveValue("Card reply");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// The thread composer's refusal is the thread's while it shows: Back, Escape from the thread and
// Escape in the composer's field each drop it, its draft with it, as they drop the card's own
// reply's, so neither the thread opened again nor the next Reply brings it back.
test("on a phone, Back and Escape from a thread reply's refusal drop its draft and refusal @phone", async ({
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
    const refusal = thread.getByText(WRITE_REFUSED_TEXT);
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
test("on a phone, the reader's own Resolve keeps a thread reply's refusal and its comment @phone", async ({
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
    const refusal = thread.getByText(WRITE_REFUSED_TEXT);
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
test("on a phone, a thread reply refused after Back comes back in its own thread on another Reply @phone", async ({
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
    const refused = answeredPost(
      page,
      `/api/v1/issues/${issue.key}/comments`,
      WRITE_REFUSED.status
    );
    refuse();
    await refused;

    await commentTurn(page, second.id).getByRole("button", { exact: true, name: "Reply" }).click();
    await expect(thread.locator(`[data-turn="comment:${first.id}"]`)).toBeVisible();
    await expect(thread.locator(`[data-turn="comment:${second.id}"]`)).toHaveCount(0);
    await expect(thread.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(field).toHaveValue("Meant for the first");
  } finally {
    await alice.close();
  }
});

// A send from the thread view keeps it open past the phone layout: while that send is out the
// view stays full-screen at any width, with its card's reply composer - the draft, the send and
// that send's refusal - until the reader leaves it with Back.
test("on a phone, a thread's own reply out when the viewport widens keeps its thread, draft and refusal @phone", async ({
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
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
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
test("on a phone, a thread resolved while its own reply is out stays open with the reply and its refusal @phone", async ({
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
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(field).toHaveValue("Reply under a resolve");
    await expect(thread.getByRole("button", { name: "Back" })).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// Back, Escape and the thread's own controls wait for a send from the thread, so a request the
// server never answers must not keep the reader there: past the client's deadline they let go,
// and Back takes the reader out. The send is not refused - the server may still take it - so the
// thread offers no Retry, and the draft stays held. Each attempt is held to KEYED_SEND_ATTEMPT_MS
// (15 s), so by the deadline the page has asked again, under the write's one key, and once the
// server does answer, it has made the reply once.
test("on a phone, a thread reply the server never answers lets Back go at the deadline @phone", async ({
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
    const keys = send.keys();
    expect(keys.length).toBeGreaterThan(1);
    expect(new Set(keys).size).toBe(1);
    await back.click();
    await expect(threadView(page)).toHaveCount(0);

    const landed = answeredPost(page, `/api/v1/issues/${issue.key}/comments`, 201);
    send.release();
    await landed;
    await send.settled();
    const { replies } = await getComment(earlier.id);
    expect(replies.filter((reply) => reply.body === "Reply into the void")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// On a phone the docked composer sits at the foot of the screen, and a refusal's row grows it
// upward. `Jump to latest` shows while the reader is scrolled down, which is when they reach for
// the refusal's Retry, or a closed issue's Discard draft, so it never sits over the composer.
test("on a phone, Jump to latest covers none of the docked composer's Retry or Discard draft @phone", async ({
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
    // The close below reaches this page on a refresh the scheduler holds for at most
    // `MIN_REFETCH_INTERVAL_MS`: a page timer its own clock fires.
    await page.clock.install();
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
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();

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
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
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

// The shell's status corner floats in the same corner of the same foot of the screen as `Jump to
// latest`: the pending-writes indicator above the connection pill (`app.tsx`), wider than the
// pill and painted over it. So while a write of the reader's is out and the live socket is let
// go, the corner would take the taps a reader scrolled back through the turns means for the
// pill. The corner publishes how far up from the screen's bottom edge it reaches, as a composer
// does (`useFootInset`), and the pill rides above that, so neither the indicator nor the
// connection pill covers any of it.
test("on a phone, the shell's status corner covers none of Jump to latest @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Status corner over the pill" });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Older turn ${index}` }, session);
  }
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    // The return to the network at the end waits a reconnect delay the page draws; its own clock
    // spends that rather than the wall clock.
    await page.clock.install();
    const network = await pageNetwork(page);
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Older turn 14")).toBeVisible();
    const jump = page.getByTestId("jump-to-latest");
    const pill = page.getByTestId("connection-pill");
    const pendingWrites = page.getByRole("region", { name: "Pending changes" });

    // The outage draws the pill, and a pin pressed under it is kept and never answered, so the
    // indicator names it above the pill for as long as this row reads them.
    await network.offline();
    await expect(pill).toHaveText("Reconnecting…");
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ has: page.getByText("Older turn 14", { exact: true }) })
      .getByRole("button", { name: "Pin" })
      .click();
    await expect(pendingWrites).toBeVisible();

    await scrollToOldest(page);
    await expect(jump).toBeVisible();
    await expectUncovered(pill, jump);
    await expectUncovered(pendingWrites, jump);

    await network.online();
    await reconnected(page, pill);
  } finally {
    await alice.close();
  }
});

// Both foot controls at once, which is the reader's ordinary case while the live socket is down:
// the shell's status corner shows and the docked composer sits under it. `Jump to latest` is
// placed above the reach the corner publishes (`--foot-status-inset`, `useFootInset`). That
// reach was once measured only when the corner's own box or the screen changed size, never when
// `--foot-composer-inset` moved the corner. So a corner shown before the composer published its
// reach (the socket refused while the page loads, below), and one the composer's growth moved
// afterwards (a refusal's Retry row), both left the pill placed against a reach the corner had
// left: it dropped down the screen into the docked composer and took the taps meant for Send,
// Retry and the draft. The hook's cascade measures every element it holds again whenever a
// published value changes, and this row is what guards that fix.
test("on a phone, Jump to latest covers none of the docked composer while the status corner shows @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pill over the composer" });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Older turn ${index}` }, session);
  }
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    // The socket alone is down, as a server's restart or a proxy leaves it: the page loads over
    // HTTP and the corner shows its pill from the page's first attempt, before the Conversation
    // has drawn the composer the corner then rides above.
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      await route.close({ code: 1006, reason: "the network is down" });
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Older turn 14")).toBeVisible();
    const pill = page.getByTestId("connection-pill");
    await expect(pill).toHaveText("Reconnecting…");
    const form = page.getByRole("form", { name: "Comment composer" });
    const jump = page.getByTestId("jump-to-latest");

    await scrollToOldest(page);
    await expect(jump).toBeVisible();
    await expectUncovered(jump, form);

    // The other order: the corner is on the page before the composer grows under it, and the
    // refusal's row raises the composer by its own height.
    const comments = `**/api/v1/issues/${issue.key}/comments`;
    const refuse = await refusePosts(page, comments);
    await form.getByLabel("Comment").fill("Kept through every refusal");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();

    await scrollToOldest(page);
    await expect(jump).toBeVisible();
    await expectUncovered(jump, form);
  } finally {
    await alice.close();
  }
});

// The shell's connection pill floats at the foot of the screen, where a phone's docked composer
// sits and grows upward with a refusal's row. While the page is offline the live socket is let go
// and the pill reads `Reconnecting…`; it sits above the composer, on the narrowest phone and a
// common one, so it covers neither Retry, nor Send, nor the line under Send, and it leaves once
// the socket reopens. (The two-minute stuck state is the next row's.)
test("on a phone, the connection pill covers none of the docked composer @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Connection pill over a refusal" });
  const alice = await asUser(browser, "alice");
  const narrow = { height: 568, width: 320 };

  try {
    const page = await alice.newPage();
    const network = await pageNetwork(page);
    await page.setViewportSize(narrow);
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const pill = page.getByTestId("connection-pill");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await form.getByLabel("Comment").fill("A draft\nover\nfour lines\nof text");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();

    await network.offline();
    await expect(pill).toHaveText("Reconnecting…");
    await expectUncovered(pill, form);
    await page.setViewportSize(phone);
    await expectUncovered(pill, form);
    await page.setViewportSize(narrow);
    await expectUncovered(pill, form);

    await network.online();
    await expect(pill).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// A live socket refused for two minutes is stuck: the pill says live updates are not reaching the
// page and offers Reload, while the page keeps trying underneath. It floats where `Reconnecting…`
// does, so it too sits above the docked composer, grown by a refusal's row, and neither it nor its
// Reload covers any of it, on the narrowest phone and a common one.
test("on a phone, the stuck live-updates pill covers none of the docked composer @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stuck pill over a refusal" });
  const alice = await asUser(browser, "alice");
  const narrow = { height: 568, width: 320 };

  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.setViewportSize(narrow);
    // Every opening of the live socket fails, as behind a proxy that drops the upgrade.
    let opens = 0;
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      opens += 1;
      await route.close({ code: 1006, reason: "the opening handshake failed" });
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" });
    const pill = page.getByTestId("connection-pill");
    const refuse = await refusePosts(page, `**/api/v1/issues/${issue.key}/comments`);
    await form.getByLabel("Comment").fill("A draft\nover\nfour lines\nof text");
    await form.getByRole("button", { exact: true, name: "Send" }).click();
    refuse();
    await expect(form.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await expect(pill).toHaveText("Reconnecting…");

    await page.clock.fastForward(120_000);
    await expect(pill).toContainText("Live updates are not reaching this page");
    const reload = pill.getByRole("button", { name: "Reload" });
    await expect(reload).toBeVisible();
    await expect.poll(() => opens).toBeGreaterThan(1);
    await expectUncovered(pill, form);
    await expectUncovered(reload, form);
    await page.setViewportSize(phone);
    await expectUncovered(pill, form);
    await expectUncovered(reload, form);
  } finally {
    await alice.close();
  }
});

// A thread opened full-screen on a phone has its own composers fixed at the foot of the screen:
// the one a comment's Reply opens there, and the thread card's own inline reply. The connection
// pill sits above whichever is on screen, grown by a refusal's row, while the page is offline.
// SLOW(>=2s): two composers each filled and refused, and two rounds offline and back, whose
// reconnects WebKit has to carry on a phone viewport.
test("on a phone, the connection pill covers none of a thread's composers @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Connection pill over a thread" });
  const root = await createComment(issue.key, { body: "Thread root" });
  const alice = await asUser(browser, "alice");
  const narrow = { height: 568, width: 320 };

  try {
    const page = await alice.newPage();
    // Each return to the network below waits a reconnect delay the page draws; its own clock
    // spends that rather than the wall clock.
    await page.clock.install();
    const network = await pageNetwork(page);
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
    await expect(threadForm.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await network.offline();
    await expect(pill).toHaveText("Reconnecting…");
    await expectUncovered(pill, threadForm);
    await network.online();
    await reconnected(page, pill);

    // Cancel reply gives the thread back its card's own reply, which a refusal grows too.
    await threadForm.getByRole("button", { name: "Cancel reply" }).click();
    const cardForm = thread
      .getByRole("form", { name: "Comment composer" })
      .filter({ has: page.getByRole("textbox", { name: "Reply" }) });
    await cardForm.getByRole("textbox", { name: "Reply" }).fill("A reply\nover\nfour lines");
    await cardForm.getByRole("button", { exact: true, name: "Send" }).click();
    await expect(cardForm.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
    await network.offline();
    await expect(pill).toHaveText("Reconnecting…");
    await expectUncovered(pill, cardForm);
    await page.setViewportSize(phone);
    await expectUncovered(pill, cardForm);
    await network.online();
    await reconnected(page, pill);
  } finally {
    await alice.close();
  }
});

// A thread opened full-screen covers the Conversation, and the pill acts on the Conversation's
// place, which the reader cannot see from the thread: it stays off the thread view, where it would
// sit over the thread's own reply composer, and comes back with the Conversation on Back.
test("on a phone, Jump to latest stays off a thread opened over the Conversation @phone", async ({
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

// A thread over an older conversation, opened full-screen, covers the Conversation, and the
// reader cannot see the newest card the issue's read mark is written from: while such a thread
// is open that mark is never written, however long the newest turn sits behind it, and Back
// gives the dwell back, so no issue is left unread for good. A thread over the newest
// conversation is that conversation read and writes the mark from its own card, which is the
// row below. The mark is nothing the Conversation draws, so this row reads it where it is
// kept - the reader's own state on the server, `GET /api/v1/me/state` - and watches the page's
// writes of it.
test("on a phone, a thread over an older conversation holds the issue's read mark until Back @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Read mark under a thread" });
  const earlier = await createComment(issue.key, { body: "Earlier comment" });
  await createMessage(issue.key, { body: "Newest turn" }, session);
  // Nothing here has been read, so both cards carry `New` and a dwell on the newest one - the
  // only card the mark watches - would read the issue through its head.
  const head = (await getIssue(issue.key)).last_seq;
  const mark = async () => (await getMyState())[issue.key]?.last_read_seq ?? 0;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    // The dwell is a page timer, so the page's own clock spends it rather than the run: until it
    // is spent the thread opens over a dwell still counting, as a reader's Expand thread within
    // the first second does. The clock is paused as well as installed, since `install()` leaves
    // it running at real speed: the list's own dwell would otherwise be spent by the time this
    // row has counted the `New` marks and opened the thread, and the mark would be written
    // before the thread the row is about ever covered the list.
    await page.clock.install();
    await pauseClock(page);
    const marks: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
      ) {
        marks.push(request.postData() ?? "");
      }
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await drawn(turns.getByText("New", { exact: true }), 2);

    await openThread(page, earlier.id);
    // Several dwells' worth of the page's clock behind the thread, and then two round trips: the
    // page's own, which answers only once a `PUT` the dwell started had left the page, and the
    // read of the mark itself.
    await page.clock.runFor(DWELL_MS * 3);
    await page.evaluate(() => fetch("/api/v1/me/state").then(({ status }) => status));
    expect(await mark()).toBe(0);
    expect(marks).toEqual([]);

    // The list is the reader's again on Back, and the dwell marks the issue read through its
    // head, as it would have had no thread ever opened.
    await leaveThread(page);
    await expect
      .poll(async () => {
        await page.clock.runFor(DWELL_MS);
        return mark();
      }, PROBE)
      .toBe(head);
  } finally {
    await alice.close();
  }
});

// The other side of the rule: a comment link opens that comment's thread full-screen as the
// page lands on a phone, so a reader following one from the Inbox, a Replies row or a copied
// reference reads the conversation in the thread and never sees the list behind it. A thread
// showing the newest conversation is the issue read through its head, so the mark is written
// from the thread itself rather than waiting for a Back that may never come: `usePhoneThread`
// adds no history entry, so the browser's back leaves the issue, and the list it would have
// been written from is never shown. This row lands on the link from another page, so that back
// leaves the issue altogether and no dwell of the list can write the mark after it.
test("on a phone, a comment link to the newest conversation marks the issue read from its thread @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Read mark from the thread" });
  await createMessage(issue.key, { body: "Older turn" }, session);
  // The link's own comment, made last, so it is the newest conversation: the one card the mark
  // is written from, and the card the thread shows.
  const newest = await createComment(issue.key, { body: "Newest comment" });
  const head = (await getIssue(issue.key)).last_seq;
  const mark = async () => (await getMyState())[issue.key]?.last_read_seq ?? 0;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    // The dwell is a page timer, so the page's own clock spends it rather than the run. The
    // clock is paused as well as installed, since `install()` leaves it running at real speed:
    // the dwell would then be spent while the row was still reading the thread, before the
    // wait for the write below was registered, and that wait would never answer.
    await page.clock.install();
    await pauseClock(page);
    const marks: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
      ) {
        marks.push(request.postData() ?? "");
      }
    });
    // Where the reader comes from, so the back below leaves the issue rather than landing on
    // the Conversation, whose own dwell would write the mark whatever the thread did.
    await page.goto("/projects/CORE/issues");
    const issueLink = page.getByRole("link", { name: new RegExp(issue.key) }).first();
    await drawn(issueLink, 1);
    await expect(issueLink).toBeVisible();

    await page.goto(`/issues/${issue.key}/comments/${newest.id}`);
    const thread = threadView(page);
    await drawn(thread.getByText("Newest comment", { exact: true }), 1);
    await expect(thread).toBeVisible();
    await expect(thread.getByText("Newest comment", { exact: true })).toBeVisible();
    expect(await mark()).toBe(0);

    // The dwell's second, spent in the thread, writes the mark from the card the reader has on
    // screen; the write is awaited here so the back below cannot cut it off.
    const written = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await expect
      .poll(async () => {
        await page.clock.runFor(DWELL_MS);
        return marks.length;
      }, PROBE)
      .toBe(1);
    await written;

    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE\/issues$/);
    expect(await mark()).toBe(head);
  } finally {
    await alice.close();
  }
});

// The mark written from the thread lands about a second after its card is on screen, which is
// the second a reader spends reaching for the thread's own Reply. It moves `lastReadSeq`, and
// that value is in the `unreadBoundary` read's key, so the read re-enters `isPending`, every
// card's `New` unmounts and the thread's rows move up a row - under the press. That is the
// webkit-iphone failure of run 42233316986 (job 127949708013), where Reply moved 24 px between
// the mousedown and the mouseup, the click went to the list behind the dialog and the thread's
// composer never opened. Whichever way the mark is written, nothing the reader is pressing may
// move under the press. The write is held here and let go under the press, so the race is run
// rather than waited for, and the row asserts the write was sent at all: a press with nothing
// held would run no race and pass whatever the page did.
test("on a phone, the newest conversation's read mark moves nothing under a press in its thread @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Read mark under a press" });
  // The one conversation, so the thread the reader opens is the newest one.
  const root = await createComment(issue.key, { body: "Thread root" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    let sent = false;
    await page.route(`**/api/v1/me/issues/${issue.key}/state`, async (route) => {
      if (route.request().method() !== "PUT") return route.fallback();
      sent = true;
      await held;
      return route.fallback();
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    const thread = await openThread(page, root.id);
    const reply = thread.getByRole("button", { exact: true, name: "Reply" });
    await expect(reply).toBeVisible();
    // The dwell's own second, spent on the wall clock, after which the route holds the write.
    // Nothing below is a race unless that write was sent, so the row asserts it rather than
    // falling through when it never comes.
    await expect.poll(() => sent).toBe(true);

    const before = await reply.boundingBox();
    if (before === null) throw new Error("the thread's Reply has no box");
    await page.mouse.move(before.x + before.width / 2, before.y + before.height / 2);
    await page.mouse.down();
    // The held write, let go under the press, and its answer, which is what moves the rows: the
    // box below is read once that answer has landed rather than after a guess at how long it
    // would take.
    const written = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    release();
    await written;
    // Three frames of the page's own: the task its `fetch` resolves in, the render React queues
    // for that answer, and the layout that render makes.
    await page.evaluate(
      () =>
        new Promise((done) =>
          requestAnimationFrame(() => requestAnimationFrame(() => requestAnimationFrame(done)))
        )
    );
    const under = await reply.boundingBox();
    await page.mouse.up();

    expect(under, "the thread's Reply moved under the reader's press").toEqual(before);
    await expect(
      thread
        .getByRole("form", { name: "Comment composer" })
        .filter({ has: page.getByRole("textbox", { name: "Comment" }) })
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

/** Sweeps the band from the top of the page down to `end` in the page: at every 4 px step it
 *  scrolls, lets two frames pass so the pill's own scroll listener and the render React queues
 *  for it have both landed, and records where a tap at the centre of a control does not reach
 *  it as it should: a tab must take its own tap, and no control may lose its tap to `Jump to
 *  latest`. Every control above the turns is tapped - the issue's header, its tabs and the All
 *  / Open / Closed filter - and inside the turns the two that decide a suggestion, Accept and
 *  Reject, which are what the pill took while it sat at the head of the list. `end` is the
 *  bottom of the newest card, so that card is the one card the band carries, and it is swept
 *  from where it lands under the controls up off the top of the screen: no card crosses the
 *  foot band here, where the pill does cover a card's Accept and Reject for as long as the
 *  card is passing. The pill floats at the foot now, where the list scrolls behind it as it
 *  does behind any floating control, so a turn's quiet actions - Copy reference, Reply, Pin -
 *  pass under it and are not tapped.
 *  Controls whose centre is off screen, or clipped out of view by a scrolling ancestor (the
 *  issue tablist scrolls sideways at this width, so a tab past its edge is not on screen to tap
 *  at all), are not tapped either. A step away from where the page settled that draws no pill is
 *  recorded too. */
async function tapsLostAcrossBand(
  page: Page,
  band: { end: number; settled: number }
): Promise<string[]> {
  return page.evaluate(async ({ end, settled }) => {
    const turns = document.querySelector('[aria-label="Conversation turns"]');
    if (turns === null) throw new Error("expected the Conversation's turns");
    const frame = () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => resolve());
      });
    const pillBox = () =>
      document.querySelector('[data-testid="jump-to-latest"]')?.getBoundingClientRect();
    const lost: string[] = [];
    for (let top = 1; top <= end; top += 4) {
      window.scrollTo(0, top);
      // One frame carries the scroll into layout and runs the pill's listener; a step that
      // changes whether the pill shows takes one more for the render React queued for it.
      await frame();
      const shows = Math.abs(top - settled) > 1;
      if (shows !== (pillBox()?.height ?? 0) > 0) await frame();
      const pill = document.querySelector('[data-testid="jump-to-latest"]');
      if (shows && (pill?.getBoundingClientRect().height ?? 0) === 0) {
        lost.push(`scrolled to ${top}: no Jump to latest`);
        continue;
      }
      const turnsTopNow = turns.getBoundingClientRect().top;
      for (const control of document.querySelectorAll<HTMLElement>(
        '[role="tab"], button, a[href], input'
      )) {
        const box = control.getBoundingClientRect();
        const x = box.left + box.width / 2;
        const y = box.top + box.height / 2;
        const label = control.getAttribute("aria-label");
        const decides = label === "Accept suggestion" || label === "Reject suggestion";
        if (pill?.contains(control) || box.width === 0) continue;
        if (box.bottom > turnsTopNow && !decides) continue;
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
        const name = label ?? (control.closest("label") ?? control).textContent?.trim() ?? "";
        lost.push(
          `scrolled to ${top}: ${name} at ${Math.round(x)},${Math.round(y)} reached ${hit?.outerHTML.slice(0, 80)}`
        );
      }
    }
    return lost;
  }, band);
}

// On a phone the issue's header, its tabs and the filters sit above the turns, the newest turn
// just under them, so a reader scrolling back up passes a band where they are on screen while
// `Jump to latest` shows. The pill is fixed at the foot there, above the docked composer, so
// every control above the turns keeps its own tap through that band, and so does the newest
// card's Accept and Reject, the pair the pill took for itself while it sat at the head of the
// list. The band ends at that card's bottom, so the decisions this row checks are the newest
// card's above the foot band, never a card's crossing it: a card scrolled through the foot
// passes under the pill and loses those two taps for as long as it is there, which is accepted
// - scrolling the card on, or the pill's own tap, which hides the pill, clears it, as it does
// for that card's Reply and Pin.
// SLOW(>=2s): the band is swept 4 px at a time, a frame per step, and every control above the
// turns, with the suggestion's decisions under it, is hit-tested at each one.
test("on a phone, Jump to latest covers none of the issue's tabs, the controls above the turns, or the newest card's Accept and Reject above the foot band @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The quick brown fox",
    title: "Pill over the tabs",
  });
  for (let index = 0; index < 15; index += 1) {
    await createMessage(issue.key, { body: `Older turn ${index}` }, session);
  }
  // The newest turn, so it is the first card under the band: a collapsed suggestion, whose
  // Accept and Reject sit in the content column, where the pill was painted over them.
  await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "quick" },
    body: "Suggested replacement.",
    suggestion: { replace_with: "swift" },
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize(phone);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Older turn 14")).toBeVisible();
    const jump = page.getByTestId("jump-to-latest");
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(page.getByRole("button", { name: "Accept suggestion" })).toBeVisible();
    // Where the page settled on the latest turn, the one place in the band with no pill.
    const settled = await page.evaluate(() => window.scrollY);
    // The band ends where the newest card has gone off the top of the screen - its bottom is
    // the scroll position the sweep stops at - so every control on that card is hit-tested on
    // its way up, not only the header row the card opens with.
    const end = await turns.evaluate((list) => {
      const first = list.firstElementChild;
      if (first === null) throw new Error("expected a first turn");
      const box = first.getBoundingClientRect();
      return box.bottom + window.scrollY;
    });
    // Both ends of the band keep Playwright's own visibility check; the sweep between them runs
    // in the page, one round trip rather than three per step.
    await page.evaluate(() => window.scrollTo(0, 1));
    if (Math.abs(1 - settled) > 1) await expect(jump).toBeVisible();
    const lost = await tapsLostAcrossBand(page, { end, settled });
    await page.evaluate((y) => window.scrollTo(0, y), end);
    if (Math.abs(end - settled) > 1) await expect(jump).toBeVisible();
    expect(lost).toEqual([]);
  } finally {
    await alice.close();
  }
});
