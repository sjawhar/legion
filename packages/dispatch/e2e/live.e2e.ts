import { expect, type Locator, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  disconnectAllStreams,
  getIssue,
  patchIssue,
  putArchitectureSource,
  putIssueState,
  resolveAsk,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// These scenarios exercise the event stream fanning updates into other open tabs, so
// the counterparty ("bob") is an autonomous session acting through the bearer API —
// only alice's browser needs to be real, since that is what proves the live update.
const bob = { actor: { kind: "session" as const, id: "bob" }, as: "agent" as const };
const eventStreamUrl = /\/api\/v1\/events(\?|$)/;
let renames = 0;

/**
 * bob renames the open issue, again every 2 s, until its header shows the new name from a read
 * of `GET /api/v1/issues/<key>` the page sent after the write (`reads` counts them). Only the
 * page's event stream can bring either, so past this the stream is subscribed and every event
 * written before the rename has been handled.
 */
async function renameUntilShown(
  header: Locator,
  key: string,
  title: string,
  reads: () => number
): Promise<void> {
  for (let attempt = 0; attempt < 10; attempt += 1) {
    renames += 1;
    const renamed = `${title} (rename ${renames})`;
    const loads = reads();
    await patchIssue(key, { title: renamed }, bob);
    const shown = await expect(header)
      .toHaveText(renamed, { timeout: 2_000 })
      .then(() => true)
      .catch(() => false);
    if (shown && reads() > loads) return;
  }
  throw new Error(`no rename of ${key} reached the page in 10 writes 2 s apart`);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("live: bob's ask updates alice's Inbox and project badges without reload", async ({
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
  // The sidebar now provides project navigation; an open ask updates its Inbox and
  // project badges without changing the issue route in the other tab.
  const navigation = issuePage.getByRole("navigation", { name: "Navigation" });
  await expect(navigation.locator('a[href="/projects/CORE"]')).toBeVisible();

  const ask = await createAsk(
    targetIssue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Ship it?" },
    bob
  );

  await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("1");
  await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("1");
  expect(issuePage.url()).toContain(readingIssue.key);

  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toBeVisible();
  await expect(inboxPage.getByText("Ship it?")).toBeVisible();

  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("sidebar-project-badge-live.png"),
  });
  await inboxPage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("inbox-new-ask-live.png"),
  });

  await alice.close();
});

