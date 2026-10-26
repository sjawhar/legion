import { expect, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions, setSessionSendStatus } from "./agents";
import {
  createIssue,
  createMessage,
  createProject,
  getMessage,
  listComments,
  listIssueMessages,
  patchIssue,
} from "./api";
import {
  actionBar,
  barAction,
  composer,
  documentEditor,
  expectMark,
  marginCard,
  selectEditorQuote,
} from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A closed issue still takes comments and messages (dispatch://INFRA-448), so a finished issue's
// agents can be asked from its page: each row closes the issue, then a person, in the browser,
// writes a comment, a message to an agent, a reply, a retry and a margin comment, and the API
// reads each back.
//
// Fixture: before the close, bob posted a plain message, a message to the live planner, and one
// to the live tester whose delivery failed (the tester's listener answers 404).

const spec = "The quick brown fox jumps over the lazy dog.";

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  session_id: "e2e-closed-planner",
  title: "planner",
};
const tester: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  session_id: "e2e-closed-tester",
  title: "tester",
};

interface Fixture {
  readonly issueKey: string;
  /** Bob's plain message: a thread a person replies to. */
  readonly questionId: string;
  /** Bob's message to the planner, delivered. */
  readonly plannerMessageId: string;
  /** Bob's message to the tester, whose delivery failed. */
  readonly testerMessageId: string;
}

test.beforeEach(async () => {
  await resetDatabase();
});

async function seedClosedIssue(): Promise<Fixture> {
  await setLiveSessions([planner, tester]);
  await setSessionSendStatus(tester.session_id, 404);
  await createProject({ key: "E2E", name: "Closed issues" });
  const issue = await createIssue({ project: "E2E", spec, title: "Asked after it closed" });
  const question = await createMessage(issue.key, { body: "Bob's question" }, { login: "bob" });
  const toPlanner = await createMessage(
    issue.key,
    { body: "Planner, is the fix in?", delivery: "btw", target: `session:${planner.session_id}` },
    { login: "bob" }
  );
  const toTester = await createMessage(
    issue.key,
    { body: "Tester, did it pass?", delivery: "btw", target: `session:${tester.session_id}` },
    { login: "bob" }
  );
  await patchIssue(issue.key, { status: "done" }, { login: "bob" });
  return {
    issueKey: issue.key,
    plannerMessageId: toPlanner.id,
    questionId: question.id,
    testerMessageId: toTester.id,
  };
}

/** The Conversation's card for message `id`, opened on the closed issue's page. */
async function openMessage(page: Page, issueKey: string, id: string) {
  await page.goto(`/issues/${issueKey}/conversation`);
  await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
  const card = page.locator(`[data-turn="message:${id}"]`).first();
  await expect(card).toBeVisible();
  return card;
}

/** Sends what the docked composer holds and waits for the server to take it. */
async function send(page: Page, path: string): Promise<void> {
  const form = page.getByRole("form", { name: "Comment composer" });
  const posted = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" && new URL(response.url()).pathname === path
  );
  await form.getByRole("button", { exact: true, name: "Send" }).click();
  expect((await posted).status()).toBe(201);
  await expect(form.getByLabel("Comment")).toHaveValue("");
}

