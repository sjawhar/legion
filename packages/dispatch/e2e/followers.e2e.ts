import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
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
    const followers = card.getByRole("region", { name: "Followers" });
    await expect(followers).toContainText("Followed by 2");
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

    await expect(followers).toContainText("Followed by 1");
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
    await expect(followers).toContainText("Followed by 2");
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});
