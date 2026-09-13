import { expect, test } from "@playwright/test";

import { setInterests, setLiveSessions } from "./agents";
import {
  createArtifactAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  getAsk,
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
  await shipOption.click();
  await expect(shipOption).toBeChecked();
  await newestAskCard.getByRole("button", { name: "Answer" }).click();
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
  // A typed answer is the text-only option; no separate choice is required.
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

    // The agent reply returns the clarification to the server-ordered Waiting on you section.
    await createComment(
      issue.key,
      { ask_id: clarifying.id, body: "The release candidate." },
      session
    );
    await page.reload();
    await expect(page.getByRole("heading", { name: "Waiting on agents" })).toHaveCount(0);
    await expect(page.getByText("e2e-session-title replied")).toBeVisible();
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
    await page.goto("/");
    for (const ask of [p2Ask, p0Ask]) {
      const card = page.getByTestId(`ask-${ask.id}`);
      await card.getByLabel("Your answer").fill(`Clarify ${ask.id}`);
      await card.getByRole("button", { name: "Ask back" }).click();
    }

    await page.reload();
    const waitingOnAgents = page.getByRole("heading", { name: "Waiting on agents" }).locator("..");
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
