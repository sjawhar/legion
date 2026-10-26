import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createIssue,
  createProject,
  getAsk,
  patchIssue,
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

    // The claimant gives the issue up over the agent API; the open card drops it over SSE.
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

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
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

    // A role route set on the issue joins the card over SSE, naming the session holding the role.
    await patchIssue(issue.key, { route: "role:reviewer" });
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
  const read = await getAsk(ask.id);
  expect([read.claimant, read.route, read.route_holder]).toEqual([
    claimant.id,
    "role:e2e-reach-implementer",
    claimant.id,
  ]);

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
  const read = await getAsk(ask.id);
  expect([read.route, read.route_status, read.route_holder]).toEqual([
    "role:reviewer",
    "live",
    asker.id,
  ]);

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

test("a route change that closes the issue moves the open card's route row without a reload", async ({
  browser,
}) => {
  const moved = "e2e-reach-routed-next";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The route moves as it closes" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  // No claim: closing a claimed issue releases it in the same write, and that `issue.released`
  // would refresh the card on its own, so only `issue.closed` carries the route change here.
  await patchIssue(issue.key, { route: `session:${routed}` });
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: routed, title: "Routed (e2e)" },
    { session_id: moved, title: "Routed next (e2e)" },
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 2");
    // Focus stays on this toggle inside the card's row. The Inbox lists an open ask only while its
    // issue is open, but it holds a row the reader's focus is in where they last saw it, so the
    // card stays mounted in the Inbox after the close and its own thread is what must move.
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    const viaIssue = recipients.getByRole("list", { name: "Via claim or route" });
    const routeRow = (session: string) =>
      viaIssue.getByRole("listitem").filter({ has: page.locator(`[title='${session}']`) });
    await expect(routeRow(routed)).toContainText("via route");

    // One PATCH sets the new route and closes the issue: the server emits `issue.closed` alone.
    await patchIssue(issue.key, { route: `session:${moved}`, status: "done" });
    await expect(routeRow(moved)).toContainText("Routed next (e2e)");
    await expect(routeRow(moved)).toContainText("via route");
    await expect(recipients.locator(`[title='${routed}']`)).toHaveCount(0);
    await expect(recipients).toContainText("Reaches 2");
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
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: claimant.id, title: "Claimant (e2e)" },
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  // The stale body must land after the claim's flush, or the flush itself refreshes the card it
  // seeded (it reads the cached threads on the issue) and nothing here depends on the issue's
  // mark. A flush cancels an Inbox first load the first time it reaches it, so the first request
  // is held only until an earlier, unrelated write's flush has spent that cancel; the second
  // request is the first load the claim then finds in flight, and is let land (and refreshed when
  // it settles) rather than cancelled. Every later request reaches the server.
  const firstRequested = Promise.withResolvers<void>();
  const secondFetched = Promise.withResolvers<void>();
  const releaseSecond = Promise.withResolvers<void>();
  let staleClaimant: unknown;
  let inboxRequests = 0;
  await page.route("**/api/v1/inbox*", async (route) => {
    inboxRequests += 1;
    if (inboxRequests === 1) {
      const response = await route.fetch();
      firstRequested.resolve();
      await secondFetched.promise;
      // Cancelled by the first flush: the client discards this body.
      await route.fulfill({ response });
      return;
    }
    if (inboxRequests === 2) {
      // Composed after the unrelated write and before the claim.
      const response = await route.fetch();
      const rows = (await response.json()) as { id: string; thread: { claimant: unknown } }[];
      staleClaimant = rows.find((row) => row.id === ask.id)?.thread.claimant;
      secondFetched.resolve();
      await releaseSecond.promise;
      await route.fulfill({ response });
      return;
    }
    await route.fallback();
  });
  const streamOpened = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/api/v1/events"
  );
  // Every issue event's flush refreshes the issue lists, the shell's pinned list among them: its
  // request is the page's own word that it has handled the event.
  const pinnedListRequested = () =>
    page.waitForRequest((request) => {
      const url = new URL(request.url());
      return url.pathname === "/api/v1/issues" && url.searchParams.get("pinned") === "true";
    });
  try {
    const navigation = page.goto("/");
    // The writes must reach an open stream: a cold client's stream replays nothing from before.
    await Promise.all([firstRequested.promise, streamOpened]);
    await patchIssue(other.key, { title: "Edited while the Inbox loads" });
    await secondFetched.promise;
    expect(staleClaimant).toBeNull();

    const claimFlushed = pinnedListRequested();
    await claimIssue(issue.key, { actor: claimant, as: "agent" });
    await claimFlushed;
    releaseSecond.resolve();
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
    secondFetched.resolve();
    releaseSecond.resolve();
    await alice.close();
  }
});
