import { expect, type Page, test } from "@playwright/test";

import {
  agentRow,
  getSentMessages,
  openAgents,
  plannerSession,
  postedBroadcasts,
  reviewerSession,
  seedAgents,
  setSessionSendDelay,
} from "./agents";
import { listComments, publishAgentStreamFrame, slowWrites } from "./api";
import { dropFirstAnswer, replayed, watchAnswers, watchWrites } from "./keyed-writes";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A message to one session goes under an `Idempotency-Key`, and the page tries it again under
// that key until Dispatch answers it (`writeUnderKey`, the one loop every write the page makes
// runs); a broadcast is keyed and kept the same way. These rows run both against the real server:
// an attempt whose answer the page never gets, and one a server that is down answers 503, each
// end in one message per recipient and the composer cleared. Dispatch answers a repeat under a key
// it has answered with the first answer's status, marked `Idempotency-Replayed: ?1`.

const messages = `/api/v1/agents/${plannerSession.session_id}/messages`;
const broadcasts = "/api/v1/broadcasts";

/** Opens the Planner's composer from the keyboard, types `body` and sends it. */
async function send(page: Page, body: string): Promise<void> {
  const field = agentRow(page, plannerSession.session_id).getByRole("textbox", { name: "Comment" });
  await page.keyboard.press("j");
  await page.keyboard.press("Enter");
  await expect(field).toBeFocused();
  await page.keyboard.type(body);
  await page.keyboard.press("Control+Enter");
}

