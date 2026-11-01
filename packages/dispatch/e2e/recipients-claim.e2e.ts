import { expect, test } from "@playwright/test";

import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";

import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createIssue,
  createProject,
  getAsk,
  getIssue,
  patchIssue,
  putIssueState,
  releaseClaim,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A person's answer and replies on an ask reach the ask's followers, the session holding the
// issue's claim, and the session the issue's route reaches (a role route's holder). The card lists
// each session once under `Reaches N`, and a claim or route change on the issue moves its rows
// without a reload.
const asker = { kind: "session" as const, id: "e2e-reach-asker" };
const claimant = { kind: "session" as const, id: "e2e-reach-claimant" };
const routed = "e2e-reach-routed";
// A row that waits out the listener roster (routeRosterTTL, five seconds) sees the refreshed read
// only on its next probe, and the default ladder probes once a second after the first second.
const ROSTER = { intervals: [50, 50, 100, 200, 500], timeout: 15_000 };

test.beforeEach(async () => {
  await resetDatabase();
});

test("the ask card lists the issue's claimant and route, and drops the claimant on release without a reload", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who else hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await claimIssue(issue.key, { actor: claimant, as: "agent" });
  await patchIssue(issue.key, { route: `session:${routed}` });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: claimant.id, title: "Claimant (e2e)" },
    { session_id: routed, title: "Routed (e2e)" },
  ]);
  const read = await getAsk(ask.id);
  expect(read.followers.map((row) => row.session_id)).toEqual([asker.id]);
  expect([read.claimant, read.route]).toEqual([claimant.id, `session:${routed}`]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 3");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    await expect(
      recipients.getByRole("list", { name: "Followers" }).locator(`[title='${asker.id}']`)
    ).toHaveCount(1);
    const viaIssue = recipients.getByRole("list", { name: "Via claim or route" });
    const claimRow = viaIssue
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${claimant.id}']`) });
    await expect(claimRow).toContainText("Claimant (e2e)");
    await expect(claimRow).toContainText("via claim");
    const routeRow = viaIssue
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${routed}']`) });
    await expect(routeRow).toContainText("Routed (e2e)");
    await expect(routeRow).toContainText("via route");
    // The claim and the route change on the issue, not here.
    await expect(viaIssue.getByRole("button")).toHaveCount(0);
    // Each row is drawn by `AgentName`: its registry running state and a link to its live view.
    await expect(recipients.getByRole("img", { exact: true, name: "Running" })).toHaveCount(3);
    await expect(claimRow.getByRole("link", { name: "Claimant (e2e)" })).toHaveAttribute(
      "href",
      `/agents/${claimant.id}/live`
    );

    // The asker is neither the claimant nor the route session: unfollowing it stops delivery.
    await recipients
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${asker.id}']`) })
      .getByRole("button", { name: "Unfollow" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Unfollow" });
    await expect(dialog).toContainText(
      "Unfollow Asker (e2e) from this ask? Its answer and replies will no longer reach them. They will be told."
    );
    await dialog.getByRole("button", { name: "Cancel" }).click();
    await expect(dialog).toHaveCount(0);

    // The claimant gives the issue up over the agent API; the open card drops it over the live socket's event feed.
    await releaseClaim(issue.key, { actor: claimant, as: "agent" });
    await expect(recipients).toContainText("Reaches 2");
    await expect(recipients.locator(`[title='${claimant.id}']`)).toHaveCount(0);
    await expect(routeRow).toContainText("via route");
    await expect(recipients.locator(`[title='${asker.id}']`)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a follower that holds the claim is listed once, and unfollowing it says the claim still reaches it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The asker works the issue" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await claimIssue(issue.key, { actor: asker, as: "agent" });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { roles: ["reviewer"], session_id: routed, title: "Reviewer (e2e)" },
  ]);
  // The server answers route reads from one listener roster kept up to five seconds
  // (routeRosterTTL), so an issue routed to the role is read until that roster holds the reviewer:
  // the single refresh the card makes on the route change below must not read an older one.
  const probe = await createIssue({
    project: "CORE",
    title: "A roster probe for the reviewer role",
  });
  await patchIssue(probe.key, { route: "role:reviewer" });
  await expect.poll(async () => (await getIssue(probe.key)).route_holder, ROSTER).toBe(routed);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    // The card's reads below arrive over the event feed, whose refetch the page holds to one per
    // second (`MIN_REFETCH_INTERVAL_MS`): a page timer the page's own clock fires.
    await page.clock.install();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 1");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    await expect(recipients.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect(recipients.getByRole("list", { name: "Via claim or route" })).toHaveCount(0);

    await recipients.getByRole("button", { name: "Unfollow" }).click();
    const dialog = page.getByRole("dialog", { name: "Unfollow" });
    await expect(dialog).toContainText(
      "Unfollow Asker (e2e) from this ask? A person's answer and replies still reach them through the claim. They will be told."
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();

    // Unfollowed, the claimant moves into the claim group rather than off the card.
    const viaIssue = recipients.getByRole("list", { name: "Via claim or route" });
    await expect(viaIssue.getByRole("listitem")).toHaveCount(1);
    await expect(viaIssue).toContainText("via claim");
    await expect(recipients.getByRole("list", { name: "Followers" })).toHaveCount(0);
    await expect(recipients).toContainText("Reaches 1");

    // A role route set on the issue joins the card over the event feed, naming the role's holder.
    await patchIssue(issue.key, { route: "role:reviewer" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(recipients).toContainText("Reaches 2");
    const roleRow = viaIssue
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${routed}']`) });
    await expect(roleRow).toContainText("Reviewer (e2e)");
    await expect(roleRow).toContainText("via route (role:reviewer)");
    await expect(roleRow.getByRole("img", { exact: true, name: "Running" })).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a role route held by the claimant is counted once, under the claim that names its holder", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The claimant holds the route" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await claimIssue(issue.key, { actor: claimant, as: "agent" });
  await patchIssue(issue.key, { route: "role:e2e-reach-implementer" });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { roles: ["e2e-reach-implementer"], session_id: claimant.id, title: "Claimant (e2e)" },
  ]);
  // The server keeps one listener roster for up to five seconds (routeRosterTTL), so an earlier
  // read may still answer from the roster before these sessions went live.
  await expect
    .poll(async () => {
      const read = await getAsk(ask.id);
      return [read.claimant, read.route, read.route_holder];
    }, ROSTER)
    .toEqual([claimant.id, "role:e2e-reach-implementer", claimant.id]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    // The asker and the claimant: the route reaches the claimant, already counted.
    await expect(recipients).toContainText("Reaches 2");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    const viaIssue = recipients.getByRole("list", { name: "Via claim or route" });
    await expect(viaIssue.getByRole("listitem")).toHaveCount(1);
    const holderRow = viaIssue.getByRole("listitem");
    await expect(holderRow.locator(`[title='${claimant.id}']`)).toHaveText("Claimant (e2e)");
    await expect(holderRow).toContainText("via claim");
    await expect(holderRow.getByRole("img", { exact: true, name: "Running" })).toHaveCount(1);
    await expect(recipients.locator("[title='role:e2e-reach-implementer']")).toHaveCount(0);
    await expect(recipients.locator(`[title='${claimant.id}']`)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("unfollowing a follower while a role route's holder is unknown says the route may still reach it, under the issue page's marker", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The listener did not answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await patchIssue(issue.key, { route: "role:reviewer" });
  // The asker holds the role: the very follower Dispatch cannot rule out once the listener is
  // silent.
  await setLiveSessions([{ roles: ["reviewer"], session_id: asker.id, title: "Asker (e2e)" }]);
  // The server keeps one listener roster for up to five seconds (routeRosterTTL), so an earlier
  // read may still answer from the roster before the asker went live.
  await expect
    .poll(async () => {
      const read = await getAsk(ask.id);
      return [read.route, read.route_status, read.route_holder];
    }, ROSTER)
    .toEqual(["role:reviewer", "live", asker.id]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  // The listener answering nothing is forced on the real reads: the issue, the ask's thread and
  // the Inbox rows keep every other field the server sent.
  const unknownReach = { route_holder: null, route_status: "unknown" };
  await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as Record<string, unknown>;
    await route.fulfill({ json: { ...body, ...unknownReach }, response });
  });
  await page.route(`**/api/v1/asks/${ask.id}`, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as Record<string, unknown>;
    await route.fulfill({ json: { ...body, ...unknownReach }, response });
  });
  await page.route("**/api/v1/inbox*", async (route) => {
    const response = await route.fetch();
    const rows = (await response.json()) as { id: string; thread: Record<string, unknown> }[];
    await route.fulfill({
      json: rows.map((row) =>
        row.id === ask.id ? { ...row, thread: { ...row.thread, ...unknownReach } } : row
      ),
      response,
    });
  });
  try {
    await page.goto(`/issues/${issue.key}`);
    const headerMarker = page.getByTestId("issue-route-unknown");
    await expect(headerMarker).toHaveText("Route reach unknown");
    const headerTitle = await headerMarker.getAttribute("title");
    expect(headerTitle).toBe(
      "The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route."
    );

    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 1 + route (unknown)");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    const routeRow = recipients
      .getByRole("list", { name: "Via claim or route" })
      .getByRole("listitem")
      .filter({ has: page.locator("[title='role:reviewer']") });
    await expect(routeRow).toContainText("via route");
    const rowMarker = routeRow.getByText("Route reach unknown");
    await expect(rowMarker).toHaveAttribute("title", headerTitle ?? "");

    await recipients
      .getByRole("list", { name: "Followers" })
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${asker.id}']`) })
      .getByRole("button", { name: "Unfollow" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Unfollow" });
    await expect(dialog).toContainText(
      "Unfollow Asker (e2e) from this ask? A person's answer and replies may still reach them through the route: Dispatch could not tell who holds it. They will be told."
    );
    await expect(dialog).not.toContainText("will no longer reach them");
  } finally {
    await alice.close();
  }
});

test("a route change in a status write moves the open card's route row without a reload, and a close that also moves the route withdraws the card in place", async ({
  browser,
}) => {
  const moved = "e2e-reach-routed-next";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The route moves, then it closes" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  // No claim: a claim's `issue.released` would refresh the card on its own (closing a claimed
  // issue releases it in the same write), so only the issue events of each PATCH carry what moves
  // here.
  await patchIssue(issue.key, { route: `session:${routed}` });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: routed, title: "Routed (e2e)" },
    { session_id: moved, title: "Routed next (e2e)" },
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    // Both PATCH writes below reach the card over the event feed, whose refetch the page holds to
    // one per second (`MIN_REFETCH_INTERVAL_MS`): a page timer the page's own clock fires.
    await page.clock.install();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 2");
    // Focus stays on this toggle inside the card's row. The Inbox lists an open ask only while its
    // issue is open, but it holds a row the reader's focus is in where they last saw it, so the
    // card stays mounted in the Inbox after the close below.
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    const viaIssue = recipients.getByRole("list", { name: "Via claim or route" });
    const routeRow = (session: string) =>
      viaIssue.getByRole("listitem").filter({ has: page.locator(`[title='${session}']`) });
    await expect(routeRow(routed)).toContainText("via route");

    // One PATCH sets the new route and moves the status: one `issue.updated` names both, and the
    // card's own thread is what must move.
    await patchIssue(issue.key, { route: `session:${moved}`, status: "in_progress" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(routeRow(moved)).toContainText("Routed next (e2e)");
    await expect(routeRow(moved)).toContainText("via route");
    await expect(recipients.locator(`[title='${routed}']`)).toHaveCount(0);
    await expect(recipients).toContainText("Reaches 2");

    // One PATCH moves the route back and closes the issue. The close withdraws the issue's open
    // asks, so the mounted card shows its ask withdrawn by the closer and lists no recipient at
    // all, since nobody can answer it any more.
    await patchIssue(issue.key, { route: `session:${routed}`, status: "done" });
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(card).toContainText("Withdrawn when alice closed the issue");
    await expect(recipients).toHaveCount(0);
    await expect(card).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a claim that lands while the Inbox is loading reaches the card that mounts from the earlier list", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed mid-load" });
  const other = await createIssue({ project: "CORE", title: "An unrelated rename target" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  // Pinned, so a rename of it refreshes alice's pinned list and nothing the Inbox shows.
  await putIssueState(other.key, { pinned: true });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: claimant.id, title: "Claimant (e2e)" },
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  // The Inbox's first load is composed now, before the claim, and held until the claim has been
  // handled. No cached thread names the ask yet, so the claim's refresh of the threads on its
  // issue reaches nothing: only the issue's mark sends the card that mounts from this body to the
  // ask endpoint. Every later request reaches the server.
  const firstFetched = Promise.withResolvers<void>();
  const releaseFirst = Promise.withResolvers<void>();
  let staleClaimant: unknown;
  let inboxRequests = 0;
  await page.route("**/api/v1/inbox*", async (route) => {
    inboxRequests += 1;
    if (inboxRequests === 1) {
      const response = await route.fetch();
      const rows = (await response.json()) as { id: string; thread: { claimant: unknown } }[];
      staleClaimant = rows.find((row) => row.id === ask.id)?.thread.claimant;
      firstFetched.resolve();
      await releaseFirst.promise;
      await route.fulfill({ response });
      return;
    }
    await route.fallback();
  });
  const streamOpened = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  const pinnedListRead = () =>
    page.waitForRequest((request) => {
      const url = new URL(request.url());
      return url.pathname === "/api/v1/issues" && url.searchParams.get("pinned") === "true";
    });
  try {
    const firstPinnedRead = pinnedListRead();
    const navigation = page.goto("/");
    // The claim goes out once the page's live socket is open and the sidebar has read its pinned
    // list, so its flush reaches the page while the Inbox load it is timed against is still in
    // flight, and the rename after it is seen by a pinned list already loaded.
    await Promise.all([firstFetched.promise, streamOpened, firstPinnedRead]);
    expect(staleClaimant).toBeNull();

    await claimIssue(issue.key, { actor: claimant, as: "agent" });
    // Events reach the page in order, so once the rename written after the claim has made the
    // page read its pinned list again, the claim's event has been handled too.
    const renameHandled = pinnedListRead();
    await patchIssue(other.key, { title: "Renamed after the claim" });
    await renameHandled;
    releaseFirst.resolve();
    await navigation;

    // The card mounts from the body that names no claimant, and shows the claim anyway.
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 2");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    const claimRow = recipients
      .getByRole("list", { name: "Via claim or route" })
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${claimant.id}']`) });
    await expect(claimRow).toContainText("Claimant (e2e)");
    await expect(claimRow).toContainText("via claim");
  } finally {
    releaseFirst.resolve();
    await alice.close();
  }
});
