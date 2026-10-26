import { expect, test } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createMessage,
  createProject,
  disconnectLiveSockets,
  getIssue,
  patchIssue,
  putArchitectureSource,
  putIssueState,
  resolveAsk,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// These scenarios exercise the live event feed fanning updates into other open tabs, so
// the counterparty ("bob") is an autonomous session acting through the bearer API —
// only alice's browser needs to be real, since that is what proves the live update.
const bob = { actor: { kind: "session" as const, id: "bob" }, as: "agent" as const };

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

test("live: a message on another issue skips the Inbox refetch", async ({ browser }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const otherIssue = await createIssue({ project: "CORE", title: "Unviewed issue" });

  const alice = await asUser(browser, "alice");
  const inboxPage = await alice.newPage();
  let inboxReads = 0;
  let issueReads = 0;
  inboxPage.on("request", (request) => {
    if (request.method() !== "GET") return;
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/inbox") inboxReads += 1;
    if (path === "/api/v1/issues") issueReads += 1;
  });

  try {
    await inboxPage.goto("/");
    await expect(inboxPage.getByText("Nothing needs you")).toBeVisible();
    if (testInfo.project.name === "iphone") {
      // The pinned list lives in the drawer; open it so both projects measure the same queries.
      await inboxPage.getByRole("button", { name: "Open navigation" }).click();
      await expect(inboxPage.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    }
    await inboxPage.waitForTimeout(250);
    inboxReads = 0;
    issueReads = 0;

    await createMessage(otherIssue.key, { body: "Unviewed issue update" }, bob);
    await expect.poll(() => issueReads).toBe(1);
    // The debounce and any straggling refetch settle here, so a late Inbox read still counts.
    await inboxPage.waitForTimeout(350);

    // The Inbox is the subtracted query; the issue lists stay on main's conservative refresh.
    expect([inboxReads, issueReads]).toEqual([0, 1]);
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
  const socketOpened = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto("/");
  await expect(page.getByText("Nothing needs you")).toBeVisible();
  const socket = await socketOpened;
  await expect(page.getByText("Reconnecting")).toHaveCount(0);

  // D10: the pill must never move the reading surface. It has to render as an
  // overlay (not push content in flow), so the main heading's box stays put both
  // while it appears and after it goes away.
  const heading = page.locator("main").getByRole("heading", { name: "Inbox" });
  const boundsBeforeOutage = await heading.boundingBox();

  const closed = socket.waitForEvent("close");
  await alice.setOffline(true);
  await closed;
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

test("live: a forced server disconnect resumes from the last event id", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Disconnect target" });
  await createMessage(issue.key, { body: "Before disconnect" }, bob);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  // Each live socket the page opens, in order: the id of every event frame it received and the
  // resume of every event feed subscribe it sent.
  const sockets: { readonly events: string[]; readonly resumes: (string | undefined)[] }[] = [];
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    const seen = { events: [] as string[], resumes: [] as (string | undefined)[] };
    sockets.push(seen);
    socket.on("framereceived", ({ payload }) => {
      const frame = JSON.parse(String(payload)) as { readonly type?: string; readonly id?: string };
      if (frame.type === "event" && frame.id !== undefined) seen.events.push(frame.id);
    });
    socket.on("framesent", ({ payload }) => {
      const frame = JSON.parse(String(payload)) as {
        readonly type?: string;
        readonly view?: { readonly kind?: string };
        readonly resume?: { readonly since?: string };
      };
      if (frame.type === "subscribe" && frame.view?.kind === "events") {
        seen.resumes.push(frame.resume?.since);
      }
    });
  });
  const whoami = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/auth/whoami" && response.ok()
  );

  await page.goto(`/issues/${issue.key}/conversation`);
  const head: string = (await (await whoami).json()).event_head;
  await expect(page.getByText("Before disconnect")).toBeVisible();
  // The first subscription resumes from the sign-in check's head.
  await expect.poll(() => sockets[0]?.resumes).toEqual([head]);
  await createMessage(issue.key, { body: "Seen live before disconnect" }, bob);
  await expect(page.getByText("Seen live before disconnect")).toBeVisible();
  const first = sockets[0];
  if (first === undefined) throw new Error("the page opened no live socket");
  expect(first.events.length).toBeGreaterThan(0);

  // A 1012 restart opens the socket at once and resumes the event feed after the last event the
  // closed socket carried.
  await disconnectLiveSockets();
  await expect.poll(() => sockets[1]?.resumes.length ?? 0).toBe(1);
  expect(sockets).toHaveLength(2);
  const last = first.events.at(-1);
  expect(sockets[1]?.resumes).toEqual([last]);
  expect(BigInt(last ?? "0")).toBeGreaterThan(BigInt(head));

  await createMessage(issue.key, { body: "Missed during disconnect" }, bob);
  await expect(page.getByText("Missed during disconnect")).toBeVisible();

  await alice.close();
});