/** Ticks every session the page lists, types `body` into the broadcast composer and sends it. */
async function broadcast(page: Page, body: string): Promise<void> {
  for (const title of ["Planner", "Reviewer"]) {
    await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
  }
  const composer = page.getByRole("region", { name: "Broadcast" });
  await composer.getByRole("textbox", { name: "Broadcast message" }).fill(body);
  await composer.getByRole("button", { name: "Send to 2" }).click();
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a send whose answer is lost is tried again under its key, and delivered once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", messages);
    // The first POST reaches Dispatch, which makes the message and sends it; its answer is lost on
    // the way back, as a dropped connection loses it.
    const firstAnswer = dropFirstAnswer(page, `**${messages}`, "POST");
    const landed = replayed(page, "POST", messages);
    await openAgents(page);
    await send(page, "Exactly once, please");

    const answer = await landed;
    expect(await firstAnswer).toBe(201);
    // The repeat is answered what the first attempt was.
    expect(answer.status()).toBe(201);
    expect(posts).toHaveLength(2);
    const [first, second] = posts;
    expect(first).toEqual({
      input: { body: "Exactly once, please", delivery: "aside" },
      key: expect.any(String),
    });
    // The same message under the same key: Dispatch answers it with the message it made.
    expect(second).toEqual(first);
    expect((await answer.json()).body).toBe("Exactly once, please");
    expect(await getSentMessages()).toMatchObject([
      { message: "Exactly once, please", target_session: plannerSession.session_id },
    ]);
    const row = agentRow(page, plannerSession.session_id);
    await expect(row.getByRole("textbox", { name: "Comment" })).toHaveValue("");
    await expect(row.getByText(/^Couldn't send/)).toHaveCount(0);
    expect(await getSentMessages()).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a send a server that is down answers 503 is tried again, not refused, and delivered once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", messages);
    let refused = false;
    await page.route(`**${messages}`, async (route) => {
      if (route.request().method() !== "POST" || refused) return route.fallback();
      refused = true;
      return route.fulfill({
        body: JSON.stringify({ code: "UNAVAILABLE", error: "the server is down" }),
        contentType: "application/json",
        status: 503,
      });
    });
    const landed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === messages &&
        response.status() === 201
    );
    await openAgents(page);
    await send(page, "Once, after the outage");

    // The 503 never reached Dispatch, so the second attempt is the first it answers.
    expect((await landed).headers()["idempotency-replayed"]).toBeUndefined();
    expect(refused).toBe(true);
    expect(posts).toHaveLength(2);
    const [first, second] = posts;
    expect(first?.key).toEqual(expect.any(String));
    expect(second).toEqual(first);
    const row = agentRow(page, plannerSession.session_id);
    await expect(row.getByRole("textbox", { name: "Comment" })).toHaveValue("");
    await expect(row.getByText(/^Couldn't send/)).toHaveCount(0);
    expect(await getSentMessages()).toMatchObject([
      { message: "Once, after the outage", target_session: plannerSession.session_id },
    ]);
    expect(await getSentMessages()).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// The tab keeps an unconfirmed send (`sessionStorage`) and asks before it is left. This row means
// to leave: it accepts the prompt, reloads while the first attempt's answer is still out, and the
// reloaded page sends the message again under the same key, which Dispatch answers with the
// message the first attempt made.
test("a send held through a reload is resumed under its key, and delivered once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", messages);
    const prompts: string[] = [];
    page.on("dialog", (dialog) => {
      prompts.push(dialog.type());
      void dialog.accept();
    });
    // The first POST reaches Dispatch, which makes the message and sends it; its answer is held
    // until the page that asked is gone.
    const reloaded = Promise.withResolvers<void>();
    const firstAnswer = dropFirstAnswer(page, `**${messages}`, "POST", reloaded.promise);
    const resumed = replayed(page, "POST", messages);
    await openAgents(page);
    await send(page, "Kept through the reload");
    expect(await firstAnswer).toBe(201);

    await page.reload();
    reloaded.resolve();
    await resumed;
    expect(prompts).toEqual(["beforeunload"]);
    expect(posts).toHaveLength(2);
    const [first, second] = posts;
    expect(first).toEqual({
      input: { body: "Kept through the reload", delivery: "aside" },
      key: expect.any(String),
    });
    expect(second).toEqual(first);
    expect(await getSentMessages()).toMatchObject([
      { message: "Kept through the reload", target_session: plannerSession.session_id },
    ]);
    expect(await getSentMessages()).toHaveLength(1);
    // A landed send leaves the tab's storage, so the next load resumes nothing.
    await expect
      .poll(() =>
        page.evaluate(
          () =>
            Object.keys(window.sessionStorage).filter((name) => name.startsWith("dispatch.write:"))
              .length
        )
      )
      .toBe(0);
  } finally {
    await alice.close();
  }
});

// A broadcast is keyed too, and the same loss of an answer is the same outcome: Dispatch made the
// broadcast and sent every recipient their copy, and the repeat under that key is answered with it
// rather than making a second one.
test("a broadcast whose answer is lost is tried again under its key, and each agent hears it once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    const firstAnswer = dropFirstAnswer(page, `**${broadcasts}`, "POST");
    const landed = replayed(page, "POST", broadcasts);
    await openAgents(page);
    await broadcast(page, "Exactly once, everyone");

    const answer = await landed;
    expect(await firstAnswer).toBe(201);
    // The page opens the broadcast the repeat was answered with, which is the one the lost
    // answer made.
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/u);
    expect(page.url().split("/").at(-1)).toBe((await answer.json()).id);
    expect(posted.length).toBeGreaterThanOrEqual(2);
    const [first] = posted;
    expect(first).toEqual({
      input: {
        body: "Exactly once, everyone",
        delivery: "btw",
        session_ids: [plannerSession.session_id, reviewerSession.session_id],
      },
      key: expect.any(String),
    });
    // Every attempt is that one press: the same request under the same key.
    for (const attempt of posted) expect(attempt).toEqual(first);
    await expect
      .poll(async () => (await getSentMessages()).map((entry) => entry.target_session).sort())
      .toEqual([plannerSession.session_id, reviewerSession.session_id].sort());
    expect(await getSentMessages()).toHaveLength(2);
    await expect(page.getByText(/^Could not send/)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The broadcast kept through a reload: the tab asks before it is left, this row means to leave,
// and the reloaded page sends the identical request under the same key, which Dispatch answers
// with the broadcast the first attempt made. It was pressed before this visit, so the page stays
// on Agents and its strip links the broadcast rather than opening it.
test("a broadcast held through a reload is resumed under its key, and each agent hears it once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    const prompts: string[] = [];
    page.on("dialog", (dialog) => {
      prompts.push(dialog.type());
      void dialog.accept();
    });
    const reloaded = Promise.withResolvers<void>();
    const firstAnswer = dropFirstAnswer(page, `**${broadcasts}`, "POST", reloaded.promise);
    const resumed = replayed(page, "POST", broadcasts);
    await openAgents(page);
    await broadcast(page, "Kept through the reload");
    expect(await firstAnswer).toBe(201);

    await page.reload();
    reloaded.resolve();
    await resumed;
    expect(prompts).toEqual(["beforeunload"]);
    expect(posted.length).toBeGreaterThanOrEqual(2);
    const [first] = posted;
    expect(first).toEqual({
      input: {
        body: "Kept through the reload",
        delivery: "btw",
        session_ids: [plannerSession.session_id, reviewerSession.session_id],
      },
      key: expect.any(String),
    });
    for (const attempt of posted) expect(attempt).toEqual(first);
    await expect(
      page.getByRole("region", { name: "Sends" }).getByRole("link", { name: "Sent to 2 agents" })
    ).toBeVisible();
    await expect
      .poll(async () => (await getSentMessages()).map((entry) => entry.target_session).sort())
      .toEqual([plannerSession.session_id, reviewerSession.session_id].sort());
    expect(await getSentMessages()).toHaveLength(2);
    // A landed broadcast leaves the tab's storage, so the next load resumes nothing.
    await expect
      .poll(() =>
        page.evaluate(
          () =>
            Object.keys(window.sessionStorage).filter((name) => name.startsWith("dispatch.write:"))
              .length
        )
      )
      .toBe(0);
  } finally {
    await alice.close();
  }
});

