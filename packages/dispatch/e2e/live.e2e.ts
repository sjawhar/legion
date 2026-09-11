import { expect, test } from "@playwright/test";

import { createAsk, createIssue, createMessage, createProject, disconnectAllStreams } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// These scenarios exercise the event stream fanning updates into other open tabs, so
// the counterparty ("bob") is an autonomous session acting through the bearer API —
// only alice's browser needs to be real, since that is what proves the live update.
const bob = { actor: { kind: "session" as const, id: "bob" }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

test("live: bob's ask appears in alice's inbox and sidebar without reload", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const readingIssue = await createIssue({ project: "CORE", title: "Reading" });
  const targetIssue = await createIssue({ project: "CORE", title: "Needs an answer" });

  const alice = await asUser(browser, "alice");
  const inboxPage = await alice.newPage();
  await inboxPage.goto("/");
  await expect(inboxPage.getByText("Nothing needs you")).toBeVisible();

  const issuePage = await alice.newPage();
  await issuePage.goto(`/issues/${readingIssue.key}`);
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Open navigation" }).click();
  }
  // With zero items, #844 hides the whole "Needs you" group (heading included) —
  // rather than rendering it empty. The issue instead sits, un-flagged, in
  // "Everything else".
  await expect(issuePage.getByRole("heading", { exact: true, name: "Needs you" })).toHaveCount(0);
  const nav = issuePage.getByRole("navigation", { name: "Issues" });
  await expect(nav.locator("li", { hasText: targetIssue.key })).toBeVisible();

  const ask = await createAsk(
    targetIssue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it?" },
    bob
  );

  // alice never navigated — the sidebar on readingIssue's page and the inbox on a
  // separate tab both pick up bob's ask purely from the event stream.
  await expect(issuePage.getByRole("heading", { name: "Needs you (1)" })).toBeVisible();
  const needsYou = issuePage.locator("section", {
    has: issuePage.getByRole("heading", { name: /^Needs you/ }),
  });
  await expect(needsYou.getByText(targetIssue.key, { exact: true })).toBeVisible();
  expect(issuePage.url()).toContain(readingIssue.key);

  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toBeVisible();
  await expect(inboxPage.getByText("Ship it?")).toBeVisible();

  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("sidebar-needs-you-live.png"),
  });
  await inboxPage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("inbox-new-ask-live.png"),
  });

  await alice.close();
});

test("live: answering an ask moves the issue out of Needs you immediately", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const readingIssue = await createIssue({ project: "CORE", title: "Reading" });
  const targetIssue = await createIssue({ project: "CORE", title: "Needs an answer" });
  const ask = await createAsk(
    targetIssue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it?" },
    bob
  );

  const alice = await asUser(browser, "alice");
  const inboxPage = await alice.newPage();
  await inboxPage.goto("/");
  const issuePage = await alice.newPage();
  await issuePage.goto(`/issues/${readingIssue.key}`);
  if (testInfo.project.name === "iphone") {
    await issuePage.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(issuePage.getByRole("heading", { name: "Needs you (1)" })).toBeVisible();

  await inboxPage.getByTestId(`ask-${ask.id}`).getByRole("radio", { name: "Yes" }).check();
  await inboxPage
    .getByTestId(`ask-${ask.id}`)
    .getByRole("button", { name: "Submit answer" })
    .click();
  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toHaveCount(0);

  // The answer happened on a different tab (inboxPage) than the one displaying the
  // sidebar (issuePage) — the row moving proves the update came from the stream, not
  // from the mutation's own optimistic cache update.
  // The group empties back out to zero items, so #844 hides it (and its heading)
  // entirely rather than rendering it with a "(0)" or bare label.
  await expect(issuePage.getByRole("heading", { exact: true, name: "Needs you" })).toHaveCount(0);
  const nav = issuePage.getByRole("navigation", { name: "Issues" });
  const targetRow = nav.locator("li", { hasText: targetIssue.key });
  await expect(targetRow).toBeVisible();
  await expect(targetRow.getByText(/^\d+$/)).toHaveCount(0);
  expect(issuePage.url()).toContain(readingIssue.key);

  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("sidebar-after-answer-live.png"),
  });

  await alice.close();
});

test("live: new events on the open issue appear in Conversation and mark it read as they are viewed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Conversation target" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
    "aria-selected",
    "true"
  );

  await createMessage(issue.key, { body: "Live message from bob" }, bob);

  await expect(page.getByText("Live message from bob")).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(async (key) => {
        const response = await fetch("/api/v1/me/state");
        const state = (await response.json()) as Record<string, { last_read_seq: number }>;
        return state[key]?.last_read_seq ?? 0;
      }, issue.key)
    )
    .toBeGreaterThan(0);

  await alice.close();
});

test("live: the stream recovers after the connection drops", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Reconnect target" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto("/");
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  await expect(page.getByText("Reconnecting")).toHaveCount(0);

  // D10: the pill must never move the reading surface. It has to render as an
  // overlay (not push content in flow), so the main heading's box stays put both
  // while it appears and after it goes away.
  const heading = page.getByRole("heading", { name: "Inbox" });
  const boundsBeforeOutage = await heading.boundingBox();

  await alice.setOffline(true);
  await expect(page.getByText("Reconnecting")).toBeVisible();
  expect(await heading.boundingBox()).toEqual(boundsBeforeOutage);
  await page.screenshot({ fullPage: true, path: testInfo.outputPath("reconnecting-pill.png") });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Missed while offline?" },
    bob
  );
  // The scenario calls for a real outage window; there is no event to await here
  // because the point under test is that nothing arrives while genuinely offline.
  await page.waitForTimeout(3_000);

  await alice.setOffline(false);

  await expect(page.getByText("Reconnecting")).toHaveCount(0);
  await expect(page.getByTestId(`ask-${ask.id}`)).toBeVisible();
  await expect(page.getByText("Missed while offline?")).toBeVisible();
  expect(await heading.boundingBox()).toEqual(boundsBeforeOutage);

  await alice.close();
});

