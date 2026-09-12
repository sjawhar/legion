import { expect, test } from "@playwright/test";

import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
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
  const childIssue = await createIssue({
    parent: firstIssue.key,
    project: "CORE",
    title: "Child decision",
  });
  await patchIssue(firstIssue.key, {
    external_links: [{ kind: "github_issue", url: "https://github.com/sjawhar/legion/issues/815" }],
  });
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
  // Every issue write the browser sends, attached on failure: the route-clear race
  // shows up as a PATCH carrying the previous route instead of "".
  const issueWrites: string[] = [];
  alicePage.on("request", (request) => {
    if (request.method() === "PATCH" && request.url().includes("/api/v1/issues/")) {
      issueWrites.push(`${new Date().toISOString()} PATCH ${request.url()} ${request.postData()}`);
    }
  });
  const attachIssueWrites = () =>
    testInfo.attach("issue-writes.log", {
      body: issueWrites.join("\n"),
      contentType: "text/plain",
    });
  await alicePage.goto("/");
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }

  const inboxCards = alicePage.locator("[data-testid^=ask-]");
  await expect(inboxCards).toHaveCount(3);
  await expect(inboxCards.nth(0)).toContainText("First ask");
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
  await shipOption.click();
  await expect(shipOption).toBeChecked();
  await newestAskCard.getByRole("button", { name: "Submit answer" }).click();
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

  await alicePage.goto(`/issues/${firstIssue.key}`);
  const subscribedAgents = alicePage.getByRole("region", { name: "Subscribed agents" });
  await expect(subscribedAgents).toContainText("e2e-session-title");
  await expect(subscribedAgents.getByText("e2e-session", { exact: true })).toHaveCount(0);
  await expect(subscribedAgents.locator("[title='e2e-session']")).toHaveCount(1);
  await alicePage.getByRole("heading", { level: 1 }).click();
  const issueTitle = alicePage.getByLabel("Issue title");
  await issueTitle.fill("First decision revised");
  await issueTitle.press("Enter");
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({ title: "First decision revised" });
  await alicePage.getByLabel("Status").selectOption("testing");
  await expect.poll(() => getIssue(firstIssue.key)).toMatchObject({ status: "testing" });
  await alicePage.getByText("Messages default to no route").click();
  await alicePage.getByLabel("Route").fill("role:legion-controller-core");
  await alicePage.getByRole("button", { name: "Save route" }).click();
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({
      route: "role:legion-controller-core",
    });
  await alicePage.getByText("Messages default to role:legion-controller-core").click();
  const routeInput = alicePage.getByLabel("Route");
  await routeInput.fill("");
  // The field must still read "" when the earlier PATCH's response has been applied;
  // a revert here (not a wrong request body) is the race this test guards.
  await expect(alicePage.getByRole("button", { name: "Save route" })).toBeEnabled();
  await expect(routeInput).toHaveValue("");
  await alicePage.getByRole("button", { name: "Save route" }).click();
  try {
    await expect.poll(() => getIssue(firstIssue.key)).toMatchObject({ route: null });
  } finally {
    await attachIssueWrites();
  }
  await expect(
    alicePage.getByRole("link", { name: "https://github.com/sjawhar/legion/issues/815" })
  ).toHaveAttribute("title", "GitHub details are unavailable for this sign-in.");
  await alicePage.getByRole("tab", { name: "Children" }).click();
  await expect(
    alicePage
      .getByRole("tabpanel")
      .getByRole("link", { name: `${childIssue.key} · Child decision` })
  ).toBeVisible();
  await alicePage.getByRole("tab", { name: "Conversation" }).click();
  const conversationAsk = alicePage.getByTestId(`ask-${newestAsk.id}`);
  await expect(conversationAsk).toHaveCount(1);
  await expect(conversationAsk).toContainText("Answered by");
  await alicePage.screenshot({ path: testInfo.outputPath("issue-page.png"), fullPage: true });

  const inboxContext = await asUser(browser, "alice");
  const inboxPage = await inboxContext.newPage();
  await inboxPage.goto("/");
  await expect(inboxPage.locator("[data-testid^=ask-]")).toHaveCount(2);
  await alicePage.getByLabel("Status").selectOption("done");
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({
      closed_at: expect.any(String),
      status: "done",
    });
  await expect(alicePage.getByText("This issue is closed.", { exact: true })).toBeVisible();
  await expect(inboxPage.locator("[data-testid^=ask-]")).toHaveCount(1);
  await expect(inboxPage.getByText("First ask", { exact: true })).toHaveCount(0);
  await inboxContext.close();
  await alicePage.getByRole("button", { name: "Pin issue" }).click();
  await alicePage.goto("/");
  await alicePage.goto(`/issues/${firstIssue.key}`);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(alicePage.getByRole("heading", { name: "Pinned" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }
  await expect(alicePage.getByRole("button", { name: "Unpin issue" })).toBeVisible();
  const bob = await asUser(browser, "bob");
  const bobPage = await bob.newPage();
  await bobPage.goto("/");
  await expect(bobPage.locator("[data-testid^=ask-]")).toHaveCount(1);
  await expect(bobPage.getByText("First ask", { exact: true })).toHaveCount(0);
  await expect(bobPage.getByRole("heading", { name: "Pinned" })).toHaveCount(0);

  const textOnlyAsk = await createAsk(
    secondIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Text-only ask" },
    session
  );
  await alicePage.goto("/");
  const textOnlyCard = alicePage.getByTestId(`ask-${textOnlyAsk.id}`);
  // Free text is a choice of its own: the Other row reveals the answer field.
  await expect(textOnlyCard.getByLabel("Your answer")).toHaveCount(0);
  await textOnlyCard.getByRole("radio", { name: "Other" }).check();
  await textOnlyCard
    .getByLabel("Your answer")
    .fill("Neither option fits; going with a third path.");
  await textOnlyCard.getByRole("button", { name: "Submit answer" }).click();
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

  await bob.close();
  await alice.close();
});

