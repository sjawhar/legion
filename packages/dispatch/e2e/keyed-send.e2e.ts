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
import {
  dropFirstAnswer,
  keptWrites,
  replayed,
  runClockUntil,
  watchAnswers,
  watchWrites,
} from "./keyed-writes";
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

/** Every attempt Dispatch has made to deliver a mention of the Planner on `issueKey`'s comments,
 *  as it has them now: the comment it belongs to, which attempt it is and where it stands. Read
 *  over every comment rather than one, since the row reads it before Dispatch has answered the
 *  write and named the comment. */
async function plannerAttempts(
  issueKey: string
): Promise<{ attempt: number; comment: string; state: string }[]> {
  return (await listComments(issueKey))
    .flatMap(({ deliveries }) => deliveries)
    .filter((delivery) => delivery.session_id === plannerSession.session_id)
    .map(({ attempt, comment_id, state }) => ({ attempt, comment: comment_id, state }));
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a send whose answer is lost is tried again under its key, and delivered once @phone", async ({
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
    // The replay waits `FIRST_BACKOFF_MS` after the lost answer; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await openAgents(page);
    await send(page, "Exactly once, please");

    const answer = await runClockUntil(page, landed);
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

test("a send a server that is down answers 503 is tried again, not refused, and delivered once @phone", async ({
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
    // The second attempt waits `FIRST_BACKOFF_MS` after the 503; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await openAgents(page);
    await send(page, "Once, after the outage");

    // The 503 never reached Dispatch, so the second attempt is the first it answers.
    expect((await runClockUntil(page, landed)).headers()["idempotency-replayed"]).toBeUndefined();
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
// SLOW(>=2s): One cold page load and one reload, in WebKit only; the row is already the shared
// per-test cost plus its own single step.
test("a send held through a reload is resumed under its key, and delivered once @phone", async ({
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
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

// A broadcast is keyed too, and the same loss of an answer is the same outcome: Dispatch made the
// broadcast and sent every recipient their copy, and the repeat under that key is answered with it
// rather than making a second one.
test("a broadcast whose answer is lost is tried again under its key, and each agent hears it once @phone", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    const firstAnswer = dropFirstAnswer(page, `**${broadcasts}`, "POST");
    const landed = replayed(page, "POST", broadcasts);
    // The replay waits `FIRST_BACKOFF_MS` after the lost answer; the page's own clock runs that
    // wait out, so nothing here is spent on it.
    await page.clock.install();
    await openAgents(page);
    await broadcast(page, "Exactly once, everyone");

    const answer = await runClockUntil(page, landed);
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
// SLOW(>=2s): One cold page load and one reload, in WebKit only; the row is already the shared
// per-test cost plus its own single step.
test("a broadcast held through a reload is resumed under its key, and each agent hears it once @phone", async ({
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
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

// INFRA-442: Send is pressable whenever the composer holds text. Each press is its own write under
// its own key, and the writes to one session go out in the order they were pressed. Two presses
// while the network is down are both held, Send stays pressable, and once the network is back the
// session hears the first and then the second, each once.
test("two live-view sends pressed during an outage go out in press order, each once, with Send pressable throughout @phone", async ({
  browser,
}) => {
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
    await page.clock.install();
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
    // Both presses are queued, so driving the clock cannot reorder them: it only runs out the
    // backoff each held attempt is waiting on (`pause`, `keyed-write.ts`).
    await runClockUntil(
      page,
      expect
        .poll(() => new Set(answers.filter((a) => a.status === 201).map((a) => a.key)).size, {
          timeout: 30_000,
        })
        .toBe(2)
    );
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
// Dispatch is still delivering it. Dispatch is slow on its own side (the slow-write hook holds
// the comment 1.2 s once its key is claimed) and the listener takes 2 s to answer the mention's
// send, so the delivery runs from about 1.2 s to 3.2 s of the row's real time. The page's
// attempt, silent all that time, is cut off at the 15 s it waits on one silent attempt
// (`KEYED_SEND_ATTEMPT_MS`), which its own clock is jumped past inside that window - both holds
// are the server's own time, which no page clock can move, so they are as short as the ordering
// allows while the 15 s constant is untouched. The first run finishes the delivery regardless;
// the page's repeat waits on it and is answered with the comment, and the session hears the
// mention once.
// SLOW(>=2s): Dispatch's held write and the listener's send delay are real time on the server's
// side: the page's attempt must be cut off between the delivery's start and its end, so the
// hook's delay (1.2 s) and the listener's (2 s) are the shortest values that keep that order
// observable. The page's own 15 s cut-off is driven with `page.clock`.
test("a comment whose @-mention is cut off mid-delivery by the page is delivered once @phone", async ({
  browser,
}) => {
  const issueKey = await seedAgents();
  const path = `/api/v1/issues/${issueKey}/comments`;
  await slowWrites("POST", path, 1_200);
  await setSessionSendDelay(plannerSession.session_id, 2_000);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const landed = replayed(page, "POST", path);
    await page.clock.install();
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

    // The delivery has started and has not finished: the fake listener records a send when it
    // takes it and answers `send_delay_ms` later (`e2e/fake-envoy.ts`), and Dispatch still has
    // the attempt `pending`. That is the window the page's cut-off has to land in, and the page's
    // own clock is what puts it there.
    await expect.poll(() => getSentMessages(), { timeout: 15_000 }).toHaveLength(1);
    expect(await plannerAttempts(issueKey)).toEqual([
      { attempt: 1, comment: expect.any(String), state: "pending" },
    ]);
    await page.clock.fastForward(15_000);

    const answer = await landed;
    expect(answer.status()).toBe(201);
    const comment: { id: string } = await answer.json();
    // The page gave up on its first attempt mid-delivery and asked again under the same key.
    expect(posts.length).toBeGreaterThanOrEqual(2);
    expect(new Set(posts.map((post) => post.key)).size).toBe(1);
    // The mention's one attempt was delivered, within the first run, not by a later sweep, and it
    // is the landed comment's.
    await expect
      .poll(() => plannerAttempts(issueKey), { timeout: 10_000 })
      .toEqual([{ attempt: 1, comment: comment.id, state: "sent" }]);
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
