import { expect, type Locator, test } from "@playwright/test";

import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getIssue,
  getIssueEvents,
  mintAgentToken,
  patchIssue,
} from "./api";
import { recordClipboard } from "./clipboard";
import { clearIssueCreator, resetDatabase } from "./seed";
import { asUser } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

test("issue header closes an issue and reopens it into Backlog", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Close from the header" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const close = page.getByRole("button", { name: "Close issue" });
    await expect(close).toBeVisible();
    const closing = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await close.click();
    expect((await closing).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    const reopen = page.getByRole("button", { name: "Reopen issue" });
    await expect(reopen).toHaveAttribute("title", "Reopen into Backlog");
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        closed_at: expect.any(String),
        status: "done",
      });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    // Done is a collapsed rail by default: it counts the closed card until the edges are shown.
    await expect(page.getByRole("region", { name: "Done (collapsed)" })).toContainText("1");
    await page.getByRole("button", { name: "Show Icebox & Done" }).click();
    const done = page.getByRole("region", { name: "Done" });
    await expect(
      done.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();

    await page.goto(`/issues/${issue.key}`);
    const reopening = page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await reopen.click();
    expect((await reopening).status()).toBe(200);
    await expect(page.getByText("This issue is closed.", { exact: true })).toHaveCount(0);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({ closed_at: null, status: "backlog" });

    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const backlog = page.getByRole("region", { name: "Backlog" });
    await expect(
      backlog.getByRole("article", { name: `${issue.key} Close from the header` })
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};

test("issue header identifies session and human creators", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await setLiveSessions([
    {
      session_id: "e2e-session",
      title: "chief of staff",
    },
  ]);
  const sessionIssue = await createIssue(
    { project: "CORE", title: "Session-authored issue" },
    session
  );
  const humanIssue = await createIssue({ project: "CORE", title: "Human-authored issue" });
  const historicalIssue = await createIssue({ project: "CORE", title: "Historical issue" });
  await clearIssueCreator(historicalIssue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${sessionIssue.key}`);
    const sessionHeader = page.getByTestId("issue-header");
    await expect(sessionHeader).toContainText(/Opened by:\s*chief of staff/);
    if (testInfo.project.name === "iphone") {
      const rail = sessionHeader.getByTestId("issue-metadata-rail");
      expect(await rail.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(
        true
      );
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
    }
    const sessionScreenshot = testInfo.outputPath("issue-header-session-creator.png");
    await sessionHeader.screenshot({ path: sessionScreenshot });
    await testInfo.attach("session-authored issue header", {
      contentType: "image/png",
      path: sessionScreenshot,
    });

    await page.goto(`/issues/${humanIssue.key}`);
    const humanHeader = page.getByTestId("issue-header");
    await expect(humanHeader).toContainText(/Opened by:\s*alice/);
    const humanScreenshot = testInfo.outputPath("issue-header-human-creator.png");
    await humanHeader.screenshot({ path: humanScreenshot });
    await testInfo.attach("human-authored issue header", {
      contentType: "image/png",
      path: humanScreenshot,
    });

    await page.goto(`/issues/${historicalIssue.key}`);
    const historicalHeader = page.getByTestId("issue-header");
    await expect(historicalHeader).toBeVisible();
    await expect(historicalHeader.getByText("Opened by:", { exact: true })).toHaveCount(0);
    const historicalScreenshot = testInfo.outputPath("issue-header-historical-creator.png");
    await historicalHeader.screenshot({ path: historicalScreenshot });
    await testInfo.attach("historical issue header", {
      contentType: "image/png",
      path: historicalScreenshot,
    });
  } finally {
    await context.close();
  }
});

test("issue header copies its key and persists its title, status, and route", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const copied = await recordClipboard(page);
  const issueWrites: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "PATCH" && request.url().includes("/api/v1/issues/")) {
      issueWrites.push(`${new Date().toISOString()} PATCH ${request.url()} ${request.postData()}`);
    }
  });
  const attachIssueWrites = () =>
    testInfo.attach("issue-writes.log", {
      body: issueWrites.join("\n"),
      contentType: "text/plain",
    });

  try {
    await page.goto(`/issues/${issue.key}`);
    const heading = page.getByRole("heading", { level: 1, name: "First decision" });
    await expect(heading).toBeVisible();
    const copyKey = page.getByRole("button", { name: `Copy issue key ${issue.key}` });
    await copyKey.click();
    await expect(page.getByText("Copied", { exact: true })).toBeVisible();
    await expect.poll(copied).toEqual([issue.key]);
    // The same button copies the dispatch:// reference under Ctrl/Cmd.
    await copyKey.click({ modifiers: ["ControlOrMeta"] });
    await expect.poll(copied).toEqual([issue.key, `dispatch://${issue.key}`]);
    await page.getByText("Owner:", { exact: true }).hover();
    await page.getByText("Owner:", { exact: true }).evaluate((label) => {
      const hintId = label.getAttribute("aria-describedby");
      const hint = hintId === null ? null : document.getElementById(hintId);
      if (hint === null) {
        throw new Error("Owner hint is missing");
      }
      const labelBox = label.getBoundingClientRect();
      Object.assign(hint.style, {
        background: "Canvas",
        border: "1px solid CanvasText",
        borderRadius: "0.375rem",
        boxShadow: "0 4px 6px rgb(0 0 0 / 0.15)",
        clip: "auto",
        clipPath: "none",
        color: "CanvasText",
        height: "auto",
        left: `${Math.min(labelBox.left, window.innerWidth - 304)}px`,
        maxWidth: "calc(100vw - 16px)",
        overflow: "visible",
        padding: "0.5rem",
        position: "fixed",
        top: `${labelBox.bottom + 8}px`,
        whiteSpace: "normal",
        width: "18rem",
        zIndex: "50",
      });
    });
    await page.screenshot({
      path: testInfo.outputPath(
        `issue-header-${testInfo.project.name === "iphone" ? "390" : "1280"}.png`
      ),
    });
    await heading.click();
    const issueTitle = page.getByLabel("Issue title");
    await issueTitle.fill("First decision revised");
    await issueTitle.press("Enter");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ title: "First decision revised" });
    await page.getByLabel("Status").selectOption("todo");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ status: "todo" });
    await page.getByRole("button", { name: "Messages default to no owner" }).click();
    await page.getByLabel("Owner").fill("role:legion-controller-core");
    await page.getByRole("button", { name: "Save owner" }).click();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        route: "role:legion-controller-core",
      });
    await page
      .getByRole("button", { name: "Messages default to role:legion-controller-core" })
      .click();
    const routeInput = page.getByLabel("Owner");
    await routeInput.fill("");
    // The field must still read "" when the earlier PATCH's response has been applied;
    // a revert here (not a wrong request body) is the race this test guards.
    await expect(page.getByRole("button", { name: "Save owner" })).toBeEnabled();
    await expect(routeInput).toHaveValue("");
    await page.getByRole("button", { name: "Save owner" }).click();
    try {
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ route: null });
    } finally {
      await attachIssueWrites();
    }
  } finally {
    await context.close();
  }
});

