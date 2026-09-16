import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getIssueEvents,
  listComments,
  resolveAsk,
} from "./api";
import { documentEditor, replyInThread, threadCard } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// "bob" names the session that opened the ask - authenticated over the API with
// the agent bearer token, not a human X-Dispatch-User login - so its own reply
// (clause 2 below) exercises the same "agent replies live" path a real Legion
// session would use via dispatch_comment(reply_to_ask).
const bobSession = {
  actor: { kind: "session" as const, id: "e2e-session-bob", origin: { tmux: "dispatch:1.3" } },
  as: "agent" as const,
};

// On the phone layout the margin is a collapsed bottom sheet; open it before
// interacting with anything inside, as margin.e2e.ts does.
async function setSheet(page: Page, project: string, open: boolean): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const sheet = page.getByTestId("margin-sheet");
  const expanded = open ? "true" : "false";
  if ((await sheet.getAttribute("data-expanded")) !== expanded) {
    await page
      .getByRole("button", { name: open ? /Open review panel/ : /Close review panel/ })
      .click();
  }
  await expect(sheet).toHaveAttribute("data-expanded", expanded);
}

async function expandedThread(page: Page, rootId: string): Promise<Locator> {
  const card = threadCard(page, rootId);
  if ((await card.getAttribute("aria-expanded")) !== "true") {
    // Only the collapsed card's toggle - an expanded card holds Resolve/Edit/Reply buttons too.
    await card.locator('button[aria-expanded="false"]').click();
    // The card re-renders as the margin's selection and the page's first live events land
    // together; a click that hits mid-render can be dropped, so wait for the state, not the
    // click, before handing out a locator whose buttons only exist once expanded.
    await expect(card).toHaveAttribute("aria-expanded", "true");
  }
  const phoneThread = page.getByRole("dialog", { name: "Thread" });
  return (await phoneThread.count()) === 0 ? card : phoneThread;
}

async function closeThreadView(page: Page, project: string): Promise<void> {
  if (project !== "iphone") {
    return;
  }
  const phoneThread = page.getByRole("dialog", { name: "Thread" });
  if ((await phoneThread.count()) > 0) {
    await phoneThread.getByRole("button", { name: "Back" }).click();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("an ask is a thread: replies before and after answering, then a live agent reply", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Ship the change to production",
    title: "Threaded decision",
  });
  // Anchored, so the ask (and its thread) render in the margin, which - unlike
  // the open-only Inbox and issue-board feeds - keeps an answered ask visible
  // via AskCard's onAnswered callback.
  const ask = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "production" },
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship the **change**?",
    },
    bobSession
  );
  await expect
    .poll(async () =>
      (await getIssueEvents(issue.key)).some((event) => event.type === "ask.opened")
    )
    .toBe(true);

  const alice = await asUser(browser, "alice");
  const alicePage = await alice.newPage();
  await alicePage.goto(`/issues/${issue.key}`);
  await alicePage.getByRole("tab", { name: "Spec" }).click();
  await setSheet(alicePage, testInfo.project.name, true);

  const margin = alicePage.getByTestId("margin-sheet");
  const card = margin.getByTestId(`ask-${ask.id}`);
  await expect(card).toContainText("Ship the change?");
  await expect(card.locator("strong")).toHaveText("change");
  const thread = margin.getByTestId(`thread-${ask.id}`);

  // Alice asks back from the same compact composer that can submit her answer.
  await card.getByRole("button", { name: "Add a note or answer in your own words" }).click();
  await card.getByLabel("Your answer").fill("Any blockers first?");
  await card.getByRole("button", { name: "Ask back" }).click();
  await expect(thread.getByText("Any blockers first?")).toBeVisible();

  // Alice answers - a distinct, first-class event.
  await card.getByRole("radio", { name: "Ship" }).check();
  await card.getByRole("button", { exact: true, name: "Answer" }).click();
  await expect(margin.getByText(/Answered by/)).toBeVisible();

  // Answering does not end the conversation: the thread and its composer stay
  // open here (the margin, unlike the Inbox, keeps an anchored answered ask),
  // and Alice replies again.
  const threadAfterAnswer = margin.getByTestId(`thread-${ask.id}`);
  await expect(threadAfterAnswer.getByLabel("Reply")).toBeVisible();
  await threadAfterAnswer.getByLabel("Reply").fill("Shipping now.");
  await threadAfterAnswer.getByRole("button", { name: "Reply" }).click();
  await expect(threadAfterAnswer.getByText("Shipping now.")).toBeVisible();

  // All three - the first reply, the answer, and the second reply - render
  // under the question, and the two replies keep their chronological order.
  const replies = threadAfterAnswer.locator("li");
  await expect(replies).toHaveCount(2);
  await expect(replies.nth(0)).toContainText("Any blockers first?");
  await expect(replies.nth(1)).toContainText("Shipping now.");
  await expect(margin.getByText(/Answered by/)).toBeVisible();

  // The asking session replies over the API (bearer auth, its own comment on
  // the ask). Alice's already-open page shows it live over SSE, no reload.
  await createComment(issue.key, { ask_id: ask.id, body: "Thanks, merging." }, bobSession);
  await expect(threadAfterAnswer.getByText("Thanks, merging.")).toBeVisible();
  await expect(replies).toHaveCount(3);
  await expect(replies.nth(2)).toContainText("Thanks, merging.");

  await alice.close();
});

