import { expect, type Locator, type Page, test } from "@playwright/test";

import { setInterests, setLiveSessions } from "./agents";
import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  getInbox,
  getIssue,
  getIssueEvents,
  patchIssue,
  replyToCommentDelivery,
} from "./api";
import { recordClipboard } from "./clipboard";
import { setPendingCredentialRequests } from "./fake-broker-helpers";
import { resetDatabase, setCreatedAt } from "./seed";
import { asUser } from "./users";

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};
test.beforeEach(async () => {
  await resetDatabase();
});

test("ask cards show urgency accents and copy their session ID, title, and tmux target", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Urgency accents" });
  const tmuxSession = {
    ...session,
    actor: { ...session.actor, origin: { session_title: "e2e-session-title", tmux: "dev:4.7" } },
  };
  const blocking = await createAsk(
    issue.key,
    { question: "Blocking decision", urgency: "blocking" },
    tmuxSession
  );
  await createAsk(issue.key, { question: "High decision", urgency: "high" }, tmuxSession);
  await createAsk(issue.key, { question: "Medium decision", urgency: "med" }, tmuxSession);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const copied = await recordClipboard(page);
  try {
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }

    const inboxCards = page.locator("[data-testid^=ask-]");
    await expect(inboxCards).toHaveCount(3);
    await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toContainText(
      "BLOCKING"
    );
    await expect(page.getByRole("article", { name: "Urgency: High" })).toContainText("HIGH");
    await expect(page.getByRole("article", { name: "Urgency: Medium" })).toBeVisible();
    await expect(page.getByText("Medium", { exact: true })).toHaveCount(0);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Close navigation" }).click();
    }

    const blockingCard = page.getByTestId(`ask-${blocking.id}`);
    // The ask's own reference is on the provenance line; the session's own handles - its ID,
    // its title, its tmux target - fold behind the author chip, so the line fits a phone and a
    // 280 px margin without pushing the reference out of reach (LEGION-67).
    const reference = `dispatch://${issue.key}/ask/${blocking.id}`;
    await expect(blockingCard.getByRole("button", { name: /^Copy / })).toHaveCount(1);
    await blockingCard.getByRole("button", { expanded: false, name: /e2e-session/ }).click();
    const copyButtons = blockingCard.getByRole("button", { name: /^Copy / });
    await expect(copyButtons).toHaveCount(4);
    expect(
      await copyButtons.evaluateAll((buttons) => buttons.map((button) => button.ariaLabel))
    ).toEqual([
      `Copy reference ${reference}`,
      "Copy session ID e2e-session",
      "Copy session title e2e-session-title",
      "Copy tmux target dev:4.7",
    ]);
    await blockingCard.getByRole("button", { name: "Copy session ID e2e-session" }).click();
    await expect(blockingCard.getByText("Copied", { exact: true })).toBeVisible();
    await blockingCard
      .getByRole("button", { name: "Copy session title e2e-session-title" })
      .click();
    await blockingCard.getByRole("button", { name: "Copy tmux target dev:4.7" }).click();
    await blockingCard.getByRole("button", { name: `Copy reference ${reference}` }).click();
    await expect.poll(copied).toEqual(["e2e-session", "e2e-session-title", "dev:4.7", reference]);
    if (testInfo.project.name === "iphone") {
      await page.locator("main").screenshot({ path: testInfo.outputPath("askcard-390.png") });
    } else {
      await page.screenshot({ fullPage: true, path: testInfo.outputPath("askcard-1280.png") });
    }
  } finally {
    await alice.close();
  }
});

test("an ask thread shows its newest two replies, expands older replies, and puts a fresh reply first", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Newest replies" });
  const ask = await createAsk(issue.key, { question: "Which reply should lead?" }, session);
  const replies = await Promise.all(
    ["Oldest reply", "Older reply", "Middle reply", "Newer reply", "Newest reply"].map((body) =>
      createComment(issue.key, { ask_id: ask.id, body }, session)
    )
  );
  await Promise.all(
    replies.map((reply, index) =>
      setCreatedAt("comments", reply.id, `2026-10-03T00:00:0${index + 1}Z`)
    )
  );
  expect((await getAsk(ask.id)).replies.map((reply) => reply.body)).toEqual([
    "Oldest reply",
    "Older reply",
    "Middle reply",
    "Newer reply",
    "Newest reply",
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const thread = card.getByRole("region", { name: "Replies" });
    const replyBodies = () => thread.locator("li > div > .dispatch-markdown").allTextContents();

    await expect.poll(replyBodies).toEqual(["Newest reply", "Newer reply"]);
    await expect(card.getByText("Middle reply", { exact: true })).toHaveCount(0);
    const showMore = card.getByRole("button", { name: "Show 3 more replies" });
    await expect(showMore).toHaveAttribute("aria-expanded", "false");
    const before = testInfo.outputPath(`ask-thread-before-${testInfo.project.name}.png`);
    await page.screenshot({ path: before, fullPage: true });
    await testInfo.attach(`ask thread before (${testInfo.project.name})`, {
      contentType: "image/png",
      path: before,
    });

    await showMore.click();
    await expect
      .poll(replyBodies)
      .toEqual(["Newest reply", "Newer reply", "Middle reply", "Older reply", "Oldest reply"]);
    const showFewer = card.getByRole("button", { name: "Show fewer replies" });
    await expect(showFewer).toHaveAttribute("aria-expanded", "true");
    await showFewer.click();
    await expect.poll(replyBodies).toEqual(["Newest reply", "Newer reply"]);

    await card.getByLabel("Your answer").fill("Fresh reply");
    await card.getByRole("button", { name: "Ask back" }).click();
    await expect.poll(replyBodies).toEqual(["Fresh reply", "Newest reply"]);
    const after = testInfo.outputPath(`ask-thread-after-${testInfo.project.name}.png`);
    await page.screenshot({ path: after, fullPage: true });
    await testInfo.attach(`ask thread after (${testInfo.project.name})`, {
      contentType: "image/png",
      path: after,
    });
  } finally {
    await alice.close();
  }
});

