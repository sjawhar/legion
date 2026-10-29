import { expect, type Locator, type Page, test } from "@playwright/test";
import {
  type FakeSession,
  getSentMessages,
  setInterests,
  setLiveSessions,
  setSessionSendStatus,
} from "./agents";
import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editAsk,
  getAsk,
  getIssue,
  getMessage,
  patchIssue,
  putIssueState,
  replyToMessageDelivery,
  resolveAsk,
  resolveComment,
} from "./api";
import { conversationFilter } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-conv-agent", kind: "session" as const },
  as: "agent" as const,
};
const bob = {
  actor: { id: "e2e-conv-bob", kind: "session" as const },
  as: "agent" as const,
};

/** A top-level Conversation turn containing `text` exactly; a thread's replies nest inside it. */
function turn(page: Page, text: string) {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li")
    .filter({ has: page.getByText(text, { exact: true }) });
}

/** The reply inside `root`'s thread whose body is `text` exactly (a quote of it is not exact). */
function threadReply(page: Page, root: Locator, text: string) {
  return root
    .getByRole("list", { name: "Replies" })
    .locator("li")
    .filter({ has: page.getByText(text, { exact: true }) });
}

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/w/legion",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: "s1",
  title: "planner",
};

test.beforeEach(async () => {
  await resetDatabase();
});

test("Conversation owns the route, lists one card per message newest first, and resolves live agent names @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Conversation route" });
  await createMessage(issue.key, { body: "First **bold** line" }, agent);
  await createMessage(issue.key, { body: "```ts\nconst x = 1;\n```" }, agent);
  await createMessage(issue.key, { body: "From bob" }, bob);
  await createMessage(issue.key, { body: "Another from bob" }, bob);
  await createMessage(
    issue.key,
    { body: "From a disconnected agent" },
    { actor: { id: "ghost-session-0000", kind: "session" }, as: "agent" }
  );

  await Promise.all([
    setLiveSessions([{ session_id: "e2e-conv-agent", title: "Planner (e2e)" }]),
    setInterests([
      {
        session_id: "e2e-conv-agent",
        topics: [
          `notifications.dispatch.issue.${issue.key}`,
          `notifications.dispatch.issue.${issue.key}.>`,
        ],
      },
    ]),
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribedAgents.getByText("Planner (e2e)", { exact: true })).toBeVisible();
    await expect(subscribedAgents.getByText("ghost-session-0000", { exact: true })).toHaveCount(0);

    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByRole("tablist", { name: "Issue detail" }).getByRole("tab")).toHaveText([
      "Spec",
      "Conversation",
      "Children",
      "Artifacts1",
      "Activity",
    ]);

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );

    // One card per root message, newest activity first: five messages, five cards, no event row
    // between them.
    const cards = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li[data-turn]");
    await expect(cards).toHaveCount(5);
    await expect(cards.first()).toContainText("From a disconnected agent");
    await expect(cards.last()).toContainText("First bold line");
    await expect(turn(page, "First bold line").locator("strong")).toHaveText("bold");
    await expect(turn(page, "const x = 1;").locator("pre code")).toContainText("const x = 1;");
    await expect(turn(page, "First bold line")).toContainText("Planner (e2e)");
    await expect(turn(page, "From a disconnected agent")).toContainText("session:ghost-se…");

    // The pin is the shared icon toggle at the turn's top-right, not a text button beside it.
    const firstTurn = turn(page, "First bold line");
    const pin = firstTurn.getByRole("button", { name: "Pin" });
    await expect(pin).toHaveAttribute("aria-pressed", "false");
    const [turnBox, pinBox] = await Promise.all([firstTurn.boundingBox(), pin.boundingBox()]);
    if (turnBox === null || pinBox === null) throw new Error("turn or pin has no layout box");
    expect(pinBox.x + pinBox.width).toBeGreaterThan(turnBox.x + turnBox.width - 16);
    expect(pinBox.y).toBeLessThan(turnBox.y + 16);
    await pin.click();
    await expect(firstTurn.getByRole("button", { name: "Unpin" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    // A pinned conversation is the review margin's to show and to unpin: the Pinned tab carries
    // the card's own body, and its Unpin releases the pin the Conversation made.
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    const pinnedCard = margin.getByRole("article").filter({ hasText: "First bold line" });
    await expect(pinnedCard).toHaveCount(1);
    await pinnedCard.getByRole("button", { name: "Unpin" }).click();
    await expect(margin.getByText("No pinned items.")).toBeVisible();
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }
    await expect(firstTurn.getByRole("button", { name: "Pin" })).toHaveAttribute(
      "aria-pressed",
      "false"
    );

    const screenshot = testInfo.outputPath(
      `conversation-pinned-turn-${testInfo.project.name === "iphone" ? "390" : "1280"}.png`
    );
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation desktop", { contentType: "image/png", path: screenshot });
  } finally {
    await alice.close();
  }
});

