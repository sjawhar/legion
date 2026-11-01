import { expect, test } from "@playwright/test";

import { setListsState, setLiveSessions } from "./agents";
import { claimIssue, createIssue, createProject, mintAgentToken } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A session's claim lapses when its session is no longer running, and the claim chip and the
// project list's Unclaimed filter judge that against the live agent list. A list the Envoy
// listener refuses as refilling (ENVOY_REFILLING) says nothing of whether the holder is gone, so
// neither view may call the claim lapsed from it: the chip says it could not check, and the
// filter keeps the claim as held and says why. A complete list that does not name the holder is
// the one answer that lapses the claim, which the second test shows.
//
// Dispatch passes the listener's `Retry-After: 5` on with that refusal, and the shared retry
// policy leaves a read that names a wait alone for it before each of its two retries, plus a
// random share of it again (`queryRetryDelay`, `web/src/api/query-client.ts`), so a view's first
// refilling read says so up to 20 s after the page made it: `REFILL_TIMEOUT` is that wait and a
// margin, and the test's own deadline covers the two reads below that take it.

test.beforeEach(async () => {
  await resetDatabase();
});

const holder = { session_id: "claim-refill-session", title: "Claim refill holder" };
const REFILLING = "The agent list is refilling. Try again in a moment.";
const REFILL_TIMEOUT = 30_000;

async function seedSessionClaim(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Claimed while the list refills" });
  const token = await mintAgentToken("claim-refill", "alice");
  await setLiveSessions([holder]);
  await claimIssue(issue.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  return issue.key;
}

test("a refilling agent list never lapses a session's claim on the chip or in the Unclaimed filter", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const key = await seedSessionClaim();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}`);
    const chip = page.getByTestId("issue-claim");
    await expect(chip).toHaveAttribute("title", /Claimed /);
    await expect(chip).not.toHaveAttribute("title", /no longer running/);

    await setListsState("refilling");
    await page.reload();
    await expect(page.getByTestId("issue-header").getByTestId("agent-list-refilling")).toHaveText(
      REFILLING,
      { timeout: REFILL_TIMEOUT }
    );
    await expect(chip).toHaveAttribute(
      "title",
      /Couldn't check whether that session is still running: The agent list is refilling\. Try again in a moment\.$/,
      { timeout: REFILL_TIMEOUT }
    );
    await expect(chip).not.toHaveAttribute("title", /no longer running/);

    await page.goto("/projects/CORE/issues?unclaimed=1");
    await expect(page.getByTestId("unclaimed-unchecked")).toHaveText(
      `Couldn't check which claims have lapsed, so every claim counts as held: ${REFILLING}`,
      { timeout: REFILL_TIMEOUT }
    );
    await expect(page.locator("[data-issue-row]").filter({ hasText: key })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a complete agent list without the holder lapses its claim on the chip and in the Unclaimed filter", async ({
  browser,
}) => {
  const key = await seedSessionClaim();
  await setLiveSessions([]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}`);
    await expect(page.getByTestId("issue-claim")).toHaveAttribute("title", /no longer running/);
    await page.goto("/projects/CORE/issues?unclaimed=1");
    await expect(page.locator("[data-issue-row]").filter({ hasText: key })).toHaveCount(1);
    await expect(page.getByTestId("unclaimed-unchecked")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