test("issue pages show subscribers, external-link fallbacks, and children", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const child = await createIssue({
    parent: issue.key,
    project: "CORE",
    title: "Child decision",
  });
  const grandchild = await createIssue({
    parent: child.key,
    project: "CORE",
    title: "Grandchild decision",
  });
  await patchIssue(grandchild.key, { status: "done" });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_issue", url: "https://github.com/sjawhar/legion/issues/815" }],
  });
  await createAsk(issue.key, { question: "First ask" }, session);
  await subscribeSession(issue.key);
  await expect
    .poll(async () =>
      (await getIssueEvents(issue.key)).some((event) => event.actor.id === "e2e-session")
    )
    .toBe(true);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
    const subscribedAgents = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribedAgents).toContainText("e2e-session-title");
    await expect(subscribedAgents.getByText("e2e-session", { exact: true })).toHaveCount(0);
    await expect(subscribedAgents.locator("[title='e2e-session']")).toHaveCount(1);
    // Without GitHub App credentials the link still reads as the reference, never the raw address.
    await expect(page.getByRole("link", { name: "#815 sjawhar/legion" })).toHaveAttribute(
      "title",
      "GitHub details are unavailable for this sign-in."
    );
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/issues/815" })
    ).toHaveCount(0);
    // No component attached anywhere on the chain: the rail's read-only line says so.
    await expect(page.getByTestId("issue-components")).toContainText("Components:Not attached");
    await page.getByRole("tab", { name: "Children" }).click();
    const childRow = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · Child decision` });
    await expect(
      childRow.getByRole("link", { name: `${child.key} · Child decision` })
    ).toBeVisible();
    // The row rolls up the whole subtree: the child itself plus its done grandchild.
    await expect(childRow).toContainText("1/2 done");
    await expect(childRow.locator("time")).toHaveCount(1);
  } finally {
    await context.close();
  }
});

test("editing the parent line moves the issue across Children tabs live", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const oldParent = await createIssue({ project: "CORE", title: "Old parent" });
  const newParent = await createIssue({ project: "CORE", title: "New parent" });
  const mover = await createIssue({
    parent: oldParent.key,
    project: "CORE",
    title: "Moving decision",
  });

  const context = await asUser(browser, "alice");
  const parentPage = await context.newPage();
  const moverPage = await context.newPage();
  try {
    await parentPage.goto(`/issues/${newParent.key}`);
    await parentPage.getByRole("tab", { name: "Children" }).click();
    await expect(parentPage.getByText("No child issues.")).toBeVisible();

    await moverPage.goto(`/issues/${mover.key}`);
    await expect(moverPage.getByRole("link", { name: oldParent.key })).toBeVisible();
    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    await moverPage.getByLabel("Parent").fill(newParent.key);
    await moverPage.getByRole("button", { name: "Save parent" }).click();
    await expect(moverPage.getByRole("link", { name: newParent.key })).toBeVisible();

    // The new parent's open page hears child.added over SSE and gains the row without a reload.
    const movedRow = parentPage
      .getByRole("tabpanel")
      .getByRole("link", { name: `${mover.key} · Moving decision` });
    await expect(movedRow).toBeVisible();

    await moverPage.getByRole("button", { name: "Edit parent issue" }).click();
    await moverPage.getByLabel("Parent").fill("");
    await moverPage.getByRole("button", { name: "Save parent" }).click();
    await expect(moverPage.getByRole("button", { name: "Set parent issue" })).toBeVisible();
    await expect.poll(() => getIssue(mover.key)).toMatchObject({ parent: null });

    // And child.removed empties it again, still without a reload.
    await expect(movedRow).toHaveCount(0);
    await expect(parentPage.getByText("No child issues.")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("an answered ask remains in the issue conversation", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Newest ask" },
    session
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open review panel (1 open ask)" }).click();
    }
    const askCard = page.getByTestId(`ask-${ask.id}`);
    await askCard.getByRole("radio", { name: "Ship" }).check();
    await askCard.getByRole("button", { exact: true, name: "Answer" }).click();
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Close review panel (0 open asks)" }).click();
    }
    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationAsk = page.getByTestId(`ask-${ask.id}`);
    await expect(conversationAsk).toHaveCount(1);
    await expect(conversationAsk).toContainText("Answered by");
  } finally {
    await context.close();
  }
});

test("a quote-anchored ask identifies its document in the margin and Conversation", async ({
  browser,
}, testInfo) => {
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
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open review panel (1 open ask)" }).click();
    }
    const marginCard = page.getByRole("region", { name: "Needs you" }).getByTestId(`ask-${ask.id}`);
    await expect(marginCard.getByRole("link", { name: "spec.md" })).toHaveAttribute(
      "href",
      documentHref
    );
    await expect(marginCard).toContainText("quoted passage");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Close review panel (1 open ask)" }).click();
    }

    await page.getByRole("tab", { name: "Conversation" }).click();
    const conversationCard = page.locator("#issue-conversation-panel").getByTestId(`ask-${ask.id}`);
    await expect(conversationCard.getByRole("link", { name: "spec.md" })).toHaveAttribute(
      "href",
      documentHref
    );
    await expect(conversationCard).toContainText("quoted passage");
  } finally {
    await alice.close();
  }
});

test("closing an issue removes its asks from the inbox and pinning stays private", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });
  const otherIssue = await createIssue({ project: "CORE", title: "Second decision" });
  await createAsk(issue.key, { question: "First ask" }, session);
  await createAsk(otherIssue.key, { question: "Second ask" }, session);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const inboxContext = await asUser(browser, "alice");
  const inbox = await inboxContext.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    await inbox.goto("/");
    await expect(inbox.locator("[data-testid^=ask-]")).toHaveCount(2);
    await page.getByRole("button", { name: "Close issue" }).click();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        closed_at: expect.any(String),
        status: "done",
      });
    await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
    await expect(inbox.locator("[data-testid^=ask-]")).toHaveCount(1);
    await expect(inbox.getByText("First ask", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Pin issue" }).click();
    await expect(page.getByRole("button", { name: "Unpin issue" })).toBeEnabled();
    await page.goto("/");
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Close navigation" }).click();
    }
    const bob = await asUser(browser, "bob");
    const bobPage = await bob.newPage();
    try {
      // Alice's issues are not in Bob's Mine; the closed one is gone from Everyone too.
      await bobPage.goto("/?view=everyone");
      await expect(bobPage.locator("[data-testid^=ask-]")).toHaveCount(1);
      await expect(bobPage.getByText("First ask", { exact: true })).toHaveCount(0);
      await expect(bobPage.getByRole("heading", { name: "Pinned" })).toHaveCount(0);
    } finally {
      await bob.close();
    }
  } finally {
    await inboxContext.close();
    await alice.close();
  }
});

test("pinning survives an immediate full navigation", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Navigation-safe event pin" });
  await createMessage(issue.key, { body: "Pinned before navigation" });
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    const initialState = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/me/state" && response.ok()
    );
    await page.goto(`/issues/${issue.key}/conversation`);
    await initialState;
    const getIntercepted = Promise.withResolvers<void>();
    const releaseGet = Promise.withResolvers<void>();
    const queueState = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/me/state" && response.ok()
    );
    const putIntercepted = Promise.withResolvers<void>();
    const releasePut = Promise.withResolvers<void>();
    let firstPut = true;
    await page.route("**/api/v1/me/state", async (route) => {
      getIntercepted.resolve();
      await releaseGet.promise;
      await route.continue();
    });
    await page.route("**/api/v1/me/issues/*/state", async (route) => {
      if (route.request().method() !== "PUT" || !firstPut) {
        await route.continue();
        return;
      }
      firstPut = false;
      putIntercepted.resolve();
      await releasePut.promise;
      await route.continue();
    });
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ has: page.getByText("Pinned before navigation", { exact: true }) })
      .getByRole("button", { name: "Pin" })
      .click();
    await getIntercepted.promise;
    releaseGet.resolve();
    await queueState;
    await putIntercepted.promise;
    const navigated = page.waitForEvent(
      "framenavigated",
      (frame) => frame === page.mainFrame() && new URL(frame.url()).pathname === "/"
    );
    const navigation = page.goto("/");
    await navigated;
    releasePut.resolve();
    await navigation;
    await page.unroute("**/api/v1/me/state");
    await page.unroute("**/api/v1/me/issues/*/state");
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    await expect(page.getByText("Pinned before navigation", { exact: true })).toBeVisible();
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
    await page.getByRole("button", { name: "Subscribers: 1" }).click();
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

async function subscribeSession(issueKey: string): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) return;
  await setLiveSessions([{ session_id: "e2e-session", title: "e2e-session-title" }]);
  await setInterests([
    {
      session_id: "e2e-session",
      topics: [
        `notifications.dispatch.issue.${issueKey}`,
        `notifications.dispatch.issue.${issueKey}.>`,
      ],
    },
  ]);
}

test("issue header keeps every control in a shared row on a phone and the whose-turn indicator in view at every width", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the viewports are set explicitly in the desktop browser project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Header on a phone" });
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }],
    labels: ["Frontend", "api"],
    priority: 1,
  });
  await createAsk(issue.key, { question: "Which layout?" }, session);
  await subscribeSession(issue.key);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const top = async (locator: Locator) => Math.round((await locator.boundingBox())?.y ?? -1);
  const middle = async (locator: Locator) => {
    const box = await locator.boundingBox();
    return Math.round((box?.y ?? -1) + (box?.height ?? 0) / 2);
  };
  const inView = async (locator: Locator, width: number) => {
    const box = await locator.boundingBox();
    return box !== null && box.x >= 0 && box.x + box.width <= width;
  };
  try {
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(`/issues/${issue.key}`);
    const header = page.getByTestId("issue-header");
    const indicator = page.getByTestId("issue-whose-turn");
    const openedBy = page.getByText("Opened by:", { exact: true });
    const subscribers = page.getByRole("button", { name: "Subscribers: 1" });
    const link = page.getByRole("link", { name: "#1051 sjawhar/legion" });
    await expect(indicator).toHaveText("Waiting on you (1)");
    await expect(subscribers).toBeVisible();
    await expect(link).toBeVisible();
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/pull/1051" })
    ).toHaveCount(0);

    // Close shares the state row; creator, labels, subscribers, and the GitHub link share the
    // details line with the indicator — nothing but the title sits on a row of its own.
    const stateRow = await top(page.getByLabel("Status"));
    expect(await top(page.getByLabel("Priority"))).toBe(stateRow);
    expect(await top(page.getByRole("button", { name: "Close issue" }))).toBe(stateRow);
    const rail = page.getByTestId("issue-metadata-rail");
    const detailsRow = await top(rail);
    expect(detailsRow).toBeGreaterThan(stateRow);
    const detailsRowMiddle = await middle(page.getByTestId("issue-labels"));
    expect(await middle(openedBy)).toBe(detailsRowMiddle);
    expect(await middle(subscribers)).toBe(detailsRowMiddle);
    expect(await middle(link)).toBe(detailsRowMiddle);
    expect(await inView(indicator, 390)).toBe(true);
    const phoneHeader = await header.boundingBox();
    expect(phoneHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThan(300);
    await expect(
      page.evaluate(
        () => document.documentElement.scrollWidth === document.documentElement.clientWidth
      )
    ).resolves.toBe(true);
    const phoneShot = testInfo.outputPath("issue-header-390.png");
    await page.screenshot({ path: phoneShot });
    await testInfo.attach("issue header (390px)", { contentType: "image/png", path: phoneShot });

    await page.setViewportSize({ height: 900, width: 1280 });
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1280)).toBe(true);
    const desktopHeader = await header.boundingBox();
    expect(desktopHeader?.height ?? Number.POSITIVE_INFINITY).toBeLessThanOrEqual(140);

    await page.setViewportSize({ height: 900, width: 1536 });
    await expect(indicator).toHaveText("Waiting on you (1)");
    expect(await inView(indicator, 1536)).toBe(true);
    const wideDetailsRowMiddle = await middle(page.getByTestId("issue-labels"));
    for (const item of [
      indicator,
      openedBy,
      page.getByRole("button", { name: "Messages default to no owner" }),
      subscribers,
      link,
    ]) {
      expect(await middle(item)).toBe(wideDetailsRowMiddle);
    }
  } finally {
    await context.close();
  }
});

test("issue header flips between Waiting on you and Waiting on agents as the newest reply changes", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Whose turn" });
  const ask = await createAsk(issue.key, { question: "Which layout?" }, session);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const indicator = page.getByTestId("issue-whose-turn");
    await expect(indicator).toHaveText("Waiting on you (1)");

    // A human clarification hands the turn to the asker, live and after a reload.
    await createComment(issue.key, { ask_id: ask.id, body: "Which widths matter?" });
    await expect(indicator).toHaveText("Waiting on agents (1)", { timeout: 10_000 });
    await page.reload();
    await expect(indicator).toHaveText("Waiting on agents (1)");

    // The agent's reply hands it back.
    await createComment(issue.key, { ask_id: ask.id, body: "390, 1280 and 1536." }, session);
    await expect(indicator).toHaveText("Waiting on you (1)", { timeout: 10_000 });
  } finally {
    await context.close();
  }
});

test("issue header gives the title the row's free space beside a short details line", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the viewports are set explicitly in the desktop browser project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Header keeps its title readable" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const title = page.getByRole("heading", { level: 1 });
  const measureTitle = () =>
    page.evaluate(() => {
      const heading = document.querySelector("[data-testid=issue-header] h1") as HTMLElement;
      const header = document.querySelector("[data-testid=issue-header]") as HTMLElement;
      const style = getComputedStyle(header);
      return {
        clipped:
          heading.scrollHeight > heading.clientHeight || heading.scrollWidth > heading.clientWidth,
        contentWidth:
          header.clientWidth -
          Number.parseFloat(style.paddingLeft) -
          Number.parseFloat(style.paddingRight),
        width: heading.getBoundingClientRect().width,
      };
    });
  try {
    // Below 1280 the details line shares the row with the title once it fits beside the state
    // controls; the title must still take the rest of the row rather than split it with the line.
    for (const width of [1024, 1200, 1279]) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(title).toHaveText("Header keeps its title readable");
      expect((await measureTitle()).clipped, `title clipped at ${width}px`).toBe(false);
      if (width === 1024) {
        const shot = testInfo.outputPath("issue-header-title-1024.png");
        await page.screenshot({ path: shot });
        await testInfo.attach("issue header title (1024px)", {
          contentType: "image/png",
          path: shot,
        });
      }
    }

    // A busy details line (whose-turn badge, one label, Route, Subscribers) that fits beside a
    // 12rem title but not beside the whole title: just below 1280 the title must keep at least
    // half the card rather than share the row and drop to its minimum. The line without the
    // GitHub link is the one that used to squeeze (with the link it was too wide to share).
    await patchIssue(issue.key, {
      labels: ["api"],
      title: "Migrate the issue header to a single row",
    });
    await createAsk(issue.key, { question: "Which layout?" }, session);
    await subscribeSession(issue.key);
    for (const withLink of [false, true]) {
      await patchIssue(issue.key, {
        external_links: withLink
          ? [{ kind: "github_pr", url: "https://github.com/sjawhar/legion/pull/1051" }]
          : [],
      });
      for (const width of [1265, 1279]) {
        await page.setViewportSize({ height: 800, width });
        await page.goto(`/issues/${issue.key}`);
        await expect(title).toHaveText("Migrate the issue header to a single row");
        await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");
        await expect(page.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
        const measured = await measureTitle();
        const label = `${width}px ${withLink ? "with" : "without"} the GitHub link`;
        expect(measured.clipped, `title clipped at ${label}`).toBe(false);
        expect(
          measured.width,
          `title ${measured.width}px of a ${measured.contentWidth}px card at ${label}`
        ).toBeGreaterThanOrEqual(measured.contentWidth / 2);
      }
    }
  } finally {
    await context.close();
  }
});

test("issue header delivers the click that ends a title edit to the control under the pointer", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name === "iphone",
    "the viewports are set explicitly in the desktop browser project"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Short title" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // A real click is mousedown then mouseup: the mousedown blurs the title editor, and the header
  // must not re-lay out on that blur, or the mouseup lands on a different element and the
  // browser delivers no click to the control the user pressed. The raw mouse sequence skips
  // `click()`'s actionability wait, so wait for the control ourselves: the pin and Close stay
  // `disabled` while their previous write is in flight, and a browser drops a click on a
  // disabled button.
  const pressWhileEditingTitle = async (control: Locator) => {
    await page.getByRole("heading", { level: 1 }).click();
    await expect(page.getByLabel("Issue title")).toBeFocused();
    await expect(control).toBeEnabled();
    const box = await control.boundingBox();
    if (box === null) {
      throw new Error("the control must be visible while the title is being edited");
    }
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.up();
  };
  try {
    for (const width of [1024, 1279]) {
      await page.setViewportSize({ height: 800, width });
      await page.goto(`/issues/${issue.key}`);
      await expect(page.getByRole("heading", { level: 1, name: "Short title" })).toBeVisible();

      const pin = page.getByRole("button", { name: "Pin issue" });
      await pressWhileEditingTitle(pin);
      await expect(page.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
        "aria-pressed",
        "true"
      );
      await pressWhileEditingTitle(page.getByRole("button", { name: "Unpin issue" }));
      await expect(pin).toHaveAttribute("aria-pressed", "false");

      await pressWhileEditingTitle(page.getByRole("button", { name: "Close issue" }));
      await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ status: "done" });
      if (width === 1024) {
        const shot = testInfo.outputPath("issue-header-title-edit-click-1024.png");
        await page.screenshot({ path: shot });
        await testInfo.attach("close clicked straight out of a title edit (1024px)", {
          contentType: "image/png",
          path: shot,
        });
      }
      await patchIssue(issue.key, { status: "triage" });
    }
  } finally {
    await context.close();
  }
});

test("issue header reassigns through the Assignee picker; a personal token's issues go to its owner", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  // A human's issue is assigned to its creator, whatever casing their sign-in carries.
  const issue = await createIssue(
    { project: "CORE", title: "Who answers this?" },
    { login: "Bob" }
  );
  expect(issue.assignee).toBe("bob");

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}`);
    const control = page.getByLabel(`Assignee of ${issue.key}`);
    await expect(control).toHaveValue("bob");
    // The allowlist is read only once the reader reaches for the control.
    await control.focus();
    await expect(control.locator("option")).toHaveText(["Unassigned", "alice", "bob"]);

    // Anyone on the allowlist may reassign: Alice takes it, and the header shows her at once.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await control.selectOption("alice");
    expect((await patch).postDataJSON()).toEqual({ assignee: "alice" });
    await expect(
      page.getByTestId("issue-header").locator("span", { hasText: /^alice$/ })
    ).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ assignee: "alice" });
    const events = await getIssueEvents(issue.key);
    expect(events.filter((event) => event.type === "issue.updated").at(-1)).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { assignee: "alice" },
    });
    const shot = testInfo.outputPath(`issue-header-assignee-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`issue header assignee (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    // Clearing it leaves the issue unassigned, and a reload agrees.
    await control.selectOption("");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ assignee: null });
    await page.reload();
    await expect(page.getByLabel(`Assignee of ${issue.key}`)).toHaveValue("");
    await expect(
      page.getByTestId("issue-header").locator("span", { hasText: /^Unassigned$/ })
    ).toBeVisible();

    // A session acting for Alice through her personal token creates issues assigned to her; a
    // child the shared token (nobody's) creates under her issue inherits her.
    const token = await mintAgentToken("Architect", "alice");
    const agent = {
      actor: { id: "architect-session", kind: "session" as const },
      as: "agent" as const,
      token,
    };
    const hers = await createIssue({ project: "CORE", title: "Minted for Alice" }, agent);
    expect(hers.assignee).toBe("alice");
    const child = await createIssue(
      { parent: hers.key, project: "CORE", title: "Child of Alice's issue" },
      { actor: agent.actor, as: "agent" }
    );
    expect(child.assignee).toBe("alice");
    await page.goto(`/issues/${hers.key}`);
    await expect(page.getByLabel(`Assignee of ${hers.key}`)).toHaveValue("alice");
  } finally {
    await context.close();
  }
});