test("Conversation updates an open ask in place after an agent edit and preserves its prior question", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Edited decision" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "REST" }, { label: "gRPC" }], question: "Which implementation?" },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.getByRole("region", { name: "Conversation" });
    const card = conversation.getByTestId(`ask-${ask.id}`);
    await expect(card).toContainText("Which implementation?");

    await editAsk(
      ask.id,
      {
        options: [{ label: "HTTP" }, { label: "MCP" }],
        question: "Which transport should we implement?",
      },
      agent
    );

    await expect(card).toContainText("Which transport should we implement?");
    await expect(card).toContainText(/Edited /);
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(1);
    // The edit itself is an Activity line, not a row in the Conversation.
    await expect(
      conversation.getByText('edited the question "Which transport', { exact: false })
    ).toHaveCount(0);
    await card.getByText("Show 1 previous version").click();
    await expect(card.getByText("Which implementation?")).toBeVisible();
    await expect(card.getByText(/Reworded by /)).toBeVisible();
    await expect(card.getByRole("list", { name: "Options" })).toContainText("REST");
    await expect(card.getByRole("list", { name: "Options" })).toContainText("gRPC");

    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(
      page
        .getByRole("list", { name: "Issue activity" })
        .getByRole("listitem")
        .filter({ hasText: 'edited the question "Which transport' })
    ).toHaveCount(1);
    await page.getByRole("tab", { name: "Conversation" }).click();

    await page.goto("/");
    const inboxCard = page.getByTestId(`ask-${ask.id}`);
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(1);
    await expect(inboxCard).toContainText("Which transport should we implement?");
    await expect(inboxCard).toContainText(/Edited /);
    await page.screenshot({ path: testInfo.outputPath("ask-edited-in-place.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

test("a deep link to an old unanchored comment scrolls its Conversation turn into view @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Old comment target" });
  const comment = await createComment(issue.key, { body: "Deep linked comment" }, agent);
  for (let index = 0; index < 210; index += 1) {
    await createMessage(issue.key, { body: `Newer message ${index}` }, agent);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);

    // On a phone the link opens the comment's thread full-screen over the Conversation; Back
    // leaves it on the focused turn.
    if (testInfo.project.name === "iphone") {
      const thread = page.getByRole("dialog", { name: "Thread" });
      await expect(thread.getByText("Deep linked comment")).toBeVisible();
      await thread.getByRole("button", { name: "Back" }).click();
      await expect(thread).toHaveCount(0);
    }
    const target = page
      .getByRole("list", { name: "Conversation turns" })
      .locator('li[aria-current="true"]');
    await expect(target).toContainText("Deep linked comment");
    await expect(target).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("a message reply nests under its parent with the quoted link, and deep links scroll to the parent turn", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Message reply thread" });
  const root = await createMessage(issue.key, { body: "Ship the build tonight" }, agent);
  await createMessage(issue.key, { body: "Sounds good, thanks!", in_reply_to: root.id }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const rootTurn = turn(page, "Ship the build tonight");
    const reply = threadReply(page, rootTurn, "Sounds good, thanks!");
    await expect(reply.getByRole("link", { name: "Ship the build tonight" })).toBeVisible();
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .locator(':scope > li[data-turn^="message:"]')
    ).toHaveCount(1);

    await page.goto(`/issues/${issue.key}/messages/${root.id}`);
    const target = page
      .getByRole("list", { name: "Conversation turns" })
      .locator('li[aria-current="true"]');
    await expect(target).toContainText("Ship the build tonight");
    await expect(target).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test("a reply in a session-authored comment thread pre-fills a deletable agent mention @phone", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Threaded comment follow-up" });
  const parent = await createComment(
    issue.key,
    { body: "Can this ship?" },
    { actor: { id: "s1", kind: "session" }, as: "agent" }
  );
  // The reply's mention reads the session's live title from the agents list, which lands after
  // the turns render, and a collapsed comment card shows no author. A later message from the same
  // session heads its turn with that title once the list is in, from the same render the Reply
  // button reads, so the click waits for it.
  await createMessage(
    issue.key,
    { body: "Build is green." },
    {
      actor: { id: "s1", kind: "session" },
      as: "agent",
    }
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(turn(page, "Build is green.")).toContainText("planner");
    const card = turn(page, "Can this ship?");
    await card.getByRole("button", { name: "Reply" }).click();
    const composer = page.getByRole("form", { name: "Comment composer" });
    // The author is the agent, drawn outside the quote's link, which holds the quoted body.
    await expect(composer.getByText(/^Replying to planner — Can this ship\?$/)).toBeVisible();
    await expect(composer.getByRole("link", { exact: true, name: "planner" })).toBeVisible();
    await expect(composer.getByRole("link", { name: "— Can this ship?" })).toBeVisible();
    const field = composer.getByLabel("Comment");
    await expect(field).toHaveValue("@planner");
    await field.press("End");
    await field.pressSequentially(" It is green now - ship it.");
    const response = page.waitForResponse(
      (candidate) =>
        candidate.request().method() === "POST" &&
        candidate.url().endsWith(`/api/v1/issues/${issue.key}/comments`)
    );
    await composer.getByRole("button", { name: "Send" }).click();
    expect((await response).request().postDataJSON()).toEqual({
      body: "@planner It is green now - ship it.",
      delivery: "steer",
      mentions: [{ target: "session:s1" }],
      reply_to: parent.id,
    });
    const phoneThread = page.getByRole("dialog", { name: "Thread" });
    const replyRoot = (await phoneThread.count()) > 0 ? phoneThread : card;
    await expect(threadReply(page, replyRoot, "@planner It is green now - ship it.")).toBeVisible();
    await expect.poll(() => getSentMessages()).toMatchObject([{ target_session: "s1" }]);
  } finally {
    await alice.close();
  }
});

test("an agent's answer on a failed attempt is accepted and shown as the answer", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await setSessionSendStatus("s1", 404);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Failed receipt" });
  const question = "Did you get this?";
  const message = await createMessage(issue.key, {
    body: question,
    delivery: "btw",
    target: "session:s1",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = turn(page, question);
    await expect(card).toContainText("Failed: no live session s1");

    // The receipt was wrong: the session did get the message and answers on that attempt.
    const answer = (await replyToMessageDelivery(
      message.id,
      { attempt: 1, body: "Loud and clear." },
      { id: "s1", kind: "session" }
    )) as { id: string; in_reply_to: string | null };
    expect(answer.in_reply_to).toBe(message.id);
    await expect(card).toContainText("Answered by planner");
    await expect(threadReply(page, card, "Loud and clear.")).toBeVisible();
    await expect(card.getByRole("button", { name: "Use BTW instead" })).toHaveCount(0);
    const stored = await getMessage(issue.key, message.id);
    expect(stored.message.deliveries).toMatchObject([
      { attempt: 1, error: null, reply_id: answer.id, state: "sent" },
    ]);
  } finally {
    await alice.close();
  }
});

test("a compact answered ask's Write a reply opens its composer as the next keyboard stop", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reply order" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Ship it?" },
    agent
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  await createComment(issue.key, { ask_id: ask.id, body: "Thanks" }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const askTurn = page
      .getByRole("region", { name: "Conversation" })
      .locator(`[data-turn="ask:${ask.id}"]`);
    const replies = askTurn.getByRole("region", { name: "Replies" }).locator("li");
    await expect(replies).toHaveCount(1);
    await expect(replies.first()).toContainText("Thanks");

    const closedToggle = askTurn.getByRole("button", {
      exact: true,
      expanded: false,
      name: "Write a reply",
    });
    await closedToggle.focus();
    await page.keyboard.press("Enter");
    const openToggle = askTurn.getByRole("button", {
      exact: true,
      expanded: true,
      name: "Write a reply",
    });
    await expect(openToggle).toBeFocused();
    await page.keyboard.press("Tab");
    const field = askTurn.getByRole("textbox", { name: "Reply" });
    await expect(field).toBeFocused();
    const controls = await openToggle.getAttribute("aria-controls");
    expect(controls).not.toBeNull();
    await expect(
      page.locator(`[id="${controls}"]`).getByRole("textbox", { name: "Reply" })
    ).toBeFocused();

    await page.keyboard.type("Following up");
    await page.keyboard.press("Control+Enter");
    await expect(replies.first()).toContainText("Following up");
    await expect(replies.nth(1)).toContainText("Thanks");
  } finally {
    await alice.close();
  }
});

test("Conversation answers an ask in place, draws no event row, and keeps its cards mounted across the filter", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this specification.",
    title: "Activity",
  });
  await createMessage(issue.key, { body: "First turn" }, agent);
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    agent
  );
  await patchIssue(issue.key, { title: "Renamed" });
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "Review" }, body: "Review comment" },
    agent
  );
  const child = await createIssue({ parent: issue.key, project: "CORE", title: "Child activity" });
  await patchIssue(child.key, { status: "in_progress" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const firstTurn = turn(page, "First turn");
    await expect(firstTurn).toBeVisible();
    const handle = await firstTurn.elementHandle();
    if (handle === null) {
      throw new Error("the first conversation turn did not produce an element handle");
    }

    const conversation = page.getByRole("region", { name: "Conversation" });
    const card = conversation.getByTestId(`ask-${ask.id}`);
    await expect(card.getByRole("radio", { name: "Ship" })).toBeVisible();
    await expect(card.getByRole("radio", { name: "Hold" })).toBeVisible();
    // The list carries the compact density (the margin shows this same ask in full), so
    // Answer appears with the choice rather than above an always-open composer.
    await card.getByRole("radio", { name: "Ship" }).check();
    await expect(card.getByRole("button", { exact: true, name: "Answer" })).toBeVisible();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(
      card.getByRole("list", { name: "Options" }).locator('li[data-selected="true"]')
    ).toContainText("Ship");
    await expect(card).toContainText(/Asked .* · Answered by alice/);
    await expect(card.locator("time")).toHaveCount(2);
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(1);

    await createComment(issue.key, { ask_id: ask.id, body: "Thanks" }, agent);
    await expect
      .poll(() => getAsk(ask.id))
      .toMatchObject({
        replies: [expect.objectContaining({ body: "Thanks" })],
      });

    // The issue rename and the child's status move are the issue's events: the Conversation
    // draws neither, and the Activity tab draws both.
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByText("Review comment")).toBeVisible();
    await expect(turns.getByText("updated the issue")).toHaveCount(0);
    await expect(turns.getByText(`moved ${child.key} from`)).toHaveCount(0);
    await expect(conversation.locator('[data-kind="activity"]')).toHaveCount(0);

    // Switching bucket and back hides the answered ask and leaves the message card mounted.
    await conversationFilter(page, "Open").click();
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(0);
    await conversationFilter(page, "All").click();
    await expect(page.locator(`[data-turn="ask:${ask.id}"]`)).toHaveCount(1);
    expect(await handle.evaluate((element) => element.isConnected)).toBe(true);

    await page.getByRole("tab", { name: "Activity" }).click();
    const activity = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    await expect(activity.filter({ hasText: "updated the issue" })).not.toHaveCount(0);
    await expect(activity.filter({ hasText: `moved ${child.key} from` })).toHaveCount(1);

    await page.goto(`/issues/${issue.key}/conversation`);
    const refreshedTurn = page
      .getByRole("region", { name: "Conversation" })
      .locator(`[data-turn="ask:${ask.id}"]`);
    await expect(refreshedTurn).toContainText("Thanks");

    const screenshot = testInfo.outputPath("conversation-ask-and-activity.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation ask and activity", {
      contentType: "image/png",
      path: screenshot,
    });
  } finally {
    await alice.close();
  }
});