test("a comment reply after an agent-authored reply targets the root without copying its anchor", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Keep this root visible.",
    title: "Root-only comment replies",
  });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "Keep" },
    body: "Root comment.",
  });
  await createComment(issue.key, { body: "Agent reply.", reply_to: root.id }, bobSession);
  const alice = await asUser(browser, "alice");
  const submittedReplies: Record<string, unknown>[] = [];

  try {
    const page = await alice.newPage();
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        request.url().endsWith(`/api/v1/issues/${issue.key}/comments`)
      ) {
        submittedReplies.push(request.postDataJSON() as Record<string, unknown>);
      }
    });
    await page.goto(`/issues/${issue.key}`);
    await setSheet(page, testInfo.project.name, true);
    await threadCard(page, root.id).getByRole("button").click();
    const phoneThread = page.getByRole("dialog", { name: "Thread" });
    const thread = (await phoneThread.count()) === 0 ? threadCard(page, root.id) : phoneThread;
    const composer = thread.getByRole("form", { name: "Reply composer" });
    await composer.getByLabel("Reply").fill("Human thread reply.");
    await composer.getByRole("button", { name: "Reply" }).click();
    await expect.poll(() => submittedReplies).toHaveLength(1);
    expect(submittedReplies[0]).toMatchObject({
      body: "Human thread reply.",
      reply_to: root.id,
    });
    expect(submittedReplies[0]).not.toHaveProperty("anchor");
    await expect
      .poll(() => listComments(issue.key))
      .toContainEqual(expect.objectContaining({ body: "Human thread reply.", reply_to: root.id }));
  } finally {
    await alice.close();
  }
});

