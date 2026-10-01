import { expect, test } from "@playwright/test";

import {
  type FakeSession,
  getSentMessages,
  setLiveSessions,
  setSessionLive,
  setSessionSendStatus,
} from "./agents";
import { getBroadcast, listBroadcasts, replyToMessageDelivery } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// No `last_seen`: the fixture answers every read of the session with the current time. A literal
// computed when this module is imported ages with the whole suite, and a session ten minutes "old"
// folds under Inactive rather than under the fold this spec opens - which is how long a full run
// takes to reach here.
const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  machine_id: "build-host",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
const tester: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/tester",
  machine_id: "build-host",
  roles: ["tester"],
  session_id: "tester-session",
  title: "Tester",
};
const reviewer: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/reviewer",
  machine_id: "review-host",
  roles: ["reviewer"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

// A third session for the order proof and the race below: it advertises both modes, so the
// browser includes it in the first send and only the server can exclude it in the second.
const observer: FakeSession = {
  capabilities: ["btw", "steer"],
  dir: "/workspaces/observer",
  machine_id: "build-host",
  roles: [],
  session_id: "observer-session",
  title: "Observer",
};

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("a broadcast lists recipient cards in the non-alphabetical order the sender ticked them", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester, reviewer, observer]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    // Sessions Dispatch has never heard from sit under the page's collapsed fold; broadcasting
    // is mostly aimed at exactly those, so the fold is the flow's first step.
    const quiet = agents.getByRole("button", { name: "No Dispatch activity (4)" });
    await expect(quiet).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("agents-before.png"), fullPage: true });
    await quiet.click();

    for (const title of ["Tester", "Observer", "Planner", "Reviewer"]) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("steer");
    // The Reviewer advertises `aside` only. It stays visible and is left out; no other mode is
    // substituted for it.
    await expect(composer.getByText("Excluded: Reviewer (does not advertise Send)")).toBeVisible();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 3 of 4 selected" })
    ).toBeVisible();
    await composer
      .getByRole("textbox", { name: "Broadcast message" })
      .fill("Stand down and report status.");
    // Send stays refused until there is a body and at least one recipient that can take the
    // mode; waiting for it is also what makes the capture below show the composer as a human
    // sees it rather than mid-keystroke.
    const send = composer.getByRole("button", { name: "Send to 3" });
    await expect(send).toBeEnabled();
    await page.screenshot({ path: testInfo.outputPath("broadcast-composer.png"), fullPage: true });

    await send.click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByRole("heading", { name: "Broadcast to 3 agents" })).toBeVisible();
    await expect(view.getByRole("article")).toHaveText([/Tester/, /Observer/, /Planner/]);
    for (const title of ["Tester", "Observer", "Planner"]) {
      await expect(
        view.getByRole("article", { name: title }).getByText(`Sent to ${title} (Send)`)
      ).toBeVisible();
    }
    await expect(view.getByRole("article", { name: "Reviewer" })).toHaveCount(0);
    await expect(view.getByText("0 of 3 answered")).toBeVisible();

    // One message per recipient: the listener was asked to send once to every selected session.
    const sends = await getSentMessages();
    expect(sends.map((entry) => entry.target_session).sort()).toEqual([
      "observer-session",
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
    await expect(view.getByText("1 of 3 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("broadcast-view.png"), fullPage: true });

    // The send stays reachable after navigating away from it.
    await page.getByRole("link", { name: "← All broadcasts" }).click();
    const list = page.getByRole("region", { name: "Broadcasts" });
    await expect(list.getByText("Stand down and report status.")).toBeVisible();
    await expect(list.getByText("1 of 3 answered")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("broadcast-list.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

test("a session that goes away before the send is excluded by the server and named on the broadcast, and a failed recipient can be retried there", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester, observer]);
  await setSessionSendStatus(tester.session_id, 404);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    await agents.getByRole("button", { name: "No Dispatch activity (3)" }).click();
    for (const title of ["Planner", "Tester", "Observer"]) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("btw");
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
    const send = composer.getByRole("button", { name: "Send to 3" });
    await expect(send).toBeEnabled();
    // The Observer goes away after the browser judged the selection and before the send lands:
    // the race the server re-checks for, and the only exclusion the browser cannot predict.
    await setSessionLive(observer.session_id, false);
    await send.click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);

    await expect(
      page.getByText(/Excluded: .*\(no live session\)\. Nothing was sent to them\./)
    ).toBeVisible();
    await expect(page.getByRole("article", { name: "Observer" })).toHaveCount(0);

    const failed = page.getByRole("article", { name: "Tester" });
    await expect(failed.getByText(/^Failed:/)).toBeVisible();
    await expect(
      page.getByRole("article", { name: "Planner" }).getByText("Asking Planner (BTW)")
    ).toBeVisible();

    // The listener takes the Tester again; the row's own Retry sends it without leaving the
    // broadcast for that agent's card.
    await setSessionSendStatus(tester.session_id, 200);
    await failed.getByRole("button", { name: "Retry" }).click();
    await expect(failed.getByText("Asking Tester (BTW)")).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("broadcast-retry.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});
