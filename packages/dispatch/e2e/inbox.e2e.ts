import { expect, test } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createProject,
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
});

test("inbox answers asks inline and keeps issue state per user", async ({ browser }, testInfo) => {
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
  await expect(inboxCards.nth(0)).toContainText("Newest ask");
  await expect(alicePage.getByTestId(`ask-${newestAsk.id}`).locator("time")).toHaveAttribute(
    "dateTime",
    newestAsk.created_at
  );
  await alicePage.screenshot({ path: testInfo.outputPath("inbox-three-asks.png"), fullPage: true });
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }

  await newestAsk;
  await alicePage.getByTestId(`ask-${newestAsk.id}`).getByRole("radio", { name: "Ship" }).check();
  await alicePage
    .getByTestId(`ask-${newestAsk.id}`)
    .getByRole("button", { name: "Submit answer" })
    .click();
  await expect(inboxCards).toHaveCount(2);
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(alicePage.getByRole("heading", { name: "Needs you (2)" })).toBeVisible();
  if (testInfo.project.name === "iphone") {
    await alicePage.getByRole("button", { name: "Close navigation" }).click();
  }
  await expect
    .poll(() => getAsk(newestAsk.id))
    .toMatchObject({
      ask: { answer: { selected: ["Ship"], user: "alice" }, state: "answered" },
    });

  await alicePage.goto(`/issues/${firstIssue.key}`);
  const activeSessions = alicePage.getByRole("region", { name: "Active sessions" });
  await expect(activeSessions).toContainText("e2e-session-title");
  await expect(activeSessions.getByText("e2e-session", { exact: true })).toHaveCount(0);
  await expect(activeSessions.locator("[title='e2e-session']")).toHaveCount(1);
  await alicePage.getByRole("heading", { level: 1 }).click();
  const issueTitle = alicePage.getByLabel("Issue title");
  await issueTitle.fill("First decision revised");
  await issueTitle.press("Enter");
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({ title: "First decision revised" });
  await alicePage.getByLabel("Status").selectOption("testing");
  await expect.poll(() => getIssue(firstIssue.key)).toMatchObject({ status: "testing" });
  await alicePage.getByText("No route — messages stay on the issue").click();
  await alicePage.getByLabel("Route").fill("role:legion-controller-core");
  await alicePage.getByRole("button", { name: "Save route" }).click();
  await expect
    .poll(() => getIssue(firstIssue.key))
    .toMatchObject({
      route: "role:legion-controller-core",
    });
  await alicePage.getByText("Messages also reach role:legion-controller-core").click();
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
  await alicePage.getByRole("tab", { name: "Log" }).click();
  await expect(alicePage.getByText("Ask answered: Newest ask")).toBeVisible();
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
  await expect(alicePage.getByText("This issue is closed.")).toBeVisible();
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

  await bob.close();
  await alice.close();
});

test("posting an issue-level comment with no selection reaches the Log and Margin", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Untouched issue" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();

  try {
    await page.goto(`/issues/${issue.key}`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await expect(composer).toBeVisible();
    await composer.getByLabel("Comment").fill("No selection needed to comment.");
    await composer.getByRole("button", { exact: true, name: "Comment" }).click();

    await expect(
      page.locator("article[data-event-seq]").getByText("No selection needed to comment.")
    ).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
    await page.screenshot({
      path: testInfo.outputPath("issue-level-comment.png"),
      fullPage: true,
    });

    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open review panel" }).click();
    }
    await expect(
      page.getByLabel("Margin review items").getByText("No selection needed to comment.")
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});