// The page's first live socket fails its opening handshake. A change committed before the next one
// opens still reaches the page, with no reload: the page tries again, and its event feed resumes
// from the sign-in check's head, so Dispatch replays the change on it.
test("live: a change committed while the first live socket fails to open is shown without a reload", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Before the failed handshake" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Every document the page fetched for itself: its first load, and a reload if there were one.
    // The `load` event is not that count - Playwright's Firefox fires one for the initial
    // `about:blank` - while a reload is a navigation request of the main frame in every engine.
    const loaded: string[] = [];
    page.on("request", (request) => {
      if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
        loaded.push(request.url());
      }
    });
    let opens = 0;
    // The event feed's subscribes the second socket carried to Dispatch, in order.
    const resumes: (string | undefined)[] = [];
    const refused = Promise.withResolvers<void>();
    const committed = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      opens += 1;
      if (opens === 1) {
        await route.close({ code: 1006, reason: "the opening handshake failed" });
        refused.resolve();
        return;
      }
      await committed.promise;
      const server = route.connectToServer();
      route.onMessage((message) => {
        const frame = JSON.parse(String(message)) as {
          readonly type?: string;
          readonly view?: { readonly kind?: string };
          readonly resume?: { readonly since?: string };
        };
        if (frame.type === "subscribe" && frame.view?.kind === "events") {
          resumes.push(frame.resume?.since);
        }
        server.send(message);
      });
    });
    const whoami = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/auth/whoami" && response.ok()
    );

    await page.goto(`/issues/${issue.key}/conversation`);
    const head: string = (await (await whoami).json()).event_head;
    await expect(page.getByText("Before the failed handshake", { exact: true })).toBeVisible();
    await refused.promise;
    await patchIssue(issue.key, { title: "Committed while the socket was down" }, bob);
    committed.resolve();

    await expect(
      page.getByText("Committed while the socket was down", { exact: true })
    ).toBeVisible();
    // The first subscribe the page ever sent rode the second socket, and resumed from the sign-in
    // check's head: the patch is above it, so Dispatch replayed it there.
    expect(resumes[0]).toBe(head);
    expect(opens).toBe(2);
    expect(loaded).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("live: a fresh page load opens the socket and stays within a bounded request budget", async ({
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

  const socketOpened = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(page.getByText("Backlog message 4")).toBeVisible();
  await socketOpened;

  // The socket is outside the HTTP request budget, and the sign-in check carries its event head.
  expect(apiRequestUrls.some((url) => new URL(url).pathname === "/api/v1/events")).toBe(false);

  // Exactly 20 on desktop (chromium): whoami, the project sidebar's Inbox, Pinned,
  // and Projects queries, me/state, issue detail, the shared inbox query for the
  // margin's open-ask count, the active-sessions read, the primary artifact's comments -
  // which the Conversation's own list shares rather than fetching again - the margin's own
  // list of the issue's asks and the Conversation's, which asks the same route for each
  // ask's thread (`?thread=1`) and so is a request of its own, not the margin's shared,
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
  // and the Activity panel is unmounted until its tab is opened. The phone project (iphone)
  // does not fetch the sidebar while its drawer is closed, so it uses 18. Asserted exactly
  // (not a ceiling) so a panel that starts eagerly fetching before its tab is ever opened
  // trips this immediately instead of only breaking some looser upper bound.
  expect(apiRequestUrls.length).toBe(testInfo.project.name === "iphone" ? 18 : 20);
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

test("live: an event during a project list's first load beats the response that predates it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "OLDTITLE" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const { promise: released, resolve: release } = Promise.withResolvers<void>();
    const { promise: inFlight, resolve: reachedServer } = Promise.withResolvers<void>();
    const streaming = page.waitForEvent(
      "websocket",
      (socket) => new URL(socket.url()).pathname === "/ws/live"
    );
    let listRequests = 0;
    // The list's first request is answered by the server now and held on the wire, so the body
    // that eventually lands is genuinely older than the event below.
    await page.route(/\/api\/v1\/issues\?project=CORE/, async (route) => {
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
    await Promise.all([inFlight, streaming]);
    // The page has opened its socket before the rename. Give its events subscription time to
    // reach Dispatch, then make the stale first list response race the arriving event.
    await page.waitForTimeout(500);
    await patchIssue(issue.key, { title: "NEWTITLE" });
    // The held body must not be released until the event's own invalidation has issued its
    // request: without that wait the stale body lands first, the list is filled from it and the
    // test passes without exercising the cancel at all.
    await expect.poll(() => listRequests).toBe(2);
    release();

    await expect(page.getByLabel(`${issue.key} NEWTITLE`)).toBeVisible();
    await expect(page.getByLabel(`${issue.key} OLDTITLE`)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
