import { expect, test } from "@playwright/test";

import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, getIssue, getIssueEvents, patchIssue } from "./api";
import { resetDatabase } from "./seed";
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

test("issue header persists its title, status, and route", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First decision" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
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
    await heading.click();
    const issueTitle = page.getByLabel("Issue title");
    await issueTitle.fill("First decision revised");
    await issueTitle.press("Enter");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ title: "First decision revised" });
    await page.getByLabel("Status").selectOption("todo");
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ status: "todo" });
    await page.getByRole("button", { name: "Messages default to no route" }).click();
    await page.getByLabel("Route").fill("role:legion-controller-core");
    await page.getByRole("button", { name: "Save route" }).click();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        route: "role:legion-controller-core",
      });
    await page
      .getByRole("button", { name: "Messages default to role:legion-controller-core" })
      .click();
    const routeInput = page.getByLabel("Route");
    await routeInput.fill("");
    // The field must still read "" when the earlier PATCH's response has been applied;
    // a revert here (not a wrong request body) is the race this test guards.
    await expect(page.getByRole("button", { name: "Save route" })).toBeEnabled();
    await expect(routeInput).toHaveValue("");
    await page.getByRole("button", { name: "Save route" }).click();
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
  await patchIssue(issue.key, {
    external_links: [{ kind: "github_issue", url: "https://github.com/sjawhar/legion/issues/815" }],
  });
  await createAsk(issue.key, { question: "First ask" }, session);
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([{ session_id: "e2e-session", title: "e2e-session-title" }]);
    await setInterests([
      {
        session_id: "e2e-session",
        topics: [
          `notifications.dispatch.issue.${issue.key}`,
          `notifications.dispatch.issue.${issue.key}.>`,
        ],
      },
    ]);
  }
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
    await expect(
      page.getByRole("link", { name: "https://github.com/sjawhar/legion/issues/815" })
    ).toHaveAttribute("title", "GitHub details are unavailable for this sign-in.");
    await page.getByRole("tab", { name: "Children" }).click();
    await expect(
      page.getByRole("tabpanel").getByRole("link", { name: `${child.key} · Child decision` })
    ).toBeVisible();
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
    await askCard.getByRole("button", { name: "Ship" }).click();
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
    await page.goto("/");
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Close navigation" }).click();
    }
    await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    const bob = await asUser(browser, "bob");
    const bobPage = await bob.newPage();
    try {
      await bobPage.goto("/");
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
