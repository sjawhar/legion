import { expect, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getIssueEvents,
  resolveAsk,
} from "./api";
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
  const toggle = page.getByRole("button", {
    name: open ? "Open review panel" : "Close review panel",
  });
  if ((await toggle.count()) > 0) {
    await toggle.click();
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
      question: "Ship the change?",
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
  const thread = margin.getByTestId(`thread-${ask.id}`);

  // Alice replies before answering - the question is a thread from the start.
  await thread.getByLabel("Reply").fill("Any blockers first?");
  await thread.getByRole("button", { name: "Reply" }).click();
  await expect(thread.getByText("Any blockers first?")).toBeVisible();

  // Alice answers - a distinct, first-class event.
  await card.getByRole("radio", { name: "Ship" }).check();
  await card.getByRole("button", { name: "Submit answer" }).click();
  await expect(margin.getByText(/answered/)).toBeVisible();

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
  await expect(margin.getByText(/answered/)).toBeVisible();

  // The asking session replies over the API (bearer auth, its own comment on
  // the ask). Alice's already-open page shows it live over SSE, no reload.
  await createComment(issue.key, { ask_id: ask.id, body: "Thanks, merging." }, bobSession);
  await expect(threadAfterAnswer.getByText("Thanks, merging.")).toBeVisible();
  await expect(replies).toHaveCount(3);
  await expect(replies.nth(2)).toContainText("Thanks, merging.");

  await alice.close();
});

test("a session retraction leaves its reason in the thread and removes the human's live inbox item", async ({
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
  await expect(issuePage.getByRole("heading", { name: "Needs you (1)" })).toBeVisible();
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
  await expect(margin.getByTestId(`thread-${ask.id}`)).toContainText(
    "Retracted by e2e-session-bob - A newer question supersedes this one."
  );
  await expect(margin.getByRole("button", { name: "Reply" })).toHaveCount(0);
  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("ask-retraction-thread.png"),
  });
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Close review panel" }).click();
    await issuePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(issuePage.getByRole("heading", { exact: true, name: "Needs you" })).toHaveCount(0);
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Close navigation" }).click();
  }

  await issuePage.getByRole("tab", { name: "Log" }).click();
  await expect(
    issuePage
      .getByRole("region", { name: "Issue log" })
      .getByText("Retracted by e2e-session-bob - A newer question supersedes this one.")
  ).toBeVisible();

  await alice.close();
});
