import { expect, type Page, test } from "@playwright/test";

import { setInterests, setLiveSessions } from "./agents";
import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
  getIssue,
  getIssueEvents,
  patchIssue,
} from "./api";
import { recordClipboard } from "./clipboard";
import { resetDatabase } from "./seed";
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
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
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
    // Session identifiers come first; the tmux target keeps its lower-priority slot, and the
    // ask's own reference closes the line.
    const reference = `dispatch://${issue.key}/ask/${blocking.id}`;
    const copyButtons = blockingCard.getByRole("button", { name: /^Copy / });
    await expect(copyButtons).toHaveCount(4);
    expect(
      await copyButtons.evaluateAll((buttons) => buttons.map((button) => button.ariaLabel))
    ).toEqual([
      "Copy session ID e2e-session",
      "Copy session title e2e-session-title",
      "Copy tmux target dev:4.7",
      `Copy reference ${reference}`,
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

    await expect(card.getByText("The live reply.")).toBeVisible({ timeout: 10_000 });
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
  const baseUrl =
    process.env.PLAYWRIGHT_BASE_URL ??
    `http://127.0.0.1:${process.env.DISPATCH_E2E_PORT || "8777"}`;
  const snapshotResponse = await fetch(new URL("/api/v1/inbox", baseUrl), {
    headers: { "X-Dispatch-User": "alice" },
  });
  if (!snapshotResponse.ok) {
    throw new Error(`snapshot Inbox response failed: ${snapshotResponse.status}`);
  }
  const snapshot = await snapshotResponse.json();
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const listRequested = Promise.withResolvers<void>();
  const releaseList = Promise.withResolvers<void>();
  const streamResponse = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/api/v1/events"
  );
  let askReads = 0;
  page.on("request", (request) => {
    if (
      request.method() === "GET" &&
      new URL(request.url()).pathname === `/api/v1/asks/${ask.id}`
    ) {
      askReads += 1;
    }
  });
  await page.route("**/api/v1/inbox", async (route) => {
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
    await expect(card.getByText("Reply after the snapshot.")).toBeVisible({ timeout: 10_000 });
    await expect.poll(() => askReads).toBe(1);
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
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([{ session_id: "e2e-session", title: "e2e-session-title" }]);
    await setInterests([
      {
        session_id: "e2e-session",
        topics: [
          `notifications.dispatch.issue.${firstIssue.key}`,
          `notifications.dispatch.issue.${firstIssue.key}.>`,
        ],
      },
    ]);
  }
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
