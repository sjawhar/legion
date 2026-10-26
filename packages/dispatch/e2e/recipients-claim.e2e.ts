import { expect, test } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  baseUrl,
  claimIssue,
  createAsk,
  createIssue,
  createProject,
  e2eAgentToken,
  getAsk,
  patchIssue,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A person's answer and replies on an ask reach the ask's followers, the session holding the
// issue's claim, and the issue's route. The card lists all three under `Reaches N`, and a claim
// or route change on the issue moves its rows without a reload.
const asker = { kind: "session" as const, id: "e2e-reach-asker" };
const claimant = { kind: "session" as const, id: "e2e-reach-claimant" };
const routed = "e2e-reach-routed";

/** `DELETE /api/v1/issues/{key}/claim` with the agent bearer: the holder gives the issue up. */
async function releaseClaim(issue: string, actor: Actor): Promise<void> {
  const path = `/api/v1/issues/${encodeURIComponent(issue)}/claim`;
  const response = await fetch(new URL(path, baseUrl), {
    body: JSON.stringify({ actor }),
    headers: { Authorization: `Bearer ${e2eAgentToken}`, "Content-Type": "application/json" },
    method: "DELETE",
  });
  if (!response.ok) {
    throw new Error(`DELETE ${path} failed: ${response.status} ${await response.text()}`);
  }
}

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
    await expect(recipients.locator("[title='Live']")).toHaveCount(3);

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
    await releaseClaim(issue.key, claimant);
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

    // A role route set on the issue joins the card over SSE as the route itself.
    await patchIssue(issue.key, { route: "role:reviewer" });
    await expect(recipients).toContainText("Reaches 2");
    const roleRow = viaIssue
      .getByRole("listitem")
      .filter({ has: page.locator("[title='role:reviewer']") });
    await expect(roleRow).toContainText("via route");
    await expect(roleRow.locator("[title='Live']")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});