test("a clarification moves an ask under Waiting on agents until the asker replies", async ({
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
    const thread = page.getByTestId(`thread-${clarifying.id}`);
    await thread.getByLabel("Ask for clarification").fill("Ship what, exactly?");
    await thread.getByRole("button", { name: "Send" }).click();
    await expect(thread.getByText("Ship what, exactly?")).toBeVisible();
    await expect(card).toBeVisible();

    // The clarification now waits on its asker, below the older ask still waiting on Alice.
    await page.reload();
    await expect(page.getByRole("heading", { name: "Waiting on you" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toBeVisible();
    await expect(page.getByText("Waiting on e2e-session-title")).toBeVisible();
    const cards = page.locator("[data-testid^=ask-]");
    await expect(cards).toHaveCount(2);
    await expect(cards.nth(0)).toHaveAttribute("data-testid", `ask-${untouched.id}`);
    await expect(cards.nth(1)).toHaveAttribute("data-testid", `ask-${clarifying.id}`);

    // The agent replies in the thread: the ask is back in front of her, on top.
    await createComment(
      issue.key,
      { ask_id: clarifying.id, body: "The release candidate." },
      session
    );
    await page.reload();
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toHaveCount(0);
    await expect(page.getByText("e2e-session-title replied")).toBeVisible();
    await expect(cards.nth(0)).toHaveAttribute("data-testid", `ask-${untouched.id}`);
    await expect(cards.nth(1)).toHaveAttribute("data-testid", `ask-${clarifying.id}`);

    const questionShaped = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Question-shaped Other" },
      session
    );
    await page.reload();
    const questionCard = page.getByTestId(`ask-${questionShaped.id}`);
    await questionCard.getByRole("radio", { name: "Other" }).check();
    await questionCard.getByLabel("Your answer").fill("How does this fit our release plan?");
    await questionCard.getByRole("button", { name: "Submit answer" }).click();
    const clarification = questionCard.getByRole("button", {
      name: "This reads like a question — send as clarification (keeps the ask open)",
    });
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

test("a human can unsubscribe an agent from an issue and the session is told", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Subscriber removal" });
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([{ session_id: "e2e-unsub-session", title: "Worker (e2e)" }]);
    await setInterests([
      {
        session_id: "e2e-unsub-session",
        topics: [
          `notifications.dispatch.issue.${issue.key}`,
          `notifications.dispatch.issue.${issue.key}.>`,
        ],
      },
    ]);
  }

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();

  try {
    await page.goto(`/issues/${issue.key}`);
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribedAgents.getByText("Worker (e2e)", { exact: true })).toBeVisible();
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await expect(subscribedAgents.locator("[title='Live']")).toHaveCount(1);
    }

    await subscribedAgents.getByRole("button", { name: "Unsubscribe" }).click();
    const dialog = page.getByRole("dialog", { name: "Unsubscribe" });
    await expect(dialog).toContainText(
      `Unsubscribe Worker (e2e) from ${issue.key}? They will be told.`
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();

    await expect(page.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);

    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await expect
        .poll(async () =>
          (await getUnsubscribeCalls()).some((call) => call.session_id === "e2e-unsub-session")
        )
        .toBe(true);
    }

    await expect
      .poll(async () =>
        (await getIssueEvents(issue.key)).some((event) => event.type === "subscription.removed")
      )
      .toBe(true);

    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect(page.getByRole("list", { name: "Conversation turns" })).toContainText(
      "unsubscribed"
    );
  } finally {
    await alice.close();
  }
});