test("a comment written in a closed issue's Conversation posts", async ({ browser }) => {
  const { issueKey } = await seedClosedIssue();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}/conversation`);
    await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
    const form = page.getByRole("form", { name: "Comment composer" });
    const field = form.getByLabel("Comment");
    await expect(field).toBeVisible();
    // A closed issue refuses an ask and a suggestion, so the composer offers neither kind.
    await expect(form.getByRole("button", { name: "Suggest" })).toHaveCount(0);
    await expect(form.getByRole("button", { exact: true, name: "Ask" })).toHaveCount(0);
    await field.fill("Posted after the close");
    await send(page, `/api/v1/issues/${issueKey}/comments`);
    expect((await listComments(issueKey)).map((comment) => comment.body)).toEqual([
      "Posted after the close",
    ]);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .getByText("Posted after the close", { exact: true })
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

// A person writes to an agent on an issue from that agent's thread there: the message goes on the
// issue, addressed to the agent, and is delivered to it.
test("a message to an agent written on a closed issue posts and reaches the agent", async ({
  browser,
}) => {
  const { issueKey, plannerMessageId } = await seedClosedIssue();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const card = await openMessage(page, issueKey, plannerMessageId);
    const reply = card.getByRole("button", { name: "Reply" }).first();
    await expect(reply).toBeVisible();
    await reply.click();
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByLabel("Comment")
      .fill("Message after the close");
    await send(page, `/api/v1/issues/${issueKey}/messages`);
    await expect
      .poll(async () =>
        (await listIssueMessages(issueKey))
          .find((thread) => thread.message.id === plannerMessageId)
          ?.replies.map((message) => [message.body, message.target])
      )
      .toEqual([["Message after the close", `session:${planner.session_id}`]]);
    await expect
      .poll(async () =>
        (await getSentMessages()).some(
          (sent) =>
            sent.target_session === planner.session_id &&
            JSON.stringify(sent).includes("Message after the close")
        )
      )
      .toBe(true);
  } finally {
    await alice.close();
  }
});

test("Reply on a closed issue's message thread posts", async ({ browser }) => {
  const { issueKey, questionId } = await seedClosedIssue();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const card = await openMessage(page, issueKey, questionId);
    const reply = card.getByRole("button", { name: "Reply" }).first();
    await expect(reply).toBeVisible();
    await reply.click();
    await page
      .getByRole("form", { name: "Comment composer" })
      .getByLabel("Comment")
      .fill("Replied after the close");
    await send(page, `/api/v1/issues/${issueKey}/messages`);
    await expect
      .poll(async () =>
        (await listIssueMessages(issueKey))
          .find((thread) => thread.message.id === questionId)
          ?.replies.map((message) => message.body)
      )
      .toEqual(["Replied after the close"]);
  } finally {
    await alice.close();
  }
});

test("Retry of a failed delivery on a closed issue posts a new attempt", async ({ browser }) => {
  const { issueKey, testerMessageId } = await seedClosedIssue();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const card = await openMessage(page, issueKey, testerMessageId);
    await expect(card).toContainText(`Failed: no live session ${tester.session_id}`);
    const retry = card.getByRole("button", { exact: true, name: "Retry" });
    await expect(retry).toBeVisible();
    await setSessionSendStatus(tester.session_id, 200);
    const posted = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === `/api/v1/messages/${testerMessageId}/deliveries`
    );
    await retry.click();
    expect((await posted).ok()).toBe(true);
    await expect
      .poll(async () =>
        (await getMessage(issueKey, testerMessageId)).message.deliveries.map((delivery) => [
          delivery.attempt,
          delivery.state,
        ])
      )
      .toEqual([
        [1, "failed"],
        [2, "sent"],
      ]);
  } finally {
    await alice.close();
  }
});

// A closed issue's document takes no edit, so the browser writes no mark: the selection bar offers
// Comment alone, the comment names its text by quote, the server writes the mark, and the thread
// arrives in the margin.
test("a margin comment on a closed issue's spec posts with its quote", async ({ browser }) => {
  const { issueKey } = await seedClosedIssue();
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issueKey}/spec`);
    await expect(documentEditor(page)).toContainText(spec);
    await selectEditorQuote(page, "brown");
    await expect(actionBar(page).getByRole("button")).toHaveText(["Comment"]);
    await barAction(page, "Comment");
    const margin = composer(page);
    await expect(margin.locator("blockquote")).toHaveText("brown");
    await expect(documentEditor(page).locator("[data-id]")).toHaveCount(0);
    await margin.getByLabel("Comment").fill("Anchored after the close");
    await margin.getByRole("button", { exact: true, name: "Send" }).click();
    await expect
      .poll(async () =>
        (await listComments(issueKey))
          .filter((comment) => comment.body === "Anchored after the close")
          .map((comment) => comment.anchor?.quote)
      )
      .toEqual(["brown"]);
    const anchored = (await listComments(issueKey)).find(
      (comment) => comment.body === "Anchored after the close"
    );
    if (anchored?.anchor == null) throw new Error("the anchored comment has no anchor");
    await expectMark(page, anchored.anchor.mark_id, "brown");
    await expect(marginCard(page, anchored.id)).toBeAttached();
  } finally {
    await alice.close();
  }
});