test("an SSE reply refreshes a thread hydrated by the Inbox response", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Live Inbox thread" });
  const ask = await createAsk(issue.key, { question: "Which thread should refresh?" }, session);
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let askReads = 0;
  page.on("request", (request) => {
    if (
      request.method() === "GET" &&
      new URL(request.url()).pathname === `/api/v1/asks/${ask.id}`
    ) {
      askReads += 1;
    }
  });

  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card).toBeVisible();
    await page.waitForTimeout(300);
    expect(askReads).toBe(0);

    await createComment(issue.key, { ask_id: ask.id, body: "The live reply." }, session);

    await expect(card.getByText("The live reply.")).toBeVisible();
    await expect.poll(() => askReads).toBe(1);
  } finally {
    await alice.close();
  }
});

test("an event before a delayed Inbox response refreshes that ask's thread", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Delayed Inbox thread" });
  const ask = await createAsk(
    issue.key,
    { question: "Which delayed thread should refresh?" },
    session
  );
  const snapshot = await getInbox();
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const listRequested = Promise.withResolvers<void>();
  const releaseList = Promise.withResolvers<void>();
  const streamResponse = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/api/v1/events"
  );
  // Only the first response is held on the pre-event snapshot; a refresh the client issues
  // after the event reaches the real server, exactly as a browser's would.
  let heldFirstInbox = false;
  await page.route("**/api/v1/inbox", async (route) => {
    if (heldFirstInbox) {
      await route.continue();
      return;
    }
    heldFirstInbox = true;
    listRequested.resolve();
    await releaseList.promise;
    await route.fulfill({ contentType: "application/json", json: snapshot });
  });

  try {
    const navigation = page.goto("/");
    await Promise.all([listRequested.promise, streamResponse]);
    await createComment(issue.key, { ask_id: ask.id, body: "Reply after the snapshot." }, session);
    await page.waitForTimeout(150);
    releaseList.resolve();
    await navigation;

    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card.getByText("Reply after the snapshot.")).toBeVisible();
    // The released body predates the reply; it must never take the card back.
    await page.waitForTimeout(300);
    await expect(card.getByText("Reply after the snapshot.")).toBeVisible();
  } finally {
    releaseList.resolve();
    await alice.close();
  }
});

test("a later Inbox response clears a hidden ask's pending refresh", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const visibleIssue = await createIssue({ project: "CORE", title: "Visible Inbox ask" });
  const hiddenIssue = await createIssue({ project: "CORE", title: "Hidden Inbox ask" });
  await patchIssue(hiddenIssue.key, { assignee: "bob" });
  const visibleAsk = await createAsk(visibleIssue.key, { question: "Visible question" }, session);
  const hiddenAsk = await createAsk(hiddenIssue.key, { question: "Hidden question" }, session);
  const snapshot = await getInbox();
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let inboxReads = 0;
  let hiddenAskReads = 0;
  let holdInbox = true;
  const listRequested = Promise.withResolvers<void>();
  const releaseList = Promise.withResolvers<void>();
  page.on("request", (request) => {
    if (request.method() !== "GET") return;
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/inbox") inboxReads += 1;
    if (path === `/api/v1/asks/${hiddenAsk.id}`) hiddenAskReads += 1;
  });
  await page.goto("/");
  await expect(page.getByTestId(`ask-${visibleAsk.id}`)).toBeVisible();
  inboxReads = 0;
  await page.route("**/api/v1/inbox", async (route) => {
    if (!holdInbox) {
      await route.fallback();
      return;
    }
    holdInbox = false;
    listRequested.resolve();
    await releaseList.promise;
    await route.fulfill({ contentType: "application/json", json: snapshot });
  });

  try {
    const streamResponse = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/events"
    );
    const reloaded = page.reload();
    // A reload is a cold client: the server starts its stream at the current head and replays
    // nothing. Posting the hidden reply before that stream is open means the frame never
    // reaches the page, no pending marker is ever set, and everything below passes without
    // testing anything. Wait for the stream as the sibling above does.
    await Promise.all([listRequested.promise, streamResponse]);
    await createComment(
      hiddenIssue.key,
      { ask_id: hiddenAsk.id, body: "Fresh hidden reply." },
      session
    );
    // The held Inbox response must not land until the browser has handled the hidden ask's
    // event. A fixed delay is a race under load, and the hidden ask shows nothing to wait on;
    // a reply posted after it on the visible ask does. Event ids are delivered in order, so
    // the marker appearing proves the frame before it was processed.
    await createComment(
      visibleIssue.key,
      { ask_id: visibleAsk.id, body: "Ordering marker." },
      session
    );
    await expect(
      page.getByTestId(`ask-${visibleAsk.id}`).getByText("Ordering marker.")
    ).toBeVisible();
    releaseList.resolve();
    await reloaded;

    await createComment(
      visibleIssue.key,
      { ask_id: visibleAsk.id, body: "Refresh Inbox." },
      session
    );
    await expect.poll(() => inboxReads).toBeGreaterThanOrEqual(2);
    await page.getByRole("button", { name: "Everyone" }).click();
    const hiddenCard = page.getByTestId(`ask-${hiddenAsk.id}`);
    await expect(hiddenCard.getByText("Fresh hidden reply.")).toBeVisible();
    await page.waitForTimeout(300);
    await expect(hiddenCard.getByText("Fresh hidden reply.")).toBeVisible();
    // The marker was cleared by the later Inbox response, so no targeted read followed: the
    // marker and item 1 never both fire.
    expect(hiddenAskReads).toBe(0);
  } finally {
    releaseList.resolve();
    await alice.close();
  }
});

