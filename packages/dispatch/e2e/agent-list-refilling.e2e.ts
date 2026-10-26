import { expect, type Locator, type Page, test } from "@playwright/test";

import { agentRow, type FakeSession, setInterests, setListsState, setLiveSessions } from "./agents";
import {
  baseUrl,
  createAsk,
  createIssue,
  createProject,
  createProjectDocument,
  getIssue,
  patchIssue,
  userHeaders,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The Envoy listener refuses its two list reads while its copy of a bucket cannot be served: 503,
// `Retry-After` and code `refilling` (the fake's `refilling` state). Dispatch answers that as
// ENVOY_REFILLING, and every view that reads the agent list or an owner's subscribers says the
// list is refilling where it would otherwise say an agent is absent: no "No agents are connected",
// no "may have ended", no "No live agents match", no subscriber or recipient count, no offline
// dot and no no_holder route marker. A listener that could not answer at all (the fake's `down`
// state) is unreachable wording on the same views, never the refilling notice.
//
// The Agents page and an agent's conversation page poll the list every 15 s, so they move from a
// complete read to a refilling one in place, keeping the rows they had; assertions on those two
// pages wait for that poll with `POLL_TIMEOUT`. The other views read again on a reload or a Retry.
// A failed read surfaces only after the shared retry policy's two retries 500 ms apart
// (`web/src/main.tsx`), well inside the default expect timeout. Liveness dots are found by their
// titles (`Live`, `Not live`, `Liveness unknown`), the hook the other follower rows use.

const REFILLING = "The agent list is refilling. Try again in a moment.";
// One 15 s poll, its two retries and a margin, measured from whenever the state flipped.
const POLL_TIMEOUT = 30_000;
const PLANNER_ROLE = "refill-planner";
const DOCUMENT_SLUG = "refill-notes";

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/srv/refill-planner",
  machine_id: "box-refill",
  roles: [PLANNER_ROLE],
  session_id: "refill-planner-session",
  title: "Planner (refill)",
};
const watcher: FakeSession = {
  dir: "/srv/refill-watcher",
  machine_id: "box-refill",
  session_id: "refill-watcher-session",
  title: "Watcher (refill)",
};

interface RefillFixture {
  askId: string;
  issueKey: string;
}

/** One issue routed to the planner's role with an open ask the planner wrote (so the planner
 *  follows it), one project document, and the watcher subscribed to both. `sessions` is what the
 *  listener's list names while it answers. */
async function seedRefillFixture(sessions: FakeSession[]): Promise<RefillFixture> {
  await setLiveSessions(sessions);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refilling route" });
  await patchIssue(issue.key, { route: `role:${PLANNER_ROLE}` });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the refill?" },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );
  await createProjectDocument("CORE", {
    content: "# Refill notes\n\nWho is listening.\n",
    name: "Refill notes",
  });
  await setInterests([
    {
      session_id: watcher.session_id,
      topics: [
        `notifications.dispatch.issue.${issue.key}.>`,
        `notifications.dispatch.document.CORE.${DOCUMENT_SLUG}.>`,
      ],
    },
  ]);
  return { askId: ask.id, issueKey: issue.key };
}

const livePath = `/agents/${planner.session_id}/live`;
const documentPath = `/projects/CORE/documents/${DOCUMENT_SLUG}`;

function refillingNotice(scope: Page | Locator): Locator {
  return scope.getByTestId("agent-list-refilling");
}

/** What the server answers `GET /api/v1/agents`: its status, its `Retry-After` and its code. */
async function agentsAnswer(): Promise<{
  code: unknown;
  retryAfter: string | null;
  status: number;
}> {
  const response = await fetch(new URL("/api/v1/agents", baseUrl), {
    headers: await userHeaders(),
  });
  const body = (await response.json()) as { code?: unknown };
  return {
    code: body.code,
    retryAfter: response.headers.get("Retry-After"),
    status: response.status,
  };
}

/** No view on the page claims an agent is absent. Every assertion is a count of zero, so a caller
 *  first waits for what the page does say. */
async function expectNoAbsenceClaim(page: Page): Promise<void> {
  await expect(page.getByText("No agents are connected.")).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Agents empty state" })).toHaveCount(0);
  await expect(page.getByRole("region", { name: "Replied, no longer connected" })).toHaveCount(0);
  await expect(page.getByText("No live agents match.")).toHaveCount(0);
  await expect(page.getByText(/may have ended/)).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Subscribers: \d/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Reaches \d/ })).toHaveCount(0);
  await expect(page.locator("[title='Not live']")).toHaveCount(0);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
}