// INFRA-442: Send is pressable whenever the composer holds text. Each press is its own write under
// its own key, and the writes to one session go out in the order they were pressed. Two presses
// while the network is down are both held, Send stays pressable, and once the network is back the
// session hears the first and then the second, each once.
test("two live-view sends pressed during an outage go out in press order, each once, with Send pressable throughout", async ({
  browser,
}) => {
  test.setTimeout(90_000);
  await seedAgents();
  await publishAgentStreamFrame(
    plannerSession.session_id,
    {
      frames: [
        {
          kind: "message",
          message: {
            at: 1_000,
            id: "u1000",
            parts: [{ text: "Start the release notes", type: "text" }],
            role: "user",
            streaming: false,
          },
          seq: 1,
          v: 1,
        },
      ],
      session_id: plannerSession.session_id,
      v: 1,
    },
    "replay"
  );
  const first = "First, pressed while the network was down";
  const second = "Second, pressed after the first";
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", messages);
    const answers = watchAnswers(page, "POST", messages);
    await page.goto(`/agents/${plannerSession.session_id}/live`);
    await expect(page.getByTestId("agent-thread")).toContainText("Start the release notes");
    const composer = page.getByTestId("agent-composer");
    const field = composer.locator("textarea");
    const send = composer.getByRole("button", { exact: true, name: "Send" });

    await alice.setOffline(true);
    for (const body of [first, second]) {
      await field.fill(body);
      await expect(send).toBeEnabled();
      await send.click();
      await expect(field).toHaveValue("");
    }
    // Both are out and neither has reached Dispatch, and Send still takes the next message.
    await field.fill("A third, typed while both are out");
    await expect(send).toBeEnabled();
    await field.fill("");
    expect(await getSentMessages()).toEqual([]);

    await alice.setOffline(false);
    await expect
      .poll(() => new Set(answers.filter((a) => a.status === 201).map((a) => a.key)).size, {
        timeout: 45_000,
      })
      .toBe(2);
    expect((await getSentMessages()).map((sent) => sent.message)).toEqual([first, second]);
    // Two presses, two keys, the first press's key first.
    const keys = [...new Set(posts.map((post) => post.key))];
    expect(keys).toHaveLength(2);
    expect(posts.find((post) => post.key === keys[0])?.input).toMatchObject({ body: first });
    expect(posts.find((post) => post.key === keys[1])?.input).toMatchObject({ body: second });
  } finally {
    await alice.close();
  }
});

// A comment's @-mention is delivered once even when the page gives up on the comment while
// Dispatch is still delivering it. Dispatch is slow on its own side (the slow-write hook holds the
// comment 12 s once its key is claimed) and the listener takes 4 s to answer the mention's send,
// so the delivery runs from about 12 s to 16 s, and the page's attempt, silent all that time, is
// cut off at 15 s, mid-delivery. The first run finishes the delivery regardless; the page's
// repeat waits on it and is answered with the comment, and the session hears the mention once.
test("a comment whose @-mention is cut off mid-delivery by the page is delivered once", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const issueKey = await seedAgents();
  const path = `/api/v1/issues/${issueKey}/comments`;
  await slowWrites("POST", path, 12_000);
  await setSessionSendDelay(plannerSession.session_id, 4_000);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const landed = replayed(page, "POST", path);
    await page.goto(`/issues/${issueKey}/conversation`);
    const form = page.getByRole("form", { name: "Comment composer" }).last();
    const field = form.getByLabel("Comment");
    await field.fill("Heads up @");
    await page
      .getByRole("listbox", { name: "Mention suggestions" })
      .getByRole("option", { exact: true, name: "Planner" })
      .first()
      .click();
    await form.getByRole("button", { exact: true, name: "Send" }).click();

    const answer = await landed;
    expect(answer.status()).toBe(201);
    const comment: { id: string } = await answer.json();
    // The page gave up on its first attempt mid-delivery and asked again under the same key.
    expect(posts.length).toBeGreaterThanOrEqual(2);
    expect(new Set(posts.map((post) => post.key)).size).toBe(1);
    // The mention's one attempt was delivered, within the first run, not by a later sweep.
    await expect
      .poll(
        async () =>
          (await listComments(issueKey))
            .find((listed) => listed.id === comment.id)
            ?.deliveries.filter((delivery) => delivery.session_id === plannerSession.session_id)
            .map((delivery) => ({ attempt: delivery.attempt, state: delivery.state })),
        { timeout: 10_000 }
      )
      .toEqual([{ attempt: 1, state: "sent" }]);
    expect(
      (await getSentMessages()).filter((sent) => sent.target_session === plannerSession.session_id)
    ).toHaveLength(1);
    await expect(field).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
  } finally {
    await slowWrites("POST", path, 0);
    await setSessionSendDelay(plannerSession.session_id, 0);
    await alice.close();
  }
});