test("a quote-anchored inbox ask names and opens its document", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "A quoted passage.",
    title: "Anchored decision",
  });
  const ask = await createAsk(
    issue.key,
    { anchor: { artifact: "spec", quote: "quoted passage" }, question: "What does this mean?" },
    session
  );
  if (ask.anchor === null || ask.anchor.block_id === null) {
    throw new Error("anchored ask is missing its document block");
  }
  const documentHref = `/issues/${issue.key}/spec#b-${encodeURIComponent(ask.anchor.block_id)}`;

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const document = card.getByRole("link", { name: "spec.md" });
    await expect(document).toHaveAttribute("href", documentHref);
    await expect(card).toContainText("quoted passage");
    const width = testInfo.project.name === "iphone" ? "390" : "1280";
    const screenshot = testInfo.outputPath(`inbox-quote-anchor-${width}.png`);
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach(`quote-anchored Inbox card (${width}px)`, {
      contentType: "image/png",
      path: screenshot,
    });
    await document.click();
    await expect(page).toHaveURL(documentHref);
  } finally {
    await alice.close();
  }
});

test("inbox shows current asks and answers issue asks in the margin", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const firstIssue = await createIssue({ project: "CORE", title: "First decision" });
  const secondIssue = await createIssue({ project: "CORE", title: "Second decision" });
  await createAsk(
    firstIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "First ask" },
    session
  );
  await createAsk(
    secondIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Second ask" },
    session
  );
  const newestAsk = await createAsk(
    firstIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Newest ask" },
    session
  );
  await Promise.all([
    setLiveSessions([{ session_id: "e2e-session", title: "e2e-session-title" }]),
    setInterests([
      {
        session_id: "e2e-session",
        topics: [
          `notifications.dispatch.issue.${firstIssue.key}`,
          `notifications.dispatch.issue.${firstIssue.key}.>`,
        ],
      },
    ]),
  ]);
  await expect
    .poll(async () =>
      (await getIssueEvents(firstIssue.key)).some((event) => event.actor.id === "e2e-session")
    )
    .toBe(true);

  const alice = await asUser(browser, "alice");
  const alicePage = await alice.newPage();
  await alicePage.goto("/");
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }

  const inboxCards = alicePage.locator("[data-testid^=ask-]");
  await expect(inboxCards).toHaveCount(3);
  await expect(inboxCards.nth(0)).toContainText("Newest ask");
  await expect(alicePage.getByTestId(`ask-${newestAsk.id}`).locator("time")).toHaveAttribute(
    "dateTime",
    newestAsk.created_at
  );
  await alicePage.screenshot({ path: testInfo.outputPath("inbox-three-asks.png"), fullPage: true });
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }

  await alicePage.goto(`/issues/${firstIssue.key}`);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open review panel (2 open asks)" }).click();
  }
  const needsYou = alicePage.getByRole("region", { name: "Needs you" });
  const newestAskCard = needsYou.getByTestId(`ask-${newestAsk.id}`);
  const shipOption = newestAskCard.getByRole("radio", { name: "Ship" });
  await shipOption.check();
  await newestAskCard.getByRole("button", { exact: true, name: "Answer" }).click();
  await expect(newestAskCard).toHaveCount(0);
  await expect
    .poll(() => getAsk(newestAsk.id))
    .toMatchObject({
      ask: { answer: { selected: ["Ship"], user: "alice" }, state: "answered" },
    });
  await alicePage.goto("/");
  await expect(inboxCards).toHaveCount(2);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }
  const navigation = alicePage.getByRole("navigation", { name: "Navigation" });
  await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("2");
  await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("2");
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }

  const textOnlyAsk = await createAsk(
    secondIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Text-only ask" },
    session
  );
  await alicePage.goto("/");
  const textOnlyCard = alicePage.getByTestId(`ask-${textOnlyAsk.id}`);
  // Select Other before entering the alternative answer, so the decision records no real option.
  await textOnlyCard.getByRole("radio", { name: "Other" }).check();
  await textOnlyCard
    .getByLabel("Your answer")
    .fill("Neither option fits; going with a third path.");
  await textOnlyCard.getByRole("button", { name: "Answer" }).click();
  await expect(textOnlyCard).toHaveCount(0);
  await expect
    .poll(() => getAsk(textOnlyAsk.id))
    .toMatchObject({
      ask: {
        answer: {
          selected: [],
          text: "Neither option fits; going with a third path.",
          user: "alice",
        },
        state: "answered",
      },
    });

  const document = await createProjectDocument("CORE", {
    content: "# Design notes\n",
    name: "Design notes",
  });
  const documentAsk = await createArtifactAsk(
    document.artifact.id,
    { question: "Does this design need review?" },
    session
  );
  await alicePage.goto("/");
  await expect(alicePage.getByTestId(`ask-${documentAsk.id}`)).toContainText(
    "Does this design need review?"
  );
  await expect(alicePage.getByText("CORE · Design notes", { exact: true })).toBeVisible();

  await alice.close();
});

