import { expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, resolveComment } from "./api";
import { documentEditor, marginCard, openedThreadCard, openSpec, setSheet } from "./editor";
import { resetDatabase } from "./seed";
import { answeredPost, navigateInApp, refusePosts } from "./sends";
import { asUser } from "./users";

// A margin thread's own reply holds its draft until the server answers, like every composer.
// These rows put each thing that would move or unmount its card in the middle of the reply's send
// - someone else resolving the thread, the reader opening another one, the reader's own Resolve,
// the Pinned tab, the margin's rail, another page - and expect the draft and the refusal back in
// the thread the reply was sent from.
const spec = "The quick brown fox";
const refused = "Couldn't send — the server is down";

test.beforeEach(async () => {
  await resetDatabase();
});

/** An issue whose spec carries two comment threads, and the route a reply to either posts to. */
async function seed(title: string) {
  await createProject({ key: "HOLD", name: "Margin replies" });
  const issue = await createIssue({ project: "HOLD", spec, title });
  const first = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "On brown",
  });
  const second = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "quick" },
    body: "On quick",
  });
  return { comments: `**/api/v1/issues/${issue.key}/comments`, first, issueKey: issue.key, second };
}

/** The refusal `refusePosts` hands the reply, once the page has it. */
function refusal(page: Page, issueKey: string) {
  return answeredPost(page, `/api/v1/issues/${issueKey}/comments`, 503);
}

/** On a phone the Thread dialog's Back waits for the reply until the send's deadline; past it,
 *  Back takes the reader out of the thread while the reply's send is still out. The page's clock
 *  has to be installed before the page loads. */
async function leavePhoneThreadPastDeadline(page: Page): Promise<void> {
  const back = page.getByRole("dialog", { name: "Thread" }).getByRole("button", { name: "Back" });
  await expect(back).toBeDisabled();
  await page.clock.fastForward(30_500);
  await back.click();
  await expect(page.getByRole("dialog", { name: "Thread" })).toHaveCount(0);
}

/** Opens a margin thread: expanded beside the document, or in the Thread dialog on a phone. */
async function openThread(page: Page, project: string, rootId: string): Promise<Locator> {
  await setSheet(page, project, true);
  await marginCard(page, rootId).getByRole("button", { expanded: false }).click();
  const card = openedThreadCard(page, project, rootId);
  await expect(card).toHaveAttribute("aria-expanded", "true");
  return card;
}

/** Sends `body` from the thread's own reply composer and returns its form and field. */
async function sendReply(card: Locator, body: string): Promise<{ field: Locator; form: Locator }> {
  const form = card.getByRole("form", { name: "Comment composer" });
  const field = form.getByRole("textbox", { name: "Reply" });
  await field.fill(body);
  await form.getByRole("button", { exact: true, name: "Send" }).click();
  await expect(field).toBeDisabled();
  return { field, form };
}