test("live: a forced server disconnect reconnects from the last event id, not from zero", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Disconnect target" });
  await createMessage(issue.key, { body: "Before disconnect" }, bob);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const streamRequestUrls: string[] = [];
  page.on("request", (request) => {
    if (/\/api\/v1\/events(\?|$)/.test(request.url())) {
      streamRequestUrls.push(request.url());
    }
  });

  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(page.getByText("Before disconnect")).toBeVisible();
  // A live message the client actually observes before the outage — the cold
  // connect itself resolves its head internally without reporting it back, so
  // without this the reconnect below could not tell "nothing has happened yet"
  // apart from "many events happened and none reached me".
  await createMessage(issue.key, { body: "Seen live before disconnect" }, bob);
  await expect(page.getByText("Seen live before disconnect")).toBeVisible();
  await expect.poll(() => streamRequestUrls.length).toBeGreaterThanOrEqual(1);

  // Ends every open SSE response on the server, exactly like a real server restart
  // or a dropped connection the OS never reports as offline — the same recovery
  // path the watchdog exercises after 45s of silence, proven here instantly.
  await disconnectAllStreams();

  await createMessage(issue.key, { body: "Missed during disconnect" }, bob);
  await expect(page.getByText("Missed during disconnect")).toBeVisible();
  await expect.poll(() => streamRequestUrls.length).toBeGreaterThanOrEqual(2);

  // The reconnect resumes from the last event id this page actually saw, not a
  // full since=0 replay of the issue's history.
  const reconnectSince = new URL(streamRequestUrls[streamRequestUrls.length - 1]).searchParams.get(
    "since"
  );
  expect(reconnectSince).not.toBe("0");
  expect(Number(reconnectSince)).toBeGreaterThan(0);

  await alice.close();
});

test("live: a fresh page load opens the stream at the current head and stays within a bounded request budget", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Budget target" });
  for (let index = 0; index < 5; index += 1) {
    await createMessage(issue.key, { body: `Backlog message ${index}` }, bob);
  }

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const apiRequestUrls: string[] = [];
  page.on("request", (request) => {
    const url = request.url();
    if (url.includes("/api/v1/") || url.includes("/auth/")) {
      apiRequestUrls.push(url);
    }
  });

  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(page.getByText("Backlog message 4")).toBeVisible();

  const streamRequest = apiRequestUrls.find((url) => /\/api\/v1\/events(\?|$)/.test(url));
  expect(streamRequest).toBeDefined();

  // The very first connection ever omits since entirely — the server resolves
  // its own current head after subscribing, instead of the client computing one
  // via a separate request and passing it here.
  const since = new URL(streamRequest ?? "").searchParams.get("since");
  expect(since).toBeNull();

  // Exactly 11 on desktop (chromium): whoami, issues, me/state, issue detail, the shared inbox
  // query for the margin's open-ask count, the stream connection, two active-sessions/Conversation
  // event reads, the primary artifact's comments, the margin's own issue-asks list, and
  // Conversation's live-agent query. The phone project (iphone) does not fetch the bare
  // `/api/v1/issues` list because its closed drawer has no sidebar, so it uses 10. Asserted exactly
  // (not a ceiling) so a panel that starts eagerly fetching before its tab is ever opened — e.g.
  // Spec's ProofDocument or Children — trips this immediately instead of only breaking some looser
  // upper bound.
  expect(apiRequestUrls.length).toBe(testInfo.project.name === "iphone" ? 10 : 11);

  await alice.close();
});

test("live: the Reconnecting pill never covers the phone margin sheet toggle", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the margin sheet toggle only renders as a fixed bottom bar on phone; desktop hides it via md:hidden"
  );
  await createProject({ key: "CORE", name: "Core" });
  // #844 hides the margin sheet's `<aside>` (and its toggle) entirely when no
  // issue is open, so the toggle only exists on an issue page.
  const issue = await createIssue({ project: "CORE", title: "Margin toggle target" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}`);

  const toggle = page.getByRole("button", { name: /Open review panel/ });
  await expect(toggle).toBeVisible();

  await alice.setOffline(true);
  const pill = page.getByTestId("connection-pill");
  await expect(pill).toBeVisible();

  const toggleBox = await toggle.boundingBox();
  const pillBox = await pill.boundingBox();
  expect(toggleBox).not.toBeNull();
  expect(pillBox).not.toBeNull();
  if (toggleBox !== null && pillBox !== null) {
    const intersects =
      toggleBox.x < pillBox.x + pillBox.width &&
      toggleBox.x + toggleBox.width > pillBox.x &&
      toggleBox.y < pillBox.y + pillBox.height &&
      toggleBox.y + toggleBox.height > pillBox.y;
    expect(intersects).toBe(false);
  }

  await page.screenshot({
    fullPage: true,
    path: testInfo.outputPath("pill-clears-sheet-toggle.png"),
  });

  await alice.setOffline(false);
  await alice.close();
});
