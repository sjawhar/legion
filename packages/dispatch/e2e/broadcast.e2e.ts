import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  type FakeSession,
  getSentMessages,
  postedBroadcasts,
  refuseBroadcasts,
  setLiveSessions,
  setSessionLive,
  setSessionSendStatus,
} from "./agents";
import {
  createAgentMessage,
  createBroadcast,
  getBroadcast,
  listBroadcasts,
  replyToMessageDelivery,
} from "./api";
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
  await resetDatabase();
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

// LEGION-446. The browser drops a same-tick double press itself, so the repeat here is the one a
// proxy or a flaky connection makes: the same request, the same key, the same human.
test("a second identical send of one composition is answered the original broadcast, so every agent hears it once", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    await page.goto("/agents");
    await page
      .getByRole("region", { name: "Agents" })
      .getByRole("button", { name: "No Dispatch activity (2)" })
      .click();
    for (const title of ["Planner", "Tester"]) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Stand down.");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const first = page.url().split("/").at(-1);
    expect(posted).toHaveLength(1);
    const [request] = posted;
    if (request === undefined) throw new Error("the press sent nothing");
    expect(request.idempotency_key).not.toBe("");

    const replay = await createBroadcast(request);
    expect(replay.id).toBe(first);
    expect((await listBroadcasts()).map((summary) => summary.id)).toEqual([first]);
    await expect
      .poll(async () =>
        (await getBroadcast(replay.id)).recipients.every((recipient) =>
          recipient.message.deliveries.every((attempt) => attempt.state !== "pending")
        )
      )
      .toBe(true);
    const sends = await getSentMessages();
    expect(sends.map((entry) => entry.target_session).sort()).toEqual([
      "planner-session",
      "tester-session",
    ]);
  } finally {
    await alice.close();
  }
});

// The security review's case: a selected session goes away between the refused send and its
// restore. Restore draft re-sends the refused request word for word, so the composer counts the
// departed session as the request does, and the server excludes it against its own registry.
test("a session that leaves between a refused send and its restore is still in the restored request, and the server names it excluded", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  // The page learns of a departure on its 15 s agent poll, which this row waits out.
  test.setTimeout(60_000);
  await setLiveSessions([planner, observer]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    const refusal = await refuseBroadcasts(page);
    await page.goto("/agents");
    await page
      .getByRole("region", { name: "Agents" })
      .getByRole("button", { name: "No Dispatch activity (2)" })
      .click();
    for (const title of ["Planner", "Observer"]) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    const composer = page.getByRole("region", { name: "Broadcast" });
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Still here?");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    const sends = page.getByRole("region", { name: "Sends" });
    await expect(sends.getByRole("status")).toHaveText(
      "Could not send to 2 agents: Envoy listener unreachable"
    );
    expect(posted[0]?.session_ids).toEqual(["planner-session", "observer-session"]);

    await setSessionLive(observer.session_id, false);
    await expect(page.getByRole("checkbox", { name: "Select Observer for broadcast" })).toHaveCount(
      0,
      { timeout: 30_000 }
    );
    refusal.allow();
    await sends.getByRole("button", { name: "Restore draft" }).click();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" })
    ).toBeVisible();
    await expect(composer.getByRole("button", { name: /^session:observer… ✕$/ })).toBeVisible();
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    expect(posted).toHaveLength(2);
    expect(posted[1]).toEqual(posted[0]);

    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(
      view.getByText(/Excluded: .*\(no live session\)\. Nothing was sent to them\./)
    ).toBeVisible();
    await expect(view.getByRole("article")).toHaveText([/Planner/]);
    await expect
      .poll(async () => (await getSentMessages()).map((entry) => entry.target_session))
      .toEqual(["planner-session"]);
  } finally {
    await alice.close();
  }
});

/** A broadcast from alice to `sessions`, each of which has answered its copy, as the
 *  `dispatch_message` reply does. */