test("a margin thread someone else resolves while its reply is out keeps the reply's draft and refusal", async ({
  browser,
}, testInfo) => {
  const { comments, first, issueKey } = await seed("Resolved under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issueKey, spec);
    const card = await openThread(page, testInfo.project.name, first.id);
    const refuse = await refusePosts(page, comments);
    const { field, form } = await sendReply(card, "Reply under a resolve");

    await resolveComment(first.id, { login: "bob" });
    await expect(card.getByText(/^Resolved by bob/)).toBeVisible();
    await expect(field).toHaveValue("Reply under a resolve");

    refuse();
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply under a resolve");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// The margin expands one thread at a time, so opening another collapses this one: its card keeps
// the reply composer, hidden, for the return. On a phone the thread fills the screen, and its Back
// and Escape wait for the reply instead.
test("leaving a margin thread while its reply is out keeps the reply's draft and refusal for the return", async ({
  browser,
}, testInfo) => {
  const { comments, first, issueKey, second } = await seed("Left under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issueKey, spec);
    const card = await openThread(page, testInfo.project.name, first.id);
    const refuse = await refusePosts(page, comments);
    const { field, form } = await sendReply(card, "Reply left behind");

    if (testInfo.project.name === "iphone") {
      const thread = page.getByRole("dialog", { name: "Thread" });
      await expect(thread.getByRole("button", { name: "Back" })).toBeDisabled();
      await page.keyboard.press("Escape");
      await expect(card).toBeVisible();
      refuse();
      await expect(thread.getByRole("button", { name: "Back" })).toBeEnabled();
    } else {
      await marginCard(page, second.id).getByRole("button", { expanded: false }).click();
      await expect(marginCard(page, second.id)).toHaveAttribute("aria-expanded", "true");
      await expect(card).toHaveAttribute("aria-expanded", "false");
      refuse();
      await card.getByRole("button", { expanded: false }).click();
      await expect(card).toHaveAttribute("aria-expanded", "true");
    }
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply left behind");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// The reader's own Resolve moves a thread to the resolved ones, but not while its reply is out:
// the thread stays, with the reply's draft and its refusal, until the reader drops the reply.
test("the reader's own Resolve while a margin reply is out keeps the reply's draft and refusal", async ({
  browser,
}, testInfo) => {
  const { comments, first, issueKey, second } = await seed("Own resolve under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issueKey, spec);
    const card = await openThread(page, testInfo.project.name, first.id);
    const refuse = await refusePosts(page, comments);
    const { field, form } = await sendReply(card, "Reply under my resolve");

    await card.getByRole("button", { exact: true, name: "Resolve" }).click();
    await expect(card.getByText(/^Resolved by alice/)).toBeVisible();
    await expect(field).toHaveValue("Reply under my resolve");

    refuse();
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply under my resolve");

    // Cancel reply drops the reply, and the resolved thread leaves the open ones, where the other
    // thread still is. (Closing a resolved thread shows the resolved ones on a desktop, so the card
    // may be on screen there.)
    await form.getByRole("button", { name: "Cancel reply" }).click();
    const openCard = (id: string) =>
      page.locator(`section[aria-label="Anchored comments"] [data-margin-item="${id}"]`);
    await expect(openCard(second.id)).toHaveCount(1);
    await expect(openCard(first.id)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The Pinned tab takes the Comments tab's place, its threads' cards with it; the reply is the
// margin's, so the thread finds it again. On a phone the thread fills the screen and Back waits for
// the reply, so the Pinned tab is reachable only once Back lets go at the send's deadline.
test("the Pinned tab while a margin thread's reply is out keeps the reply's draft and refusal", async ({
  browser,
}, testInfo) => {
  const { comments, first, issueKey } = await seed("Pinned under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const project = testInfo.project.name;
    if (project === "iphone") await page.clock.install();
    await openSpec(page, issueKey, spec);
    await openThread(page, project, first.id);
    const refuse = await refusePosts(page, comments);
    await sendReply(openedThreadCard(page, project, first.id), "Reply under the Pinned tab");
    if (project === "iphone") await leavePhoneThreadPastDeadline(page);

    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    await expect(marginCard(page, first.id)).toHaveCount(0);
    const answered = refusal(page, issueKey);
    refuse();
    await answered;
    await margin.getByRole("tab", { name: "Comments" }).click();

    const card =
      project === "iphone"
        ? await openThread(page, project, first.id)
        : openedThreadCard(page, project, first.id);
    await expect(card).toHaveAttribute("aria-expanded", "true");
    const form = card.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply under the Pinned tab");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// Collapsing the desktop margin to its rail takes its tabs off screen, the threads' cards with
// them; the reply is the margin's, so the thread finds it again when the margin comes back.
test("collapsing the margin to its rail while a thread's reply is out keeps the reply's draft and refusal", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "below xl the margin is a sheet, with no rail");
  const { comments, first, issueKey } = await seed("Rail under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await openSpec(page, issueKey, spec);
    const card = await openThread(page, testInfo.project.name, first.id);
    const refuse = await refusePosts(page, comments);
    const { field, form } = await sendReply(card, "Reply under the rail");

    await page.getByRole("button", { name: "Hide margin" }).click();
    await expect(page.getByTestId("margin-rail")).toBeVisible();
    await expect(card).toBeHidden();
    const answered = refusal(page, issueKey);
    refuse();
    await answered;
    await page.getByRole("button", { name: "Show margin" }).click();

    await expect(card).toHaveAttribute("aria-expanded", "true");
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply under the rail");
    await expect(field).toBeEnabled();
  } finally {
    await alice.close();
  }
});

// Leaving the document takes its margin's cards with it; the reply is the margin's, and it holds
// the send while the reader is away, so the thread they come back to has the draft, the refusal
// and a Retry that still answers the thread.
test("leaving the document while a margin thread's reply is out keeps the reply's draft and refusal for the return", async ({
  browser,
}, testInfo) => {
  const { comments, first, issueKey } = await seed("Left under a margin reply");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const project = testInfo.project.name;
    await openSpec(page, issueKey, spec);
    await openThread(page, project, first.id);
    const refuse = await refusePosts(page, comments);
    await sendReply(openedThreadCard(page, project, first.id), "Reply left on another page");

    // The Inbox has no margin at all, so nothing of the thread is on screen there.
    await navigateInApp(page, "/");
    await expect(page.locator("main").getByRole("heading", { name: "Inbox" })).toBeVisible();
    await expect(page.getByRole("form", { name: "Comment composer" })).toHaveCount(0);
    const answered = refusal(page, issueKey);
    refuse();
    await answered;

    // The thread the reader left is open again on their return, with its reply.
    await navigateInApp(page, `/issues/${issueKey}/spec`);
    await expect(documentEditor(page)).toContainText(spec);
    const card = openedThreadCard(page, project, first.id);
    await expect(card).toHaveAttribute("aria-expanded", "true");
    const form = card.getByRole("form", { name: "Comment composer" });
    const field = form.getByRole("textbox", { name: "Reply" });
    await expect(form.getByText(refused)).toBeVisible();
    await expect(field).toHaveValue("Reply left on another page");
    await expect(field).toBeEnabled();

    await page.unroute(comments);
    await form.getByRole("button", { name: "Retry" }).click();
    await expect(
      card.getByRole("list", { name: "Replies" }).getByText("Reply left on another page")
    ).toBeVisible();
    await expect(field).toHaveValue("");
  } finally {
    await alice.close();
  }
});

// A thread opens with its reply field focused, the second time as much as the first: the reader
// expands a thread from the keyboard to type into it.
test("a margin thread expanded a second time from the keyboard has its reply field focused", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "a phone opens a thread in its Thread dialog");
  const { first, issueKey } = await seed("Reopened from the keyboard");
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await openSpec(page, issueKey, spec);
    const card = marginCard(page, first.id);
    const field = card.getByRole("textbox", { name: "Reply" });
    for (const expansion of ["first", "second"]) {
      await card.getByRole("button", { expanded: false }).focus();
      await page.keyboard.press("Enter");
      await expect(card).toHaveAttribute("aria-expanded", "true");
      await expect(field, `the ${expansion} expansion`).toBeFocused();
      // Escape in the reply cancels it, which collapses the thread.
      await page.keyboard.press("Escape");
      await expect(card).toHaveAttribute("aria-expanded", "false");
    }
  } finally {
    await alice.close();
  }
});