test("alice resolves a comment thread and bob reopens it", async ({ browser }, testInfo) => {
  await createProject({ key: "THREAD", name: "Thread controls" });
  const issue = await createIssue({
    project: "THREAD",
    spec: "The quick brown fox",
    title: "Resolve and reopen",
  });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "Should this change?",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await Promise.all([
      setSheet(alicePage, testInfo.project.name, true),
      setSheet(bobPage, testInfo.project.name, true),
    ]);
    await replyInThread(bobPage, root.id, "Looks good.");
    await closeThreadView(bobPage, testInfo.project.name);
    const aliceThread = await expandedThread(alicePage, root.id);
    await expect(aliceThread).toContainText("Looks good.");
    await aliceThread.getByRole("button", { name: "Resolve" }).click();
    await closeThreadView(alicePage, testInfo.project.name);
    await expect(threadCard(alicePage, root.id)).toHaveCount(0);
    await expect(threadCard(bobPage, root.id)).toHaveCount(0);

    await bobPage.getByRole("button", { name: "Resolved (1)" }).click();
    const resolvedThread = await expandedThread(bobPage, root.id);
    await expect(resolvedThread).toContainText(/Resolved by alice/);
    await resolvedThread.getByRole("button", { name: "Reopen" }).click();
    await closeThreadView(bobPage, testInfo.project.name);
    await expect(threadCard(alicePage, root.id)).toBeVisible();
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("only the author edits a comment and both viewers see its edited marker", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "EDIT", name: "Comment edits" });
  const issue = await createIssue({
    project: "EDIT",
    spec: "The quick brown fox",
    title: "Edit a comment",
  });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "Original comment",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    await Promise.all([
      setSheet(alicePage, testInfo.project.name, true),
      setSheet(bobPage, testInfo.project.name, true),
    ]);
    // Exact: the card's copy-reference button is named after the `EDIT-1` key.
    await expect(
      threadCard(bobPage, root.id).getByRole("button", { exact: true, name: "Edit" })
    ).toHaveCount(0);

    const aliceThread = await expandedThread(alicePage, root.id);
    await aliceThread.getByRole("button", { exact: true, name: "Edit" }).click();
    await aliceThread.getByLabel("Edit comment").fill("Edited comment");
    await aliceThread.getByRole("button", { name: "Save" }).click();
    await expect(aliceThread).toContainText("Edited comment");
    await closeThreadView(alicePage, testInfo.project.name);

    const bobThread = await expandedThread(bobPage, root.id);
    await expect(bobThread).toContainText("Edited comment");
    await expect(bobThread).toContainText("edited");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("an agent comment reply appears in the root comment thread", async ({ browser }, testInfo) => {
  await createProject({ key: "AGENT", name: "Agent reply" });
  const issue = await createIssue({
    project: "AGENT",
    spec: "The quick brown fox",
    title: "Agent reply thread",
  });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "Human root comment",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await setSheet(page, testInfo.project.name, true);
    await createComment(issue.key, { body: "Agent response", reply_to: root.id }, bobSession);

    const thread = await expandedThread(page, root.id);
    await expect(thread).toContainText("Agent response");
    await expect(thread.getByText("Agent response").locator("..")).toHaveCSS("margin-left", "0px");
  } finally {
    await alice.close();
  }
});

test("the phone sheet opens a full-height thread view with its composer pinned at the bottom", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "This scenario exercises the phone-specific view.");
  await createProject({ key: "PHONE", name: "Phone threads" });
  const issue = await createIssue({
    project: "PHONE",
    spec: "The quick brown fox",
    title: "Phone thread view",
  });
  const root = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "Thread summary",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await setSheet(page, testInfo.project.name, true);
    await threadCard(page, root.id).getByRole("button").click();
    const thread = page.getByRole("dialog", { name: "Thread" });
    await expect(thread).toBeVisible();
    const viewport = page.viewportSize();
    const threadBox = await thread.boundingBox();
    const composerBox = await thread.getByRole("form", { name: "Reply composer" }).boundingBox();
    if (viewport === null || threadBox === null || composerBox === null) {
      throw new Error("The phone thread view did not expose a measurable layout.");
    }
    expect(threadBox.height).toBeGreaterThanOrEqual(viewport.height * 0.8);
    expect(Math.abs(viewport.height - (composerBox.y + composerBox.height))).toBeLessThanOrEqual(8);
    await page.keyboard.press("Escape");
    await expect(thread).toHaveCount(0);
    await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
      viewport.width
    );
  } finally {
    await alice.close();
  }
});