test("a clarification moves an ask under Waiting on agents until the asker hands the turn back", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "spec", title: "Turns" });
  const untouched = await createAsk(issue.key, { question: "Untouched ask" }, session);
  const clarifying = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Clarifying ask" },
    session
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    // Every open ask whose next turn is the human's appears in the top blocker section.
    await expect(page.locator("[data-testid^=ask-]")).toHaveCount(2);
    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Needs you" })).toHaveCount(0);

    // Alice asks for clarification instead of answering.
    const card = page.getByTestId(`ask-${clarifying.id}`);
    const thread = card.getByTestId(`thread-${clarifying.id}`);
    await card.getByLabel("Your answer").fill("Ship what, exactly?");
    await card.getByRole("button", { name: "Ask back" }).click();
    await expect(thread.getByText("Ship what, exactly?")).toBeVisible();
    await expect(card).toBeVisible();

    // The clarification now waits on its asker in the separate Waiting on agents section.
    await page.reload();
    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toBeVisible();
    await expect(page.getByText("Waiting on e2e-session-title")).toBeVisible();
    const cards = page.locator("[data-testid^=ask-]");
    await expect(cards).toHaveCount(2);
    await expect(cards.nth(0)).toHaveAttribute("data-testid", `ask-${untouched.id}`);
    await expect(cards.nth(1)).toHaveAttribute("data-testid", `ask-${clarifying.id}`);

    // An agent progress note keeps the turn: the ask stays under Waiting on agents and the
    // card still says whose move it is, even though the agent spoke last.
    await createComment(
      issue.key,
      { ask_id: clarifying.id, body: "Checking the release branch, back shortly.", turn: "agent" },
      session
    );
    await expect
      .poll(() => getAsk(clarifying.id))
      .toMatchObject({ ask: { waiting_on: "agent" }, replies: [{}, { turn: "agent" }] });
    await page.reload();
    const waitingOnAgents = page.locator('[data-inbox-section="agent"]');
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toBeVisible();
    await expect(waitingOnAgents.getByTestId(`ask-${clarifying.id}`)).toBeVisible();
    await expect(waitingOnAgents.getByText("Waiting on e2e-session-title")).toBeVisible();
    await expect(page.getByText("e2e-session-title replied")).toHaveCount(0);
    await expect(cards.nth(0)).toHaveAttribute("data-testid", `ask-${untouched.id}`);

    // The agent's plain reply hands the turn back: the clarification returns to the
    // server-ordered Waiting on you section.
    await createComment(
      issue.key,
      { ask_id: clarifying.id, body: "The release candidate." },
      session
    );
    await expect.poll(() => getAsk(clarifying.id)).toMatchObject({ ask: { waiting_on: "human" } });
    await page.reload();
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toHaveCount(0);
    await expect(page.getByText("e2e-session-title replied")).toBeVisible();
    await expect(page.getByTestId(`turn-${clarifying.id}`)).toHaveText("Waiting on you");
    await expect(cards.nth(0)).toHaveAttribute("data-testid", `ask-${clarifying.id}`);
    await expect(cards.nth(1)).toHaveAttribute("data-testid", `ask-${untouched.id}`);

    const questionShaped = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Question-shaped response" },
      session
    );
    await page.reload();
    const questionCard = page.getByTestId(`ask-${questionShaped.id}`);
    await questionCard.getByLabel("Your answer").fill("How does this fit our release plan?");
    await questionCard.getByRole("button", { name: "Answer" }).click();
    const clarification = questionCard.getByRole("button", { name: "Ask back instead" });
    await expect(clarification).toBeFocused();
    await clarification.press("Enter");
    await expect
      .poll(() => getAsk(questionShaped.id))
      .toMatchObject({
        ask: { answer: null, state: "open" },
        replies: [{ body: "How does this fit our release plan?" }],
      });
  } finally {
    await alice.close();
  }
});