test("live: answering an ask updates the Inbox and project badges immediately", async ({
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
  const navigation = issuePage.getByRole("navigation", { name: "Navigation" });
  await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("1");
  await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("1");

  await inboxPage.getByTestId(`ask-${ask.id}`).getByRole("radio", { name: "Yes" }).check();
  await inboxPage.getByTestId(`ask-${ask.id}`).getByRole("button", { name: "Answer" }).click();
  await expect(inboxPage.getByTestId(`ask-${ask.id}`)).toHaveCount(0);

  // The answer happened on a different tab (inboxPage) than the one displaying the
  // sidebar (issuePage), so both badges changing proves the update came from the
  // stream rather than from the mutation's optimistic cache update.
  await expect(navigation.getByRole("link", { name: "Inbox" })).toHaveText("Inbox");
  await expect(navigation.locator('a[href="/projects/CORE"]')).not.toContainText("1");
  expect(issuePage.url()).toContain(readingIssue.key);

  await issuePage.screenshot({
    fullPage: true,
    path: testInfo.outputPath("sidebar-after-answer-live.png"),
  });

  await alice.close();
});

test("live: a message on another issue reads neither the Inbox nor the pinned list", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const viewedIssue = await createIssue({ project: "CORE", title: "Viewed issue" });
  const otherIssue = await createIssue({ project: "CORE", title: "Unviewed issue" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let inboxReads = 0;
  let pinnedReads = 0;
  let viewedReads = 0;
  page.on("request", (request) => {
    if (request.method() !== "GET") return;
    const url = new URL(request.url());
    if (url.pathname === "/api/v1/inbox") inboxReads += 1;
    if (url.pathname === "/api/v1/issues" && url.searchParams.get("pinned") === "true") {
      pinnedReads += 1;
    }
    if (url.pathname === `/api/v1/issues/${viewedIssue.key}`) viewedReads += 1;
  });

  try {
    await page.goto(`/issues/${viewedIssue.key}`);
    if (testInfo.project.name === "iphone") {
      // The pinned list lives in the drawer; open it so both projects measure the same queries.
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await expect(
      page.getByRole("navigation", { name: "Navigation" }).locator('a[href="/projects/CORE"]')
    ).toBeVisible();
    const viewedHeader = page.getByTestId("issue-header").getByRole("heading", { level: 1 });
    // The first rename proves the stream is subscribed, so the message below reaches this page.
    await renameUntilShown(viewedHeader, viewedIssue.key, "Viewed issue", () => viewedReads);
    await page.waitForTimeout(250);
    inboxReads = 0;
    pinnedReads = 0;

    await createMessage(otherIssue.key, { body: "Unviewed issue update" }, bob);
    // Events reach the page in order, so once a rename written after the message shows, the
    // message's event has been handled too. A rename moves neither read: the issue has no pinned
    // row and no Inbox row.
    await renameUntilShown(viewedHeader, viewedIssue.key, "Viewed issue", () => viewedReads);
    // The 100 ms batch and any straggling read settle here, so a late one still counts.
    await page.waitForTimeout(1_200);

    expect([inboxReads, pinnedReads]).toEqual([0, 0]);
  } finally {
    await alice.close();
  }
});

test("live: an agent message makes a read issue appear in the project's Unread filter", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unread after agent message" });
  await putIssueState(issue.key, { last_read_seq: (await getIssue(issue.key)).last_seq });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/projects/CORE?unread=1");
    await expect(page.getByRole("heading", { name: "Core" })).toBeVisible();
    await expect(page.getByRole("region", { exact: true, name: "Project issues" })).toBeVisible();
    await expect(page.getByText("Unread after agent message")).toHaveCount(0);

    await createMessage(issue.key, { body: "This should make the issue unread" }, bob);

    await expect(page.getByText("Unread after agent message")).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("live: closing a grandchild updates every loaded ancestor's Children rollup", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const root = await createIssue({ project: "CORE", title: "Root" });
  const child = await createIssue({ parent: root.key, project: "CORE", title: "Child" });
  const grandchild = await createIssue({ parent: child.key, project: "CORE", title: "Grandchild" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto(`/issues/${root.key}/children`);
    const childRow = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · Child` });
    await expect(childRow).toContainText("0/2 done");

    await patchIssue(grandchild.key, { status: "done" });

    await expect(childRow).toContainText("1/2 done");
  } finally {
    await alice.close();
  }
});

test("live: resolving an ask updates its pinned issue's open-ask count", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pinned ask" });
  const ask = await createAsk(issue.key, { question: "Still open?" }, bob);
  await putIssueState(issue.key, { pinned: true });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  let pinnedReads = 0;
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (
      request.method() === "GET" &&
      url.pathname === "/api/v1/issues" &&
      url.search === "?pinned=true"
    ) {
      pinnedReads += 1;
    }
  });
  try {
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
    const pinned = page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("link", { name: new RegExp(`${issue.key}.*Pinned ask`) });
    await expect(pinned).toContainText("1");
    pinnedReads = 0;

    await resolveAsk(ask.id, { kind: "retracted", reason: "No longer needed" }, bob);

    await expect.poll(() => pinnedReads).toBe(1);
    await expect(pinned).toHaveText(`${issue.key}Pinned ask`);
  } finally {
    await alice.close();
  }
});

test("live: another request's source update refreshes Settings without reload", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({ "legion/arch": { contents: "read", installation_id: 101 } });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/settings");
    await expect(page.getByText("No architecture sources yet.")).toBeVisible();

    await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });

    await expect(page.getByRole("cell", { exact: true, name: "legion/arch" })).toBeVisible();
  } finally {
    await alice.close();
  }
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
  const heading = page.locator("main").getByRole("heading", { name: "Inbox" });
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
    if (eventStreamUrl.test(request.url())) {
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

test("live: an event committed while the stream's first attempt fails shows once the retry opens", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "First attempt fails" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const { promise: released, resolve: release } = Promise.withResolvers<void>();
    // Until the document's Conversation read lands, every stream attempt it makes fails the way a
    // network change aborts one, so its first attempt never opens; later attempts are held until
    // the comment below is committed, so the retry's cold connect starts at a head past it. The
    // state is per document, since a chunk download a network change aborts reloads the page.
    let attemptFailed = false;
    let readLanded = false;
    page.on("request", (request) => {
      if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
        attemptFailed = false;
        readLanded = false;
      }
    });
    page.on("response", (response) => {
      if (new URL(response.url()).pathname === `/api/v1/issues/${issue.key}/events`) {
        readLanded = true;
      }
    });
    await page.route(eventStreamUrl, async (route) => {
      if (!readLanded) {
        attemptFailed = true;
        await route.abort("failed");
        return;
      }
      await released;
      await route.continue();
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect.poll(() => attemptFailed && readLanded).toBe(true);
    // Committed after the Conversation's read and before any stream is open: nothing the page has
    // read or will be sent includes it.
    const comment = await createComment(
      issue.key,
      { body: "Committed before the retry opened" },
      bob
    );
    const opened = page.waitForResponse((response) => eventStreamUrl.test(response.url()));
    release();
    await opened;

    await expect(page.locator(`[data-turn="comment:${comment.id}"]`)).toContainText(
      "Committed before the retry opened"
    );
  } finally {
    await alice.close();
  }
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

  const streamRequest = apiRequestUrls.find((url) => eventStreamUrl.test(url));
  expect(streamRequest).toBeDefined();

  // The very first connection ever omits since entirely — the server resolves
  // its own current head after subscribing, instead of the client computing one
  // via a separate request and passing it here.
  const since = new URL(streamRequest ?? "").searchParams.get("since");
  expect(since).toBeNull();

  // Exactly 21 on desktop (chromium): whoami, the project sidebar's Inbox, Pinned,
  // and Projects queries, me/state, issue detail, the shared inbox query for the
  // margin's open-ask count, the stream connection, the active-sessions read,
  // the primary artifact's comments - which the Conversation's own list shares rather
  // than fetching again - the margin's own list of the issue's asks and the
  // Conversation's, which asks the same route for each ask's thread (`?thread=1`) and so
  // is a request of its own, not the margin's shared,
  // the issue's message threads, the two one-row event reads the unread mark
  // needs (the first event past the watermark, and the issue's head),
  // Conversation's live-agent query, the one block-schema fetch every document
  // surface shares for the session, the header's read of the project's
  // architecture sources (the `Components:` line offers its picker only when the
  // project has one; the component tree itself is fetched only once the picker
  // opens, never on load), the header's read of the repository mappings (an issue
  // that resolves to no repository, as this one does, shows the `Repository:` item only
  // when its project maps one), me/agents/state, whose unread replies the Agents badge
  // counts on every page (the sidebar's on desktop, the compact header's on phone), and
  // the credential requests waiting on the viewer, which the Needs-you badge counts beside
  // the inbox on every page, and `me/replies?limit=1`, whose total the Replies badge counts on
  // every page (the sidebar's on desktop, the compact header's on phone).
  // The Conversation pages no event log at all now that the history is the Activity tab's,
  // and the Activity panel is unmounted until its tab is opened, so it adds nothing here.
  // The header's assignee picker reads who has signed in only once the reader
  // reaches for it, so it is not in this count. The phone project (iphone) does not
  // fetch the sidebar while its drawer is closed, so it uses 19. Asserted exactly (not
  // a ceiling) so a panel that starts eagerly fetching before its tab is ever opened
  // trips this immediately instead of only breaking some looser upper bound.
  expect(apiRequestUrls.length).toBe(testInfo.project.name === "iphone" ? 19 : 21);
  expect(
    apiRequestUrls.filter((url) => url.endsWith("/api/v1/settings/repo-projects"))
  ).toHaveLength(1);
  expect(apiRequestUrls.filter((url) => url.endsWith("/api/v1/me/replies?limit=1"))).toHaveLength(
    1
  );
  expect(apiRequestUrls.filter((url) => url.endsWith("/api/v1/me/agents/state"))).toHaveLength(1);
  expect(
    apiRequestUrls.filter((url) => url.endsWith("/api/v1/credential-requests?approver=me"))
  ).toHaveLength(1);
  expect(apiRequestUrls.some((url) => url.endsWith("/api/v1/projects/CORE/architecture"))).toBe(
    false
  );

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

test("live: an event during a project list's first load fetches it again once that load lands, never beside it", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "OLDTITLE" });
  // Pinned, so the sidebar's pinned row shows each rename: the one sign on this page that the
  // event was handled while the list's first load is still held.
  await putIssueState(issue.key, { pinned: true });

  const alice = await asUser(browser, "alice");
  const { promise: released, resolve: release } = Promise.withResolvers<void>();
  try {
    const page = await alice.newPage();
    const { promise: inFlight, resolve: reachedServer } = Promise.withResolvers<void>();
    const listUrl = /\/api\/v1\/issues\?project=CORE/;
    let listRequests = 0;
    let listsInFlight = 0;
    let mostInFlight = 0;
    page.on("request", (request) => {
      if (!listUrl.test(request.url())) return;
      listsInFlight += 1;
      mostInFlight = Math.max(mostInFlight, listsInFlight);
    });
    const settle = (request: { url(): string }) => {
      if (listUrl.test(request.url())) listsInFlight -= 1;
    };
    page.on("requestfinished", settle);
    page.on("requestfailed", settle);
    // The list's first request is answered by the server now and held on the wire, so the body
    // that eventually lands is genuinely older than the event below.
    await page.route(listUrl, async (route) => {
      listRequests += 1;
      if (listRequests > 1) {
        await route.continue();
        return;
      }
      const body = await (await route.fetch()).text();
      reachedServer();
      await released;
      await route.fulfill({ body, contentType: "application/json", status: 200 });
    });

    await page.goto("/projects/CORE");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const pinnedRow = page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("link", { name: issue.key });
    await expect(pinnedRow).toContainText("OLDTITLE");
    await inFlight;
    // Renamed, again every 2 s, until the pinned row shows it: the stream is open and the event
    // was handled while the held first load was still in flight.
    let newTitle = "";
    for (let attempt = 1; attempt <= 10 && newTitle === ""; attempt += 1) {
      await patchIssue(issue.key, { title: `NEWTITLE ${attempt}` }, bob);
      if (
        await expect(pinnedRow)
          .toContainText(`NEWTITLE ${attempt}`, { timeout: 2_000 })
          .then(() => true)
          .catch(() => false)
      ) {
        newTitle = `NEWTITLE ${attempt}`;
      }
    }
    expect(newTitle).not.toBe("");
    // The event did not start a second request beside the held one.
    expect(listRequests).toBe(1);
    release();

    // The held body predates the rename; the list is fetched again once it lands.
    await expect(page.getByLabel(`${issue.key} ${newTitle}`)).toBeVisible();
    await expect(page.getByLabel(`${issue.key} OLDTITLE`)).toHaveCount(0);
    await page.waitForTimeout(1_500);
    expect([listRequests, mostInFlight]).toEqual([2, 1]);
  } finally {
    release();
    await alice.close();
  }
});