test("a session retraction leaves its reason on the card and removes the human's live inbox item", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Ship the change to production",
    title: "Retracted decision",
  });
  const ask = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "production" },
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Ship the change?",
    },
    bobSession
  );

  const alice = await asUser(browser, "alice");
  const inboxPage = await alice.newPage();
  await inboxPage.goto("/");
  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toBeVisible();
  await inboxPage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("inbox-before-ask-retraction.png"),
  });

  const issuePage = await alice.newPage();
  await issuePage.goto(`/issues/${issue.key}`);
  await issuePage.getByRole("tab", { name: "Spec" }).click();
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Open navigation" }).click();
  }
  const navigation = issuePage.getByRole("navigation", { name: "Navigation" });
  await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("1");
  await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("1");
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Close navigation" }).click();
  }
  await setSheet(issuePage, testInfo.project.name, true);
  const margin = issuePage.getByTestId("margin-sheet");
  await expect(margin.getByTestId(`ask-${ask.id}`)).toBeVisible();

  await resolveAsk(
    ask.id,
    { kind: "retracted", reason: "A newer question supersedes this one." },
    bobSession
  );

  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toHaveCount(0);
  await inboxPage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("inbox-after-ask-retraction.png"),
  });
  // A retracted ask is withdrawn history: hidden by default, behind the same toggle as
  // resolved comment threads.
  await expect(margin.getByTestId(`ask-${ask.id}`)).toHaveCount(0);
  await margin.getByRole("button", { name: /^Resolved \(\d+\)$/ }).click();
  await expect(margin.getByTestId(`ask-${ask.id}`)).toContainText(
    "Retracted by session:e2e-sess… - A newer question supersedes this one."
  );
  await expect(margin.getByRole("button", { name: "Reply" })).toHaveCount(0);
  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("ask-retraction-thread.png"),
  });
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: /Close review panel/ }).click();
    await issuePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(issuePage.getByRole("heading", { exact: true, name: "Needs you" })).toHaveCount(0);
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Close navigation" }).click();
  }

  await issuePage.getByRole("tab", { name: "Conversation" }).click();
  const conversation = issuePage.getByRole("region", { name: "Conversation" });
  await expect(conversation.getByTestId(`ask-${ask.id}`)).toHaveCount(0);
  await issuePage.getByRole("checkbox", { name: /^Show retracted \(\d+\)$/ }).check();
  const resolvedCard = conversation.getByTestId(`ask-${ask.id}`);
  await expect(resolvedCard.getByTestId("ask-resolution-badge")).toHaveText("Retracted");
  await expect(resolvedCard).toContainText(
    "Retracted by session:e2e-sess… - A newer question supersedes this one."
  );

  await alice.close();
});

test("two suggestions on one block are each accepted from their collapsed card", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SUGG", name: "Suggestion cards" });
  const issue = await createIssue({
    project: "SUGG",
    spec: "The quick brown fox",
    title: "Accept from the card",
  });
  const first = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "quick" },
    body: "Suggested replacement.",
    suggestion: { replace_with: "swift" },
  });
  const second = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "Closer to the photo.",
    suggestion: { replace_with: "red" },
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    // Pending suggestion marks render the quote twice inline (struck and proposed), so the
    // whole sentence only reads plainly once every mark is gone.
    await expect(documentEditor(page)).toContainText("fox");
    await setSheet(page, testInfo.project.name, true);
    const firstCard = threadCard(page, first.id);
    const secondCard = threadCard(page, second.id);
    await expect(firstCard).toHaveAttribute("aria-expanded", "false");
    await expect(secondCard).toHaveAttribute("aria-expanded", "false");
    await expect(firstCard.locator("del")).toHaveText("quick");
    await expect(firstCard.locator("ins")).toHaveText("swift");
    await expect(secondCard.locator("ins")).toHaveText("red");
    await expect(secondCard).toContainText("Closer to the photo.");
    for (const card of [firstCard, secondCard]) {
      for (const name of ["Accept suggestion", "Reject suggestion"]) {
        const bounds = await card.getByRole("button", { name }).boundingBox();
        if (bounds === null) {
          throw new Error(`${name} has no bounds on a collapsed card.`);
        }
        expect(bounds.height).toBeGreaterThanOrEqual(testInfo.project.name === "iphone" ? 44 : 32);
      }
    }
    // The compact stylesheet lays every button out inline-flex; the collapsed toggle's preview
    // must still stack - struck text, then the proposal, then the note - not sit in one row.
    const [del, ins, note] = await Promise.all([
      secondCard.locator("del").boundingBox(),
      secondCard.locator("ins").boundingBox(),
      secondCard.getByText("Closer to the photo.").boundingBox(),
    ]);
    if (del === null || ins === null || note === null) {
      throw new Error("The collapsed suggestion preview has no measurable rows.");
    }
    expect(ins.y).toBeGreaterThanOrEqual(del.y + del.height);
    expect(note.y).toBeGreaterThanOrEqual(ins.y + ins.height);

    await firstCard.getByRole("button", { name: "Accept suggestion" }).click();
    await expect(documentEditor(page)).toContainText("swift");
    await expect(documentEditor(page)).not.toContainText("quick");
    await expect(firstCard).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Thread" })).toHaveCount(0);
    await expect(secondCard).toHaveAttribute("aria-expanded", "false");
    await secondCard.getByRole("button", { name: "Accept suggestion" }).click();
    await expect(documentEditor(page)).toContainText("The swift red fox");
    await expect(secondCard).toHaveCount(0);
    await expect
      .poll(() =>
        listComments(issue.key, issue.primary_artifact_id).then((items) =>
          items.map((item) => item.suggestion?.accepted)
        )
      )
      .toEqual([true, true]);
  } finally {
    await alice.close();
  }
});

