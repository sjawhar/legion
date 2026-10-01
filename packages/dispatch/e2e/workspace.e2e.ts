import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { getInbox } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";
import { seedWorkspace } from "./workspace";

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([]);
});

// Every surface a human alone acts on has to come up non-empty from this one seed: an empty
// Inbox, Agents page or broadcast would prove nothing about a change to it.
test("the seeded workspace fills every human-only surface", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the seeded workspace");
  const seeded = await seedWorkspace();
  expect(await getInbox({ login: "alice" })).toHaveLength(seeded.inboxRows);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const inboxRow = (askID: string) =>
      page.getByRole("listitem").filter({ has: page.getByTestId(`ask-${askID}`) });

    // The Inbox opens on Mine: alice's issues' asks, the Unassigned band with the document ask,
    // and the snoozed ask folded under Later. A session's ask names it by the title its write
    // stamped, as a real agent's does, rather than by its raw id.
    await page.goto("/");
    await expect(page.getByRole("article", { name: "Urgency: Blocking" })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Later \(1\)/ })).toBeVisible();
    await expect(inboxRow(seeded.asks.snoozed)).toBeHidden();
    await expect(
      inboxRow(seeded.asks.rotate).getByRole("button", { expanded: false, name: /Reviewer/ })
    ).toBeVisible();
    await expect(inboxRow(seeded.asks.document)).toContainText("Runbook");
    await page.screenshot({ path: testInfo.outputPath("workspace-inbox.png"), fullPage: true });

    // Every session with Dispatch activity is an open row; the one Dispatch never heard from and
    // the one unseen for twenty minutes each sit under their own fold.
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const quiet = agents.getByRole("button", { name: "No Dispatch activity (1)" });
    const inactive = agents.getByRole("button", { name: "Inactive (1)" });
    await expect(quiet).toBeVisible();
    await expect(inactive).toBeVisible();
    await expect
      .poll(async () => (await agents.locator("article h2").allTextContents()).sort())
      .toEqual(["Planner", "Reviewer", "Tester"]);
    await quiet.click();
    await expect(
      agents.getByRole("region", { name: "No Dispatch activity" }).locator("article h2")
    ).toHaveText(["Observer"]);
    await quiet.click();
    await inactive.click();
    await expect(agents.getByRole("region", { name: "Inactive" }).locator("article h2")).toHaveText(
      ["Archivist"]
    );
    await inactive.click();
    const plannerCard = agents
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    await plannerCard.getByRole("button", { exact: true, name: "Planner" }).click();
    await expect(
      plannerCard.getByRole("list", { name: "Conversation with Planner" })
    ).toContainText("Half-way; the seed is next.");
    await page.screenshot({ path: testInfo.outputPath("workspace-agents.png"), fullPage: true });

    // The broadcast keeps the order it was sent in, which is not alphabetical, and shows the one
    // answer it has.
    await page.goto(`/agents/broadcasts/${seeded.broadcast.id}`);
    const broadcast = page.getByRole("region", { name: "Broadcast" });
    await expect(broadcast.getByRole("article")).toHaveText([/Tester/, /Observer/, /Planner/]);
    await expect(broadcast.getByText("1 of 3 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("workspace-broadcast.png"), fullPage: true });

    // The issue's Conversation holds the answered ask and, inside the mention's thread, the
    // mentioned session's reply.
    await page.goto(`/issues/${seeded.issues.broadcastOrder}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByText("Ship the recipient-order fix?")).toBeVisible();
    const mention = turns
      .locator('[data-turn^="comment:"]')
      .filter({ hasText: "@Tester please re-run the broadcast spec" });
    await mention.getByRole("button", { name: "Expand thread" }).click();
    await expect(mention.getByText("Re-ran: 2 passed.")).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath("workspace-conversation.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});