test("Waiting on agents puts a later P0 ask ahead of an earlier P2 ask", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const p2Issue = await createIssue({ project: "CORE", title: "Earlier P2 issue" });
  await patchIssue(p2Issue.key, { priority: 2 });
  const p2Ask = await createAsk(p2Issue.key, { question: "Earlier P2 ask" }, session);
  const p0Issue = await createIssue({ project: "CORE", title: "Later P0 issue" });
  const p0Ask = await createAsk(p0Issue.key, { question: "Later P0 ask" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${p0Issue.key}`);
    const priority = page.getByLabel("Priority");
    await priority.selectOption("0");
    await expect(priority).toHaveValue("0");
    await expect.poll(() => getIssue(p0Issue.key)).toMatchObject({ priority: 0 });
    await Promise.all([
      createComment(p2Issue.key, { ask_id: p2Ask.id, body: `Clarify ${p2Ask.id}` }),
      createComment(p0Issue.key, { ask_id: p0Ask.id, body: `Clarify ${p0Ask.id}` }),
    ]);

    await page.goto("/");
    const waitingOnAgents = page.locator('[data-inbox-section="agent"]');
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toBeVisible();
    await expect(waitingOnAgents.getByTestId(`ask-${p0Ask.id}`)).toBeVisible();
    await expect(waitingOnAgents.getByTestId(`ask-${p2Ask.id}`)).toBeVisible();
    expect(
      await waitingOnAgents
        .locator("[data-testid^=ask-]")
        .evaluateAll((cards) => cards.map((card) => card.dataset.testid))
    ).toEqual([`ask-${p0Ask.id}`, `ask-${p2Ask.id}`]);
  } finally {
    await alice.close();
  }
});

test("the Unassigned band lists a P0 ask an agent is working on above a P2 ask waiting on the viewer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // Both issues are the shared token's, so nobody holds them and both land in Alice's
  // Unassigned band - the one Inbox section that spans both turns. Opened P2 first so recency
  // alone would keep it on top.
  const p2Issue = await createIssue({ project: "CORE", title: "Unassigned P2 issue" }, session);
  await patchIssue(p2Issue.key, { priority: 2 });
  const p2Ask = await createAsk(p2Issue.key, { question: "P2 waiting on a human" }, session);
  const p0Issue = await createIssue({ project: "CORE", title: "Unassigned P0 issue" }, session);
  await patchIssue(p0Issue.key, { priority: 0 });
  const p0Ask = await createAsk(p0Issue.key, { question: "P0 the agent is still on" }, session);
  await createComment(
    p0Issue.key,
    { ask_id: p0Ask.id, body: "Still working on it.", turn: "agent" },
    session
  );
  await expect.poll(() => getAsk(p0Ask.id)).toMatchObject({ ask: { waiting_on: "agent" } });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.getByRole("button", { name: "Mine" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    const unassigned = page.locator('[data-inbox-section="unassigned"]');
    await expect(unassigned.getByTestId(`ask-${p0Ask.id}`)).toBeVisible();
    await expect(unassigned.getByTestId(`ask-${p2Ask.id}`)).toBeVisible();
    expect(
      await unassigned
        .locator("[data-testid^=ask-]")
        .evaluateAll((cards) => cards.map((card) => card.dataset.testid))
    ).toEqual([`ask-${p0Ask.id}`, `ask-${p2Ask.id}`]);
    const shot = testInfo.outputPath(`inbox-unassigned-priority-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot, fullPage: true });
    await testInfo.attach(`Unassigned band, P0 above P2 (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });
  } finally {
    await alice.close();
  }
});

test("an inbox row sets its issue's priority in place", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Needs a priority" });
  await patchIssue(issue.key, { priority: 2 });
  const ask = await createAsk(issue.key, { question: "How urgent is this?" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.getByRole("listitem").filter({ has: page.getByTestId(`ask-${ask.id}`) });
    const control = row.getByLabel(`Priority of ${issue.key}`);
    await expect(row.locator("span", { hasText: /^P2$/ })).toBeVisible();
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await control.selectOption("0");
    expect((await patch).postDataJSON()).toEqual({ priority: 0 });
    await expect(row.locator("span", { hasText: /^P0$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 0 });
    // The row's link was not followed.
    expect(new URL(page.url()).pathname).toBe("/");

    const shot = testInfo.outputPath(`inbox-priority-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`inbox priority (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    await page.reload();
    await expect(row.locator("span", { hasText: /^P0$/ })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// Deferring a row is only worth anything if it stops being asked about, so this case gates the
// whole rule at the UI: the band, and the two counts that would otherwise keep nagging - the
// Blocked-on-you banner and the rail's Needs-you badge - including after an agent replies,
// which hands the turn back and is exactly what could pull a deferred ask onto the list.
test("a snoozed row leaves Later, the banner and the Needs-you badge alone until un-snoozed", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Deal with it later" });
  const ask = await createAsk(issue.key, { question: "Which approach?" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.getByRole("listitem").filter({ has: page.getByTestId(`ask-${ask.id}`) });
    const banner = page.getByText(/Blocked on you: 1 item/);
    const badge = page.getByText(/^Needs you 1$/);
    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(banner).toBeVisible();
    await expect(badge.first()).toBeVisible();

    const put = page.waitForRequest(
      (request) =>
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/asks/${ask.id}/snooze`
    );
    await row.getByLabel(`Snooze ${issue.key}`).selectOption("tomorrow");
    expect(Date.parse((await put).postDataJSON().snoozed_until)).toBeGreaterThan(Date.now());

    // Folded away, and off both counts: the viewer is not asked about it again until it
    // returns. This is the point of the feature, not a side effect of the band.
    const later = page.getByRole("button", { name: /^Later \(1\)/ });
    await expect(later).toBeVisible();
    await expect(row).toBeHidden();
    await expect(banner).toBeHidden();
    await expect(badge).toHaveCount(0);
    await expect
      .poll(async () => (await getInbox({ login: "alice" })).at(0)?.snoozed_until)
      .not.toBeNull();

    // The agent answers, handing the turn back: the row stays deferred and stays off both
    // counts. Only the moment passing or the reader's own un-snooze returns it.
    await createComment(issue.key, { ask_id: ask.id, body: "Here is what I found." }, session);
    await page.reload();
    await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
    await expect(row).toBeHidden();
    await expect(banner).toBeHidden();
    await expect(badge).toHaveCount(0);

    // The real pointer opens the disclosure; the row is there with its return time.
    await later.click();
    await expect(row).toBeVisible();
    await expect(row).toHaveAttribute("data-inbox-section", "later");

    const shot = testInfo.outputPath(`inbox-snooze-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`inbox snooze (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    const remove = page.waitForRequest(
      (request) =>
        request.method() === "DELETE" &&
        new URL(request.url()).pathname === `/api/v1/me/asks/${ask.id}/snooze`
    );
    await row.getByRole("button", { name: `Un-snooze ${issue.key}` }).click();
    await remove;
    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await expect(page.getByText(/Blocked on you: 1 item/)).toBeVisible();
    // The row's link was not followed by any of it.
    expect(new URL(page.url()).pathname).toBe("/");
    // What the page shows so far is the optimistic write; the reload reads the server, and a
    // reload while the DELETE is unanswered aborts it, so the un-snooze must be recorded first.
    await expect
      .poll(async () => (await getInbox({ login: "alice" })).at(0)?.snoozed_until)
      .toBeNull();

    await page.reload();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
  } finally {
    await alice.close();
  }
});

// A credential request is listed above the asks and waits on its approver as much as an ask
// whose turn is theirs, so it is as much a reason not to say "Nothing needs you" and as much a
// part of the Needs-you badge and the Blocked-on-you banner. A request waiting on someone else
// is in neither.
test("a pending credential request alone is listed, counted, and keeps the empty state away", async ({
  browser,
}, testInfo) => {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const empty = page.getByText("Nothing needs you");
    const badge = page.getByText(/^Needs you \d+$/);
    await page.goto("/");
    await expect(empty).toBeVisible();
    await expect(badge).toHaveCount(0);

    await setPendingCredentialRequests([
      {
        approver: "alice",
        identifiers: ["DEMO_API_KEY"],
        kind: "agent_secret",
        record_id: "record-alice",
        requested_at: new Date().toISOString(),
      },
      {
        approver: "bob",
        identifiers: ["BOB_API_KEY"],
        kind: "agent_secret",
        record_id: "record-bob",
        requested_at: new Date().toISOString(),
      },
    ]);
    // The broker publishes nothing to Dispatch's event stream, so the list is read on load.
    await page.reload();
    const requests = page.getByRole("region", { name: "Credential requests" });
    await expect(requests.getByRole("link")).toHaveCount(1);
    await expect(requests.getByRole("link")).toContainText("Secret request");
    await expect(requests.getByRole("link")).toContainText("DEMO_API_KEY");
    await expect(requests.getByRole("link")).toHaveAttribute("href", "/credentials/record-alice");
    await expect(empty).toHaveCount(0);
    await expect(page.getByText(/^Needs you 1$/).first()).toBeVisible();
    await expect(page.getByText(/^Blocked on you: 1 item, oldest/)).toBeVisible();

    const shot = testInfo.outputPath(`inbox-credential-request-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`inbox credential request (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });
  } finally {
    await alice.close();
  }
});