test("Conversation marks unread conversations, sends with Ctrl+Enter, and jumps to newer ones @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unread and follow" });
  await createMessage(issue.key, { body: "First message" }, agent);
  await createMessage(issue.key, { body: "Second message" }, agent);
  await createMessage(issue.key, { body: "Third message" }, agent);
  await createMessage(issue.key, { body: "Fourth message" }, agent);
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Overflow turn ${index}` }, agent);
  }
  await putIssueState(issue.key, { last_read_seq: (await getIssue(issue.key)).last_seq });
  await setLiveSessions([
    { session_id: "e2e-conv-agent", title: "Planner (e2e)" },
    { session_id: "e2e-conv-reviewer", title: "Reviewer (e2e)" },
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    // The Conversation has no dividers of any kind - they belong to the Activity tab - and
    // nothing is unread, so no card carries the New mark.
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turn(page, "Overflow turn 29")).toBeVisible();
    await expect(turns.getByRole("separator")).toHaveCount(0);
    await expect(turns.getByText("New", { exact: true })).toHaveCount(0);

    const composer = page.getByRole("form", { name: "Comment composer" });
    await expect(
      composer.getByRole("button", { name: /Choose recipient|BTW|Aside|Steer/ })
    ).toHaveCount(0);
    const message = composer.getByRole("textbox", { name: "Comment" });
    let sentRequests = 0;
    page.on("request", (request) => {
      if (
        request.method() === "POST" &&
        request.url().endsWith(`/api/v1/issues/${issue.key}/comments`)
      ) {
        sentRequests += 1;
      }
    });
    await message.fill("Hello from alice");
    await message.press("Enter");
    await expect(message).toHaveValue("Hello from alice\n");
    expect(sentRequests).toBe(0);
    const request = page.waitForRequest(
      (candidate) =>
        candidate.method() === "POST" &&
        candidate.url().endsWith(`/api/v1/issues/${issue.key}/comments`)
    );
    await message.press("Control+Enter");
    await request;
    const sentTurn = turn(page, "Hello from alice");
    await expect(sentTurn).toBeVisible();
    const sentBox = await sentTurn.boundingBox();
    expect(sentBox).not.toBeNull();
    expect((sentBox?.y ?? 0) + (sentBox?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );

    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const firstTurn = turn(page, "First message");
    const before = (await firstTurn.boundingBox())?.y;
    // Everything on screen is read, so the count the pill carries is exactly the conversations
    // that arrive next. The reader is at the oldest card, so no dwell writes the mark again.
    await putIssueState(issue.key, { last_read_seq: (await getIssue(issue.key)).last_seq });
    await createMessage(issue.key, { body: "Tail arrives" }, bob);
    const jump = page.getByTestId("jump-to-latest");
    await expect(jump).toHaveText("Jump to latest · 1 new");
    await createMessage(issue.key, { body: "Another tail arrives" }, bob);
    await expect(jump).toHaveText("Jump to latest · 2 new");
    await expect(turn(page, "Tail arrives").getByText("New", { exact: true })).toHaveCount(1);
    await expect.poll(async () => (await firstTurn.boundingBox())?.y).toBe(before);
    await jump.click();
    const tail = turn(page, "Tail arrives");
    await expect(tail).toBeVisible();
    const tailBox = await tail.boundingBox();
    expect(tailBox).not.toBeNull();
    expect((tailBox?.y ?? 0) + (tailBox?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );
    await expect(jump).toHaveCount(0);

    const screenshot = testInfo.outputPath("conversation-follow-latest.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation follow latest", {
      contentType: "image/png",
      path: screenshot,
    });
  } finally {
    await alice.close();
  }
});

test("the iPhone Conversation fits controls and keeps the composer above the review panel @phone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "phone geometry is covered by the iphone project");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone conversation" });
  for (let index = 0; index < 10; index += 1) {
    await createMessage(issue.key, { body: `Phone turn ${index}` }, agent);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(turn(page, "Phone turn 9")).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);

    const controls = page.locator(
      '[aria-label="Conversation"] button, [aria-label="Conversation"] textarea, [aria-label="Conversation"] input'
    );
    for (let index = 0; index < (await controls.count()); index += 1) {
      const control = controls.nth(index);
      const opacity = await control.evaluate((element) =>
        Number(getComputedStyle(element).opacity)
      );
      if (opacity === 0) {
        continue;
      }
      // What a finger taps: a control wrapped in its own label is tapped through the label, so
      // that is the target to measure. Demanding 44 px of the box itself drew a checkbox three
      // times the size of every other control.
      const height = await control.evaluate(
        (element) => (element.closest("label") ?? element).getBoundingClientRect().height
      );
      expect(height, `Conversation control ${index} is a touch target`).toBeGreaterThanOrEqual(44);
    }
    const lastTurn = turn(page, "Phone turn 9");
    await lastTurn.hover();
    const pinBox = await lastTurn.getByRole("button", { name: "Pin" }).boundingBox();
    expect(pinBox).not.toBeNull();
    expect(pinBox?.height ?? 0).toBeGreaterThanOrEqual(44);

    const message = page.getByRole("textbox", { name: "Comment" });
    await message.focus();
    const composer = page.getByRole("form", { name: "Comment composer" });
    const reviewPanelButton = page.getByRole("button", { name: /Open review panel/ });
    const positions = [0, await page.evaluate(() => document.documentElement.scrollHeight)];
    for (const position of positions) {
      await page.evaluate((top) => window.scrollTo(0, top), position);
      const composerBox = await composer.boundingBox();
      const reviewBox = await reviewPanelButton.boundingBox();
      const viewportHeight = await page.evaluate(() => window.innerHeight);
      expect(composerBox).not.toBeNull();
      expect(composerBox?.y ?? -1).toBeGreaterThanOrEqual(0);
      expect((composerBox?.y ?? 0) + (composerBox?.height ?? 0)).toBeLessThanOrEqual(
        viewportHeight
      );
      expect(reviewBox).not.toBeNull();
      expect((composerBox?.y ?? 0) + (composerBox?.height ?? 0)).toBeLessThanOrEqual(
        reviewBox?.y ?? 0
      );
    }

    const screenshot = testInfo.outputPath("conversation-phone.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach("conversation phone", { contentType: "image/png", path: screenshot });
  } finally {
    await alice.close();
  }
});

// A live session's title is free text and can be a whole sentence. An Activity line that lets
// its author set its own width pushed the line, the list and the document past the viewport, so
// a reader scrolled sideways through the history.
test("a long session title never widens the Activity list past its column @phone", async ({
  browser,
}, testInfo) => {
  const sessionId = "e2e-long-title-session";
  const longTitle =
    "Reviewing PR #1478 LEGION-67 conversation family visual polish pass at 390px and 1280px";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Long author labels" });
  // An upload is an Activity line - `<author> added <name>` - so the session's title is the
  // line's author column, which is the column under test.
  await createIssueArtifact(
    issue.key,
    { content: "# Sketch\n", name: "Transcript sketch" },
    { actor: { id: sessionId, kind: "session" }, as: "agent" }
  );
  await setLiveSessions([{ session_id: sessionId, title: longTitle }]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const width = testInfo.project.name === "chromium" ? 1280 : 390;
    if (testInfo.project.name === "chromium") {
      await page.setViewportSize({ height: 900, width });
    }
    await page.goto(`/issues/${issue.key}/activity`);
    const lines = page.getByRole("list", { name: "Issue activity" });
    await expect(lines).toBeVisible();
    // The line really is carrying that title, so the measurement below is not vacuous.
    const activity = lines.getByRole("listitem").filter({ hasText: "Transcript sketch" });
    await expect(activity).toContainText("Reviewing PR #1478");

    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
      width
    );
    // The list never grows past the column it sits in.
    const overflow = await lines.evaluate((node) => ({
      client: (node.parentElement ?? node).clientWidth,
      scroll: node.scrollWidth,
    }));
    expect(overflow.scroll).toBeLessThanOrEqual(overflow.client);

    // Who and when are one group: the time never wraps alone onto the next row.
    const authorBox = await activity.locator("span[title]").first().boundingBox();
    const timeBox = await activity.locator("time").first().boundingBox();
    expect(authorBox).not.toBeNull();
    expect(timeBox).not.toBeNull();
    expect(Math.abs((timeBox?.y ?? 0) - (authorBox?.y ?? 0))).toBeLessThanOrEqual(2);
  } finally {
    await context.close();
  }
});

// The filter is the Conversation's one control, and it is not a preference: every visit opens on
// All, whatever an earlier build of the tab stored under `dispatch.conversation.*`.
test("the filter opens on All in a fresh context and ignores the settings an older build stored", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Filter buckets" });
  const answered = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Ship the answered ask?" },
    agent
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  const retracted = await createAsk(issue.key, { question: "Ship the retracted ask?" }, agent);
  await resolveAsk(
    retracted.id,
    { kind: "retracted", reason: "A newer question supersedes this one." },
    agent
  );
  const resolved = await createComment(issue.key, { body: "A resolved thread." }, agent);
  await resolveComment(resolved.id);
  await createComment(issue.key, { body: "An open thread." }, agent);

  const expectEverythingListed = async (page: Page) => {
    await expect(conversationFilter(page, "All")).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator(`[data-turn="ask:${answered.id}"]`)).toHaveCount(1);
    await expect(page.locator(`[data-turn="ask:${retracted.id}"]`)).toHaveCount(1);
    await expect(page.locator(`[data-turn="comment:${resolved.id}"]`)).toHaveCount(1);
  };

  const fresh = await asUser(browser, "alice");
  try {
    const page = await fresh.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expectEverythingListed(page);

    // Open hides the answered ask; All brings it back.
    await conversationFilter(page, "Open").click();
    await expect(page.locator(`[data-turn="ask:${answered.id}"]`)).toHaveCount(0);
    await expect(page.locator(`[data-turn="ask:${retracted.id}"]`)).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${resolved.id}"]`)).toHaveCount(0);
    await conversationFilter(page, "All").click();
    await expectEverythingListed(page);
  } finally {
    await fresh.close();
  }

  const stored = await asUser(browser, "alice");
  try {
    await stored.addInitScript(() => {
      window.localStorage.setItem("dispatch.conversation.showActivity", "false");
      window.localStorage.setItem("dispatch.conversation.showRetracted", "false");
    });
    const page = await stored.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expectEverythingListed(page);
  } finally {
    await stored.close();
  }
});