/** The page says nothing of an unreachable listener. */
async function expectNoUnreachableWording(page: Page): Promise<void> {
  await expect(page.getByText(/Envoy listener unreachable/)).toHaveCount(0);
  await expect(page.getByText(/Could not (load|refresh) agents/)).toHaveCount(0);
  await expect(page.getByTestId("agent-list-unreachable")).toHaveCount(0);
  await expect(page.getByTestId("issue-route-unknown")).toHaveCount(0);
}

/** The page says nothing of a refilling list. */
async function expectNoRefillingWording(page: Page): Promise<void> {
  await expect(refillingNotice(page)).toHaveCount(0);
  await expect(page.getByText(REFILLING)).toHaveCount(0);
  await expect(page.getByTestId("issue-route-refilling")).toHaveCount(0);
}

async function openIssueConversation(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/conversation`);
  await expect(page.getByRole("form", { name: "Comment composer" })).toBeVisible();
}

/** Opens the issue composer's mention suggestions afresh. Opening them reads the agent list
 *  again (`MentionComposer`), so a list that changed since the last open is the one shown. */
async function openMentionSuggestions(page: Page): Promise<Locator> {
  const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
  await field.fill("");
  await field.fill("@");
  const suggestions = page.getByRole("listbox", { name: "Mention suggestions" });
  await expect(suggestions).toBeVisible();
  return suggestions;
}

function askRecipients(page: Page, askId: string): Locator {
  return page.getByTestId(`ask-${askId}`).getByRole("region", { name: "Recipients" });
}

async function openProjectList(page: Page, issueKey: string): Promise<void> {
  await page.goto("/projects/CORE/issues");
  await expect(page.locator(`[data-issue-row]`).filter({ hasText: issueKey })).toHaveCount(1);
}

async function openProjectBoard(page: Page, issueKey: string): Promise<void> {
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.locator(`[data-board-card="${issueKey}"]`)).toHaveCount(1);
}

/** Every refilling view a fresh page shows, after a reload or a first visit: the issue header's
 *  subscribers and route, the mention suggestions, the document's subscribers, the ask card's
 *  recipients, and the project list and board. */
async function expectFreshViewsRefilling(page: Page, fixture: RefillFixture): Promise<void> {
  await openIssueConversation(page, fixture.issueKey);
  // The issue's ask card shows its own recipients notice in the Conversation and the margin.
  const header = page.getByTestId("issue-header");
  await expect(refillingNotice(header)).toHaveText(REFILLING);
  await expect(refillingNotice(header)).toHaveAttribute("role", "status");
  await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
  await expect(page.getByTestId("issue-route-refilling")).toHaveText(
    `Route reach unknown: ${REFILLING}`
  );
  const suggestions = await openMentionSuggestions(page);
  await expect(refillingNotice(suggestions)).toHaveText(REFILLING);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto(documentPath);
  await expect(refillingNotice(page)).toHaveText(REFILLING);
  await expect(page.getByRole("region", { name: "Subscribed agents" })).toHaveCount(0);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await page.goto("/");
  const recipients = askRecipients(page, fixture.askId);
  await expect(refillingNotice(recipients)).toHaveText(REFILLING);
  const toggle = recipients.getByRole("button", { name: "Recipients", exact: true });
  await toggle.click();
  await expect(
    recipients.getByRole("list", { name: "Followers" }).locator("[title='Liveness unknown']")
  ).toHaveCount(1);
  await expectNoAbsenceClaim(page);
  await expectNoUnreachableWording(page);

  await openProjectList(page, fixture.issueKey);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
  await openProjectBoard(page, fixture.issueKey);
  await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a list that starts refilling after a complete read claims no agent absent, and recovers", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const fixture = await seedRefillFixture([planner, watcher]);
  const alice = await asUser(browser, "alice");
  try {
    // A complete read: every view names the agents and counts what it reaches.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);

    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expect(livePage.getByText("box-refill · /srv/refill-planner")).toBeVisible();
    await expectNoRefillingWording(livePage);

    const issuePage = await alice.newPage();
    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-unknown")).toHaveCount(0);
    await expect(issuePage.getByTestId("issue-route-unreachable")).toHaveCount(0);
    const before = await openMentionSuggestions(issuePage);
    await expect(before.getByRole("option", { name: planner.title })).toBeVisible();
    await expectNoRefillingWording(issuePage);

    const page = await alice.newPage();
    await page.goto(documentPath);
    const subscribed = page.getByRole("region", { name: "Subscribed agents" });
    await expect(subscribed.getByText(watcher.title, { exact: true })).toBeVisible();
    await expect(subscribed.locator("[title='Live']")).toHaveCount(1);
    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await expect(recipients.getByRole("button", { name: "Reaches 2" })).toBeVisible();

    await setListsState("refilling");

    // The open composer reads the list again as its suggestions open: the rows it had stay, and
    // the refilling notice stands where "No live agents match." would.
    const during = await openMentionSuggestions(issuePage);
    await expect(refillingNotice(during)).toHaveText(REFILLING);
    await expect(during.getByRole("option", { name: planner.title })).toBeVisible();
    await expect(issuePage.getByText("No live agents match.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    // The two polling pages refreshed in place: rows and details from the complete read stay,
    // under the refilling notice, and nothing reads as gone or unreachable.
    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expect(livePage.getByText("box-refill · /srv/refill-planner")).toBeVisible();
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);

    // The ask card read while the list refilled is the one a Retry brings back.
    await page.goto("/");
    const refilled = askRecipients(page, fixture.askId);
    await expect(refillingNotice(refilled)).toHaveText(REFILLING);

    await setListsState("ok");

    // Recovery: the polls take the complete read back, and a Retry or a reload counts again.
    await expect(refillingNotice(agentsPage)).toHaveCount(0, { timeout: POLL_TIMEOUT });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expect(refillingNotice(livePage)).toHaveCount(0, { timeout: POLL_TIMEOUT });
    await expect(livePage.getByText("box-refill · /srv/refill-planner")).toBeVisible();

    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-refilling")).toHaveCount(0);
    await expect(issuePage.getByTestId("issue-route-unknown")).toHaveCount(0);
    await expect(
      (await openMentionSuggestions(issuePage)).getByRole("option", { name: planner.title })
    ).toBeVisible();
    await expectNoRefillingWording(issuePage);

    await refilled.getByRole("button", { name: "Retry" }).click();
    await expect(refilled.getByRole("button", { name: "Reaches 2" })).toBeVisible();
    await expectNoRefillingWording(page);

    await page.goto(documentPath);
    await expect(
      page.getByRole("region", { name: "Subscribed agents" }).locator("[title='Live']")
    ).toHaveCount(1);
    await expectNoRefillingWording(page);
  } finally {
    await alice.close();
  }
});

test("an empty list that starts refilling never says nobody is connected", async ({ browser }) => {
  test.setTimeout(150_000);
  const fixture = await seedRefillFixture([]);
  const alice = await asUser(browser, "alice");
  try {
    // A complete, empty read is a true absence, and each view says so.
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentsPage.getByRole("region", { name: "Agents empty state" })).toHaveText(
      "No agents are connected."
    );

    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expect(
      livePage.getByText("Not in the live session list — it may have ended.")
    ).toBeVisible();

    const issuePage = await alice.newPage();
    await openIssueConversation(issuePage, fixture.issueKey);
    await expect(issuePage.getByRole("button", { name: "Subscribers: 1" })).toBeVisible();
    await expect(issuePage.getByTestId("issue-route-unreachable")).toContainText(
      "Nobody holds it right now"
    );
    const before = await openMentionSuggestions(issuePage);
    await expect(before.getByText("No live agents match.")).toBeVisible();

    const page = await alice.newPage();
    await page.goto(documentPath);
    await expect(
      page.getByRole("region", { name: "Subscribed agents" }).locator("[title='Not live']")
    ).toHaveCount(1);
    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await recipients.getByRole("button", { name: "Reaches 2" }).click();
    await expect(recipients.locator("[title='Not live']")).toHaveCount(2);
    await openProjectList(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(1);
    await openProjectBoard(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(1);
    // The view choice is remembered; the refilling pass opens the List first, as here.
    await page.getByRole("button", { name: "List" }).click();

    await setListsState("refilling");

    const during = await openMentionSuggestions(issuePage);
    await expect(refillingNotice(during)).toHaveText(REFILLING);
    await expect(issuePage.getByText("No live agents match.")).toHaveCount(0);

    await expectFreshViewsRefilling(page, fixture);

    await expect(refillingNotice(agentsPage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expect(agentsPage.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(agentsPage);
    await expectNoUnreachableWording(agentsPage);
    await expect(refillingNotice(livePage)).toHaveText(REFILLING, { timeout: POLL_TIMEOUT });
    await expectNoAbsenceClaim(livePage);
    await expectNoUnreachableWording(livePage);
  } finally {
    await alice.close();
  }
});

test("a listener refilling from the first read shows every view refilling", async ({ browser }) => {
  test.setTimeout(90_000);
  const fixture = await seedRefillFixture([planner, watcher]);
  await setListsState("refilling");
  // What every view below reads: Dispatch's own refilling answer, not an unreachable listener.
  expect(await agentsAnswer()).toEqual({ code: "ENVOY_REFILLING", retryAfter: "5", status: 503 });
  expect(await getIssue(fixture.issueKey)).toMatchObject({
    route_status: "unknown",
    route_status_reason: "refilling",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(refillingNotice(page)).toHaveText(REFILLING);
    await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expectNoAbsenceClaim(page);
    await expectNoUnreachableWording(page);

    await page.goto(livePath);
    await expect(refillingNotice(page)).toHaveText(REFILLING);
    await expect(page.getByTestId("agent-list-loading")).toHaveCount(0);
    await expectNoAbsenceClaim(page);
    await expectNoUnreachableWording(page);

    await expectFreshViewsRefilling(page, fixture);
  } finally {
    await alice.close();
  }
});

test("a listener that cannot answer reads as unreachable on every view, never as refilling", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  const fixture = await seedRefillFixture([planner, watcher]);
  const alice = await asUser(browser, "alice");
  try {
    const agentsPage = await alice.newPage();
    await agentsPage.goto("/agents");
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    const livePage = await alice.newPage();
    await livePage.goto(livePath);
    await expect(livePage.getByText("box-refill · /srv/refill-planner")).toBeVisible();

    await setListsState("down");
    expect(await agentsAnswer()).toEqual({
      code: "ENVOY_UNAVAILABLE",
      retryAfter: null,
      status: 503,
    });
    expect(await getIssue(fixture.issueKey)).toMatchObject({
      route_status: "unknown",
      route_status_reason: "unreachable",
    });

    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByText(/^Could not load agents: /)).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(livePath);
    await expect(page.getByTestId("agent-list-unreachable")).toHaveText(
      "Could not check the live session list — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await openIssueConversation(page, fixture.issueKey);
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." })
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Subscribers", exact: true })).toBeVisible();
    await expect(page.getByTestId("issue-route-unknown")).toHaveText("Route reach unknown");
    const suggestions = await openMentionSuggestions(page);
    await expect(suggestions.getByTestId("agent-list-unreachable")).toHaveText(
      "Could not load live agents — Envoy listener unreachable."
    );
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto(documentPath);
    await expect(
      page
        .getByRole("alert")
        .filter({ hasText: "Subscribed agents unavailable — Envoy listener unreachable." })
    ).toBeVisible();
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await page.goto("/");
    const recipients = askRecipients(page, fixture.askId);
    await expect(
      recipients
        .getByRole("alert")
        .filter({ hasText: "Subscribers unavailable — Envoy listener unreachable." })
    ).toBeVisible();
    await recipients.getByRole("button", { name: "Recipients", exact: true }).click();
    await expect(
      recipients.getByRole("list", { name: "Followers" }).locator("[title='Liveness unknown']")
    ).toHaveCount(1);
    await expectNoRefillingWording(page);
    await expectNoAbsenceClaim(page);

    await openProjectList(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);
    await openProjectBoard(page, fixture.issueKey);
    await expect(page.getByTestId("issue-route-unreachable")).toHaveCount(0);

    // The polling pages refreshed in place: they keep the rows they had and say the refresh
    // failed, not that the list is refilling.
    await expect(agentsPage.getByRole("alert")).toHaveText(/^Could not refresh agents: /, {
      timeout: POLL_TIMEOUT,
    });
    await expect(agentRow(agentsPage, planner.session_id)).toBeVisible();
    await expectNoRefillingWording(agentsPage);
    await expectNoAbsenceClaim(agentsPage);
    await expect(livePage.getByTestId("agent-list-unreachable")).toBeVisible({
      timeout: POLL_TIMEOUT,
    });
    await expect(livePage.getByText("box-refill · /srv/refill-planner")).toBeVisible();
    await expectNoRefillingWording(livePage);
    await expectNoAbsenceClaim(livePage);
  } finally {
    await alice.close();
  }
});