// With nothing waiting, "Nothing needs you" stays on screen through a window focus, which refetches
// what has gone stale and reopens the event stream, whose reconnect refreshes every query, and
// through leaving the Inbox and coming back. Every credential-list call after the page's first is
// held, so a refetch of that list, had one put it back to loading, would take the empty state off
// the screen for as long as the hold lasts. A Dispatch with no secrets broker answers the list
// `null` and is asked again only when the stream reconnects, over the held answer, so the held
// second call leaves the empty state on screen; with the fake broker the list keeps its empty
// answer on screen while a refetch runs. Neither page may log a failed request.
test("Nothing needs you stays on screen through a focus and a return to the Inbox", async ({
  browser,
}) => {
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const held = Promise.withResolvers<void>();
  try {
    const failures: string[] = [];
    page.on("response", (response) => {
      if (response.status() >= 400) {
        failures.push(`${response.status()} ${new URL(response.url()).pathname}`);
      }
    });
    let credentialCalls = 0;
    await page.route("**/api/v1/credential-requests?approver=me", async (route) => {
      credentialCalls += 1;
      if (credentialCalls > 1) await held.promise;
      await route.continue();
    });
    const empty = page.getByText("Nothing needs you");
    await page.goto("/");
    await expect(empty).toBeVisible();
    expect(credentialCalls).toBe(1);
    await page.evaluate(() => {
      const record = window as Window & { emptyStateGone?: number };
      record.emptyStateGone = 0;
      new MutationObserver(() => {
        if (!document.body.textContent?.includes("Nothing needs you")) {
          record.emptyStateGone = (record.emptyStateGone ?? 0) + 1;
        }
      }).observe(document.body, { characterData: true, childList: true, subtree: true });
    });

    // The stream's whole-cache refresh refetches the inbox list; once that has answered, every
    // refetch the focus started is on the wire.
    const refreshed = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/inbox"
    );
    await page.evaluate(() =>
      document.dispatchEvent(new Event("visibilitychange", { bubbles: true }))
    );
    await refreshed;
    await page.evaluate(() => {
      const frames = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
      return frames.promise;
    });
    expect(
      await page.evaluate(() => (window as Window & { emptyStateGone?: number }).emptyStateGone)
    ).toBe(0);

    await page.keyboard.press("g");
    await page.keyboard.press("s");
    await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
    await page.keyboard.press("g");
    await page.keyboard.press("i");
    await expect(empty).toBeVisible();
    expect(failures).toEqual([]);
  } finally {
    await page.unrouteAll({ behavior: "ignoreErrors" });
    held.resolve();
    await alice.close();
  }
});

test("an unanchored issue-level comment reaches Conversation, not document review", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Untouched issue" });
  await createComment(issue.key, { body: "No selection needed to comment." }, session);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();

  try {
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(
      page
        .getByRole("list", { name: "Conversation turns" })
        .getByText("No selection needed to comment.")
    ).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
    await page.screenshot({
      path: testInfo.outputPath("issue-level-comment.png"),
      fullPage: true,
    });

    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    await expect(page.getByLabel("Margin review items")).not.toContainText(
      "No selection needed to comment."
    );
  } finally {
    await alice.close();
  }
});

