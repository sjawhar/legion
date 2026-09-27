import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { getBroadcast, listBroadcasts, replyToMessageDelivery } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  last_seen: Date.now() - 30_000,
  machine_id: "build-host",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
const tester: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/tester",
  last_seen: Date.now() - 20_000,
  machine_id: "build-host",
  roles: ["tester"],
  session_id: "tester-session",
  title: "Tester",
};
const reviewer: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/reviewer",
  last_seen: Date.now() - 10_000,
  machine_id: "review-host",
  roles: ["reviewer"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("one message reaches every selected agent that advertises the mode, and the broadcast view collects their answers", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester, reviewer]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    // Sessions Dispatch has never heard from sit under the page's collapsed fold; broadcasting
    // is mostly aimed at exactly those, so the fold is the flow's first step.
    const quiet = agents.getByRole("button", { name: "No Dispatch activity (3)" });
    await expect(quiet).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("agents-before.png"), fullPage: true });
    await quiet.click();

    for (const title of ["Planner", "Tester", "Reviewer"]) {
      await agents.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    // The Reviewer advertises `aside` only. It stays visible and is left out; no other mode is
    // substituted for it.
    await expect(composer.getByText("Excluded: Reviewer (does not advertise steer)")).toBeVisible();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 3 selected" })
    ).toBeVisible();
    await composer
      .getByRole("textbox", { name: "Broadcast message" })
      .fill("Stand down and report status.");
    // Send stays refused until there is a body and at least one recipient that can take the
    // mode; waiting for it is also what makes the capture below show the composer as a human
    // sees it rather than mid-keystroke.
    const send = composer.getByRole("button", { name: "Send to 2" });
    await expect(send).toBeEnabled();
    await page.screenshot({ path: testInfo.outputPath("broadcast-composer.png"), fullPage: true });

    await send.click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByRole("heading", { name: "Broadcast to 2 agents" })).toBeVisible();
    for (const title of ["Planner", "Tester"]) {
      await expect(
        view.getByRole("article", { name: title }).getByText(`Sent to ${title} (steer)`)
      ).toBeVisible();
    }
    await expect(view.getByRole("article", { name: "Reviewer" })).toHaveCount(0);
    await expect(view.getByText("0 of 2 answered")).toBeVisible();

    // One message per recipient: the listener was asked to send twice, once to each.
    const sends = await getSentMessages();
    expect(sends.map((send) => send.target_session).sort()).toEqual([
      "planner-session",
      "tester-session",
    ]);

    // The Planner answers its own delivery, and the open view picks the answer up from the
    // event stream without a reload.
    const [summary] = await listBroadcasts();
    if (summary === undefined) throw new Error("the broadcast was not recorded");
    const stored = await getBroadcast(summary.id);
    const plannerMessage = stored.recipients.find(
      (recipient) => recipient.session_id === "planner-session"
    );
    if (plannerMessage === undefined) throw new Error("the planner received no message");
    await replyToMessageDelivery(
      plannerMessage.message.id,
      { attempt: 1, body: "Standing down; build is green." },
      { id: "planner-session", kind: "session" }
    );
    await expect(view.getByText("Standing down; build is green.")).toBeVisible();
    await expect(
      view.getByRole("article", { name: "Planner" }).getByText("Answered by Planner")
    ).toBeVisible();
    await expect(view.getByText("1 of 2 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("broadcast-view.png"), fullPage: true });

    // The send stays reachable after navigating away from it.
    await page.getByRole("link", { name: "← All broadcasts" }).click();
    const list = page.getByRole("region", { name: "Broadcasts" });
    await expect(list.getByText("Stand down and report status.")).toBeVisible();
    await expect(list.getByText("1 of 2 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("broadcast-list.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});
