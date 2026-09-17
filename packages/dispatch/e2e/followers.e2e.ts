import { expect, test } from "@playwright/test";

import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  followAsk,
  getIssueEvents,
  listAskFollowers,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Two sessions write to one ask over the API with the agent bearer token: the asker and a
// replier. Both follow the ask (its answer and replies reach them directly) until a human
// removes one from the card.
const asker = { kind: "session" as const, id: "e2e-follow-asker" };
const replier = { kind: "session" as const, id: "e2e-follow-replier" };

test.beforeEach(async () => {
  await resetDatabase();
  // `Reaches N` counts the fake Envoy's persisted interests, which outlive the database reset
  // and match the recycled issue keys, so each test starts from none.
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setInterests([]);
  }
});

test("the ask card lists every session that wrote to the ask and a human can unfollow one", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await createComment(
    issue.key,
    { ask_id: ask.id, body: "Tests are green on my side." },
    { actor: replier, as: "agent" }
  );
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([
      { session_id: asker.id, title: "Asker (e2e)" },
      { session_id: replier.id, title: "Replier (e2e)" },
    ]);
  }
  expect((await listAskFollowers(ask.id)).followers.map((row) => row.session_id)).toEqual([
    asker.id,
    replier.id,
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const followers = card.getByRole("region", { name: "Recipients" });
    await expect(followers).toContainText("Reaches 2");
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await expect(followers.getByText("Asker (e2e)", { exact: true })).toBeVisible();
      await expect(followers.getByText("Replier (e2e)", { exact: true })).toBeVisible();
      await expect(followers.locator("[title='Live']")).toHaveCount(2);
    }
    await expect(followers.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(1);

    // The human removes the replier; the card, the API, and the event log agree.
    await followers
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${replier.id}']`) })
      .getByRole("button", { name: "Unfollow" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Unfollow" });
    await expect(dialog).toContainText(
      process.env.PLAYWRIGHT_BASE_URL
        ? "from this ask? Its answer and replies will no longer reach them. They will be told."
        : "Unfollow Replier (e2e) from this ask? Its answer and replies will no longer reach them. They will be told."
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();

    await expect(followers).toContainText("Reaches 1");
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(0);
    await expect(followers.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect
      .poll(async () => (await listAskFollowers(ask.id)).followers.map((row) => row.session_id))
      .toEqual([asker.id]);
    const removed = (await getIssueEvents(issue.key)).find(
      (event) => event.type === "ask.follower_removed"
    );
    expect(removed).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { ask_id: ask.id, session_id: replier.id },
    });

    // The replier rejoins over the API; the open card picks it up live over SSE.
    await followAsk(ask.id, replier.id, replier);
    await expect(followers).toContainText("Reaches 2");
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

// A session that never wrote to the ask still hears every event on it when its Envoy
// interests cover the owning issue's topic family. The card lists it beside the followers,
// and removing it goes through the issue-subscriber route: the one that stops delivery.
test("the ask card lists the issue's subscribers and removing one cuts the issue subscription", async ({
  browser,
}) => {
  test.skip(Boolean(process.env.PLAYWRIGHT_BASE_URL), "seeds Envoy interests on the fake listener");
  const watcher = "e2e-issue-watcher";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who else hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: watcher, title: "Watcher (e2e)" },
  ]);
  await setInterests([
    { session_id: watcher, topics: [`notifications.dispatch.issue.${issue.key}.>`] },
  ]);
  expect((await listAskFollowers(ask.id)).followers.map((row) => row.session_id)).toEqual([
    asker.id,
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 2");
    await expect(
      recipients.getByRole("list", { name: "Followers" }).locator(`[title='${asker.id}']`)
    ).toHaveCount(1);
    const viaIssue = recipients.getByRole("list", { name: "Via issue subscription" });
    await expect(recipients).toContainText("via issue subscription");
    await expect(viaIssue.getByText("Watcher (e2e)", { exact: true })).toBeVisible();
    await expect(viaIssue.locator(`[title='${watcher}']`)).toHaveCount(1);
    await expect(viaIssue.getByRole("button", { name: "Unfollow" })).toHaveCount(0);

    const unsubscribed = page.waitForRequest(
      (request) =>
        request.method() === "DELETE" &&
        request.url().endsWith(`/api/v1/issues/${issue.key}/subscribers/${watcher}`)
    );
    await viaIssue.getByRole("button", { name: "Unsubscribe" }).click();
    const dialog = page.getByRole("dialog", { name: "Unsubscribe" });
    await expect(dialog).toContainText(
      `Unsubscribe Watcher (e2e) from ${issue.key}? They will be told.`
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();
    await unsubscribed;

    await expect(recipients).toContainText("Reaches 1");
    await expect(recipients.locator(`[title='${watcher}']`)).toHaveCount(0);
    await expect(recipients.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect
      .poll(async () => (await getUnsubscribeCalls()).some((call) => call.session_id === watcher))
      .toBe(true);
    // The follower route was never touched: the asker still follows.
    expect((await listAskFollowers(ask.id)).followers.map((row) => row.session_id)).toEqual([
      asker.id,
    ]);
  } finally {
    await alice.close();
  }
});