async function answeredBroadcast(sessions: readonly string[]): Promise<string> {
  const sent = await createBroadcast({
    body: "Report status.",
    delivery: "steer",
    session_ids: [...sessions],
  });
  expect(sent.excluded).toEqual([]);
  for (const recipient of sent.recipients) {
    await replyToMessageDelivery(
      recipient.message.id,
      { attempt: 1, body: `${recipient.session_id} reporting.` },
      { id: recipient.session_id, kind: "session" }
    );
  }
  return sent.id;
}

/** An Agents-page row by the session's title. */
function agentRow(page: Page, title: string): Locator {
  return page
    .getByRole("region", { name: "Agents" })
    .locator("article")
    .filter({ has: page.getByRole("heading", { level: 2, name: title }) });
}

// LEGION-485. The broadcast page shows every recipient's answer, so opening it reads them: the
// navigation badge and each agent's row drop them, here and on the viewer's other tabs, without
// the viewer opening every row in turn.
test("opening a broadcast reads the answers it shows, on the badge, every agent's row and the viewer's other tabs", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester, observer]);
  const id = await answeredBroadcast(["tester-session", "observer-session", "planner-session"]);

  const alice = await asUser(browser, "alice");
  try {
    const agents = await alice.newPage();
    await agents.goto("/agents");
    await expect(agents.getByText("New replies 3", { exact: true })).toBeVisible();
    for (const title of ["Tester", "Observer", "Planner"]) {
      await expect(agentRow(agents, title).getByText("New reply 1", { exact: true })).toBeVisible();
    }

    const page = await alice.newPage();
    await page.goto(`/agents/broadcasts/${id}`);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByText("3 of 3 answered")).toBeVisible();
    for (const session of ["tester-session", "observer-session", "planner-session"]) {
      await expect(view.getByText(`${session} reporting.`)).toBeVisible();
    }
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("broadcast-read.png"), fullPage: true });

    // The other tab hears of each read through `user_agent_state.updated`, with no reload.
    await expect(agents.getByText(/^New repl/)).toHaveCount(0);
    for (const title of ["Tester", "Observer", "Planner"]) {
      await expect(agentRow(agents, title)).toBeVisible();
    }
    await agents.screenshot({ path: testInfo.outputPath("agents-after-read.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

// The constraint the read mark sets: it is per session and covers every older reply, so moving it
// through a broadcast answer would also read the session's older answer to another message, which
// the viewer never saw. Opening the broadcast reads its own answers alone; that older answer, and
// an answer newer than anything the page shows, both still count.
test("opening a broadcast leaves unread the answers it does not show, older and newer", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the broadcast flow");
  await setLiveSessions([planner, tester, observer]);
  const older = await createAgentMessage(planner.session_id, {
    body: "Where is the dashboard?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    older.id,
    { attempt: 1, body: "At /dash." },
    { id: planner.session_id, kind: "session" }
  );
  const id = await answeredBroadcast(["tester-session", "observer-session", "planner-session"]);
  const newer = await createAgentMessage(tester.session_id, {
    body: "Did the rerun pass?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    newer.id,
    { attempt: 1, body: "Two passed." },
    { id: tester.session_id, kind: "session" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByText("New replies 5", { exact: true })).toBeVisible();

    await page.goto(`/agents/broadcasts/${id}`);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByText("3 of 3 answered")).toBeVisible();
    await expect(page.getByText("New replies 2", { exact: true })).toBeVisible();

    await page.goto("/agents");
    await expect(page.getByText("New replies 2", { exact: true })).toBeVisible();
    await expect(agentRow(page, "Planner").getByText("New reply 1", { exact: true })).toBeVisible();
    await expect(agentRow(page, "Tester").getByText("New reply 1", { exact: true })).toBeVisible();
    await expect(agentRow(page, "Observer")).toBeVisible();
    await expect(agentRow(page, "Observer").getByText(/^New repl/)).toHaveCount(0);
    await page.screenshot({ path: testInfo.outputPath("agents-unread-kept.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});