// An ask answered after a later comment thread was written sits above it: the order is each
// conversation's own newest activity, not when it started.
test("an ask answered after a later comment thread sorts above it", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Order by newest activity" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Answered last?" },
    agent
  );
  const later = await createComment(issue.key, { body: "Written after the ask." }, agent);
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const cards = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li[data-turn]");
    await expect(cards).toHaveCount(2);
    await expect(cards.first()).toHaveAttribute("data-turn", `ask:${ask.id}`);
    await expect(cards.last()).toHaveAttribute("data-turn", `comment:${later.id}`);
  } finally {
    await alice.close();
  }
});

// The defect this tab exists to fix: a conversation older than any window of the event log was
// unreachable without paging back through it. The three per-issue reads have no window at all.
test("a closed ask older than 200 events is listed under Closed with no paging", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "No window" });
  const oldest = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "The oldest conversation?" },
    agent
  );
  await answerAsk(oldest.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  for (let index = 0; index < 205; index += 1) {
    await createMessage(issue.key, { body: `Burying message ${index}` }, agent);
  }
  expect((await getIssue(issue.key)).last_seq).toBeGreaterThan(200);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Load older" })).toHaveCount(0);
    await conversationFilter(page, "Closed").click();
    const card = page.locator(`[data-turn="ask:${oldest.id}"]`);
    await expect(card).toHaveCount(1);
    await expect(card).toContainText("The oldest conversation?");
    await expect(page.getByRole("button", { name: "Load older" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// One card per conversation, and not one event row among them: 2 asks, 2 comment threads and 1
// message thread make five cards however many status, claim and document events sit beside them.
// `e2e/activity.e2e.ts` holds the other half - the rows the Conversation does not draw.
test("the Conversation draws one card per conversation and no event row", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this specification.",
    title: "One card per conversation",
  });
  const answered = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Ship it?" },
    agent
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] }, { login: "bob" });
  const retracted = await createAsk(issue.key, { question: "Withdrawn question?" }, agent);
  await resolveAsk(retracted.id, { kind: "retracted", reason: "Superseded." }, agent);
  const resolved = await createComment(issue.key, { body: "A resolved thread." }, agent);
  await resolveComment(resolved.id);
  const open = await createComment(issue.key, { body: "An open thread." }, agent);
  const message = await createMessage(issue.key, { body: "A message thread." }, agent);
  // Three events of the kinds the Conversation never draws: a status change, a claim, and a
  // document version.
  await patchIssue(issue.key, { status: "in_progress" });
  await claimIssue(issue.key, agent);
  await createIssueArtifact(issue.key, { content: "# Note\n", name: "note.md" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const cards = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li[data-turn]");
    await expect(cards).toHaveCount(5);
    expect(
      (await cards.evaluateAll((items) => items.map((item) => item.getAttribute("data-turn"))))
        .slice()
        .sort()
    ).toEqual(
      [
        `ask:${answered.id}`,
        `ask:${retracted.id}`,
        `comment:${resolved.id}`,
        `comment:${open.id}`,
        `message:${message.id}`,
      ].sort()
    );
    await expect(page.locator('[data-kind="activity"]')).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