test("the inbox defaults to Mine with an Unassigned band, Assign to me takes a row, and Everyone is remembered per login", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // Alice's own issue (a human's issue is assigned to its creator), an issue the shared token
  // created (nobody holds it), and a project-document ask (a document has no assignee).
  const hers = await createIssue({ project: "CORE", title: "Alice's issue" });
  const nobodys = await createIssue({ project: "CORE", title: "Nobody's issue" }, session);
  expect(hers.assignee).toBe("alice");
  expect(nobodys.assignee).toBeNull();
  const hersAsk = await createAsk(hers.key, { question: "Alice's decision" }, session);
  const nobodysAsk = await createAsk(nobodys.key, { question: "Anyone's decision" }, session);
  const document = await createProjectDocument("CORE", {
    content: "# Design notes\n",
    name: "Design notes",
  });
  const documentAsk = await createArtifactAsk(
    document.artifact.id,
    { question: "Does this design need review?" },
    session
  );

  // GitHub's spelling of a login is what `/auth/whoami` echoes; issues carry the lowercase login.
  const alice = await asUser(browser, "Alice");
  const bob = await asUser(browser, "bob");
  const alicePage = await alice.newPage();
  const bobPage = await bob.newPage();
  const unassigned = (page: Page) => page.locator('[data-inbox-section="unassigned"]');
  const rowOf = (page: Page, id: string) =>
    page.getByRole("listitem").filter({ has: page.getByTestId(`ask-${id}`) });
  try {
    // Bob holds nothing: his first visit is Mine, whose only rows are the Unassigned band.
    await bobPage.goto("/");
    await expect(bobPage.getByRole("button", { name: "Mine" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(bobPage.getByRole("heading", { name: "Unassigned" })).toBeVisible();
    await expect(bobPage.getByRole("heading", { name: "Waiting on you" })).toHaveCount(0);
    await expect(bobPage.getByTestId(`ask-${hersAsk.id}`)).toHaveCount(0);
    await expect(unassigned(bobPage).getByTestId(`ask-${nobodysAsk.id}`)).toBeVisible();
    await expect(unassigned(bobPage).getByTestId(`ask-${documentAsk.id}`)).toBeVisible();
    await expect(
      rowOf(bobPage, documentAsk.id).getByRole("button", { name: /^Assign / })
    ).toHaveCount(0);
    const shot = testInfo.outputPath(`inbox-mine-unassigned-${testInfo.project.name}.png`);
    await bobPage.screenshot({ path: shot, fullPage: true });
    await testInfo.attach(`inbox Mine with the Unassigned band (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // Assign to me: one PATCH, and the row moves into Bob's own section at once.
    const patch = bobPage.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${nobodys.key}`
    );
    await bobPage.getByRole("button", { name: `Assign ${nobodys.key} to me` }).click();
    expect((await patch).postDataJSON()).toEqual({ assignee: "bob" });
    await expect(rowOf(bobPage, nobodysAsk.id)).toHaveAttribute("data-inbox-section", "human");
    await expect(bobPage.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect.poll(() => getIssue(nobodys.key)).toMatchObject({ assignee: "bob" });

    // Alice's Mine lists her own issue's ask (assignee `alice` matches her `Alice` sign-in), not
    // the one Bob took, and the document ask stays in her Unassigned band.
    await alicePage.goto("/");
    await expect(alicePage.getByRole("button", { name: "Mine" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(rowOf(alicePage, hersAsk.id)).toHaveAttribute("data-inbox-section", "human");
    await expect(alicePage.getByTestId(`ask-${nobodysAsk.id}`)).toHaveCount(0);
    await expect(unassigned(alicePage).getByTestId(`ask-${documentAsk.id}`)).toBeVisible();
    await expect(alicePage.getByText("Blocked on you: 2 items", { exact: false })).toBeVisible();

    // Everyone shows every open ask, the choice survives a reload, and it is hers alone.
    await alicePage.getByRole("button", { name: "Everyone" }).click();
    expect(new URL(alicePage.url()).searchParams.get("view")).toBe("everyone");
    await expect(alicePage.locator("[data-testid^=ask-]")).toHaveCount(3);
    await expect(alicePage.getByRole("heading", { name: "Unassigned" })).toHaveCount(0);
    await expect(alicePage.getByText("Blocked on you: 3 items", { exact: false })).toBeVisible();
    const everyone = testInfo.outputPath(`inbox-everyone-${testInfo.project.name}.png`);
    await alicePage.screenshot({ path: everyone, fullPage: true });
    await testInfo.attach(`inbox Everyone (${testInfo.project.name})`, {
      contentType: "image/png",
      path: everyone,
    });
    await alicePage.goto("/");
    await expect(alicePage.getByRole("button", { name: "Everyone" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(alicePage.locator("[data-testid^=ask-]")).toHaveCount(3);
    await bobPage.goto("/");
    await expect(bobPage.getByRole("button", { name: "Mine" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(bobPage.getByTestId(`ask-${hersAsk.id}`)).toHaveCount(0);

    // A shared link's `view` wins over the remembered choice without replacing it.
    await alicePage.goto("/?view=mine");
    await expect(alicePage.getByTestId(`ask-${nobodysAsk.id}`)).toHaveCount(0);
    await alicePage.goto("/");
    await expect(alicePage.locator("[data-testid^=ask-]")).toHaveCount(3);
  } finally {
    await alice.close();
    await bob.close();
  }
});

test("a mentioned session's callback reply reaches the open ask card", async ({ browser }) => {
  await setLiveSessions([{ capabilities: ["btw", "steer"], session_id: "planner", title: "P" }]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Callback ask thread" });
  const ask = await createAsk(issue.key, { question: "Which approach?" }, session);
  // A clarification in the ask's thread that pulls in a session; the session answers through
  // the delivery it was handed, not through POST .../comments.
  const clarification = await createComment(
    issue.key,
    {
      ask_id: ask.id,
      body: "Say more, @session:planner.",
      mentions: [{ target: "session:planner" }],
    },
    { login: "alice" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card.getByText("Say more, @session:planner.")).toBeVisible();

    await replyToCommentDelivery(
      clarification.id,
      { attempt: 1, body: "The second approach." },
      { id: "planner", kind: "session" }
    );

    await expect(card.getByText("The second approach.")).toBeVisible();
  } finally {
    await alice.close();
  }
});

// The window #1207's freshness marker covers: a pre-event Inbox body that lands inside the
// 100 ms debounce is committed and stamped fresh, so there is no request left for
// `cancelRefetch` to restart, the flush's `["ask-thread", id]` invalidation matches a query that
// does not exist yet, and a card mounting in the gap seeds its thread from the pre-event row
// under `staleTime: Infinity` and never refetches.
test("a stale Inbox body landing inside the debounce seeds a later card's thread", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const visibleIssue = await createIssue({ project: "CORE", title: "Visible Inbox ask" });
  const hiddenIssue = await createIssue({ project: "CORE", title: "Hidden Inbox ask" });
  await patchIssue(hiddenIssue.key, { assignee: "bob" });
  const visibleAsk = await createAsk(visibleIssue.key, { question: "Visible question" }, session);
  const hiddenAsk = await createAsk(hiddenIssue.key, { question: "Hidden question" }, session);
  const snapshot = await getInbox();

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let askReads = 0;
  page.on("request", (request) => {
    if (
      request.method() === "GET" &&
      new URL(request.url()).pathname === `/api/v1/asks/${hiddenAsk.id}`
    ) {
      askReads += 1;
    }
  });
  await page.goto("/");
  await expect(page.getByTestId(`ask-${visibleAsk.id}`)).toBeVisible();

  let inboxLoads = 0;
  const secondRequested = Promise.withResolvers<void>();
  const releaseSecond = Promise.withResolvers<void>();
  const releaseRest = Promise.withResolvers<void>();
  await page.route("**/api/v1/inbox", async (route) => {
    inboxLoads += 1;
    if (inboxLoads === 1) {
      secondRequested.resolve();
      await releaseSecond.promise;
      await route.fulfill({ contentType: "application/json", json: snapshot });
      return;
    }
    await releaseRest.promise;
    await route.fulfill({ contentType: "application/json", json: snapshot });
  });

  try {
    // Something unrelated puts the Inbox into a later (not first) load: it holds data already.
    await createComment(visibleIssue.key, { ask_id: visibleAsk.id, body: "Warm up." }, session);
    await secondRequested.promise;
    // The reply lands while that load is in flight - the window the marker covers.
    await createComment(
      hiddenIssue.key,
      { ask_id: hiddenAsk.id, body: "Fresh hidden reply." },
      session
    );
    // ... and the pre-event body lands before the 100ms flush, so cancelRefetch never restarts
    // it. This hold is shorter than INVALIDATION_DEBOUNCE_MS by design and has no client-visible
    // signal to wait on: a failure here means the window slipped, not that the marker regressed.
    await page.waitForTimeout(30);
    releaseSecond.resolve();
    await page.waitForTimeout(400);

    await page.getByRole("button", { name: "Everyone" }).click();
    const hiddenCard = page.getByTestId(`ask-${hiddenAsk.id}`);
    await expect(hiddenCard).toBeVisible();
    await expect(hiddenCard.getByText("Fresh hidden reply.")).toBeVisible();
    expect(askReads).toBeGreaterThan(0);
  } finally {
    releaseSecond.resolve();
    releaseRest.resolve();
    await alice.close();
  }
});

// A refused snooze or assignment is the row's widest content, and the server writes the reason:
// `snoozed_until must be in the future` is the handler's own, and a future one may be longer.
// Left to set its own width it pushed the row - and the whole document - past a phone viewport,
// so the reader scrolled sideways to reach Retry. The control group is bounded to the row and
// the alert wraps inside it; this covers both the handler's refusal and a long one.
for (const refusal of [
  { label: "the handler's own reason", reason: "snoozed_until must be in the future" },
  {
    label: "a long reason",
    reason:
      "snoozed_until must be in the future, and this deployment refuses a moment more than one year ahead of the request it arrived on",
  },
]) {
  test(`a refused snooze wraps inside the row and never widens the page: ${refusal.label}`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Deal with it later" });
    const ask = await createAsk(issue.key, { question: "Which approach?" }, session);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const viewport = page.viewportSize()?.width ?? 0;
      expect(viewport).toBeGreaterThan(0);
      await page.route(`**/api/v1/me/asks/${ask.id}/snooze`, (route) =>
        route.fulfill({
          body: JSON.stringify({ code: "INVALID_SNOOZE", error: refusal.reason }),
          contentType: "application/json",
          status: 400,
        })
      );
      await page.goto("/");
      const row = page.getByRole("listitem").filter({ has: page.getByTestId(`ask-${ask.id}`) });
      await expect(row).toBeVisible();
      await row.getByLabel(`Snooze ${issue.key}`).selectOption("tomorrow");

      const retry = row.getByRole("button", { name: "Retry" });
      await expect(retry).toBeVisible();
      await expect(retry).toBeInViewport();
      // The document never grows past the viewport: no sideways scroll to reach the refusal.
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        viewport
      );
      // The refusal never draws over the issue key. Where it lands is the row's own business:
      // beside the key when the row is wide enough (desktop, since a no-margin route gives the
      // Inbox the whole width) and on the line below when it is not (a phone). Asserting
      // "below" pinned one of those two layouts and went red on the other.
      const key = await page.locator("[data-inbox-owner]").first().boundingBox();
      // The refusal itself, not its Retry: the alert is the wide thing, and measuring the
      // narrow control inside it checked something the assertion does not claim.
      const alert = await row.getByRole("alert").boundingBox();
      expect(key).not.toBeNull();
      expect(alert).not.toBeNull();
      const overlaps =
        key !== null &&
        alert !== null &&
        alert.x < key.x + key.width &&
        alert.x + alert.width > key.x &&
        alert.y < key.y + key.height &&
        alert.y + alert.height > key.y;
      expect(overlaps).toBe(false);
    } finally {
      await alice.close();
    }
  });
}

test("an ask card's time stays with its author, never opening a line of its own", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the provenance line only wraps where it must");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Provenance line" });
  // A live session names itself in a sentence, so the author fills the line and the time is what
  // the wrap lands on.
  const author = {
    actor: {
      id: "e2e-long-author",
      kind: "session" as const,
      origin: {
        session_title: "Reviewing PR #1489 LEGION-67 production-check screen fixes at 390px",
        tmux: "dispatch:1.9",
      },
    },
    as: "agent" as const,
  };
  await createAsk(issue.key, { question: "Where does the time sit?" }, author);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.locator("[data-testid^='ask-']").first();
    await expect(card).toBeVisible();
    const middle = async (locator: Locator) => {
      const box = await locator.boundingBox();
      return Math.round((box?.y ?? -1) + (box?.height ?? 0) / 2);
    };
    const chip = card.getByRole("button", { name: /Reviewing PR #1489/ });
    const time = card.locator("time").first();
    await expect(chip).toBeVisible();
    await expect(time).toBeVisible();
    // Who and when are one group: the separator that joins them never starts a line.
    expect(await middle(time)).toBe(await middle(chip));
  } finally {
    await alice.close();
  }
});