test("accepting the later of two suggestions on the same text leaves the earlier collapsed card showing its orphaned anchor", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "SUGG", name: "Suggestion cards" });
  const issue = await createIssue({
    project: "SUGG",
    spec: "The quick brown fox",
    title: "Competing suggestions",
  });
  const earlier = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "fox" },
    body: "Suggested replacement.",
    suggestion: { replace_with: "cat" },
  });
  // A second suggestion mark on the same range takes over the mark; the earlier suggestion's
  // anchor is what the card must report once the document settles.
  const later = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "fox" },
    body: "Suggested replacement.",
    suggestion: { replace_with: "dog" },
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText("brown");
    await setSheet(page, testInfo.project.name, true);
    await threadCard(page, later.id).getByRole("button", { name: "Accept suggestion" }).click();
    await expect(documentEditor(page)).toContainText("The quick brown dog");
    await expect(threadCard(page, later.id)).toHaveCount(0);
    await expect
      .poll(() =>
        listComments(issue.key, issue.primary_artifact_id).then(
          (items) => items.find((item) => item.id === earlier.id)?.anchor?.orphaned
        )
      )
      .toBe(true);
    const earlierCard = threadCard(page, earlier.id);
    await expect(earlierCard).toHaveAttribute("aria-expanded", "false");
    await expect(earlierCard).toContainText("Text changed.");
    await expect(earlierCard.locator("ins")).toHaveText("cat");
    // The server would answer 409 ANCHOR_ORPHANED, so the card offers neither action.
    await expect(earlierCard.getByRole("button", { name: "Accept suggestion" })).toHaveCount(0);
    await expect(earlierCard.getByRole("button", { name: "Reject suggestion" })).toHaveCount(0);
    const earlierThread = await expandedThread(page, earlier.id);
    await expect(earlierThread.getByRole("button", { name: "Accept suggestion" })).toHaveCount(0);
    // Resolve is the one way to close an orphaned suggestion; the server tolerates its missing mark.
    await earlierThread.getByRole("button", { name: "Resolve" }).click();
    await closeThreadView(page, testInfo.project.name);
    await expect(threadCard(page, earlier.id)).toHaveCount(0);
    await expect
      .poll(() =>
        listComments(issue.key, issue.primary_artifact_id).then(
          (items) => items.find((item) => item.id === earlier.id)?.resolved
        )
      )
      .toBe(true);
    // The accepted later suggestion and the resolved earlier one both sit under Resolved.
    await page.getByRole("button", { name: "Resolved (2)" }).click();
    const resolvedThread = await expandedThread(page, earlier.id);
    await expect(resolvedThread).toContainText(/Resolved by alice/);
    await expect(resolvedThread.getByRole("button", { name: "Accept suggestion" })).toHaveCount(0);
    // Reopen tolerates the missing mark too; the card returns to the open list, still orphaned
    // and still without Accept/Reject.
    await resolvedThread.getByRole("button", { name: "Reopen" }).click();
    await closeThreadView(page, testInfo.project.name);
    await expect
      .poll(() =>
        listComments(issue.key, issue.primary_artifact_id).then(
          (items) => items.find((item) => item.id === earlier.id)?.resolved
        )
      )
      .toBe(false);
    await expect(page.getByRole("button", { name: "Resolved (1)" })).toBeVisible();
    const reopenedCard = threadCard(page, earlier.id);
    await expect(reopenedCard).toBeVisible();
    await expect(reopenedCard).toContainText("Text changed.");
    await expect(reopenedCard.getByRole("button", { name: "Accept suggestion" })).toHaveCount(0);
    const reopenedThread = await expandedThread(page, earlier.id);
    await expect(reopenedThread.getByRole("button", { name: "Resolve" })).toBeVisible();
  } finally {
    await alice.close();
  }
});
