import { expect, type Locator, type Page, test } from "@playwright/test";

import { createComment, createIssue, createProject, resolveComment } from "./api";
import { connectedDot, documentEditor, marginCard, openedThreadCard, setSheet } from "./editor";
import { resetDatabase } from "./seed";
import { refusePosts } from "./sends";
import { asUser } from "./users";

// A margin thread's own reply holds its draft until the server answers, like every composer.
// These rows put each thing that would move or unmount its card in the middle of the reply's send
// - someone else resolving the thread, the reader opening another one, the reader's own Resolve -
// and expect the draft and the refusal back in the thread the reply was sent from.
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

async function openSpec(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/spec`);
  await expect(documentEditor(page)).toContainText(spec);
  await expect(connectedDot(page)).toHaveText("connected");
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
    await openSpec(page, issueKey);
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
    await openSpec(page, issueKey);
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
    await openSpec(page, issueKey);
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
