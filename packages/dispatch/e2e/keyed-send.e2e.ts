import { expect, type Page, type Request, test } from "@playwright/test";

import {
  agentRow,
  getSentMessages,
  openAgents,
  plannerSession,
  postedBroadcasts,
  reviewerSession,
  seedAgents,
} from "./agents";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A message to one session goes under an idempotency key, and the page tries it again under that
// key until Dispatch answers it (`sendKeyedAgentMessage`); a broadcast is keyed and kept the same
// way (`broadcast-queue.ts`). These rows run both against the real server: an attempt whose answer
// the page never gets, and one a server that is down answers 503, each end in one message per
// recipient and the composer cleared.

const messages = `/api/v1/agents/${plannerSession.session_id}/messages`;
const broadcasts = "/api/v1/broadcasts";

/** Every POST the page makes to the Planner's message route, in order. */
function watchPosts(page: Page): Request[] {
  const posts: Request[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && new URL(request.url()).pathname === messages) {
      posts.push(request);
    }
  });
  return posts;
}

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
    const posts = watchPosts(page);
    // The first POST reaches Dispatch, which makes the message and sends it; its answer is lost on
    // the way back, as a dropped connection loses it.
    let firstAnswer: number | undefined;
    await page.route(`**${messages}`, async (route) => {
      if (route.request().method() !== "POST" || firstAnswer !== undefined) {
        return route.fallback();
      }
      firstAnswer = (await route.fetch()).status();
      return route.abort("failed");
    });
    const landed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === messages &&
        response.status() === 200
    );
    await openAgents(page);
    await send(page, "Exactly once, please");

    const answer = await landed;
    expect(firstAnswer).toBe(201);
    expect(posts).toHaveLength(2);
    const [first, second] = posts.map((post) => post.postDataJSON());
    expect(first).toEqual({
      body: "Exactly once, please",
      delivery: "aside",
      idempotency_key: expect.any(String),
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
    const posts = watchPosts(page);
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

    await landed;
    expect(refused).toBe(true);
    expect(posts).toHaveLength(2);
    const [first, second] = posts.map((post) => post.postDataJSON());
    expect(first.idempotency_key).toEqual(expect.any(String));
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
    const posts = watchPosts(page);
    const prompts: string[] = [];
    page.on("dialog", (dialog) => {
      prompts.push(dialog.type());
      void dialog.accept();
    });
    // The first POST reaches Dispatch, which makes the message and sends it; its answer is held
    // until the page that asked is gone.
    const reached = Promise.withResolvers<void>();
    const reloaded = Promise.withResolvers<void>();
    let firstAnswer: number | undefined;
    await page.route(`**${messages}`, async (route) => {
      if (route.request().method() !== "POST" || firstAnswer !== undefined) {
        return route.fallback();
      }
      firstAnswer = (await route.fetch()).status();
      reached.resolve();
      await reloaded.promise;
      // The page cancelled the request with the reload; there is nothing left to answer.
      await route.abort("failed").catch(() => undefined);
    });
    const resumed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === messages &&
        response.status() === 200
    );
    await openAgents(page);
    await send(page, "Kept through the reload");
    await reached.promise;
    expect(firstAnswer).toBe(201);

    await page.reload();
    reloaded.resolve();
    await resumed;
    expect(prompts).toEqual(["beforeunload"]);
    expect(posts).toHaveLength(2);
    const [first, second] = posts.map((post) => post.postDataJSON());
    expect(first).toEqual({
      body: "Kept through the reload",
      delivery: "aside",
      idempotency_key: expect.any(String),
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
            Object.keys(window.sessionStorage).filter((name) =>
              name.startsWith("dispatch.keyed-send:")
            ).length
        )
      )
      .toBe(0);
  } finally {
    await alice.close();
  }
});

// A broadcast is keyed too, and the same loss of an answer is the same outcome: Dispatch made the
// broadcast and sent every recipient their copy, and the repeat under that key is answered with it
// (200) rather than making a second one.
test("a broadcast whose answer is lost is tried again under its key, and each agent hears it once", async ({
  browser,
}) => {
  await seedAgents();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    let firstAnswer: number | undefined;
    await page.route(`**${broadcasts}`, async (route) => {
      if (route.request().method() !== "POST" || firstAnswer !== undefined) {
        return route.fallback();
      }
      firstAnswer = (await route.fetch()).status();
      return route.abort("failed");
    });
    const landed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === broadcasts &&
        response.status() === 200
    );
    await openAgents(page);
    await broadcast(page, "Exactly once, everyone");

    const answer = await landed;
    expect(firstAnswer).toBe(201);
    // The page opens the broadcast the repeat was answered with, which is the one the lost
    // answer made.
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/u);
    expect(page.url().split("/").at(-1)).toBe((await answer.json()).id);
    expect(posted).toHaveLength(2);
    const [first, second] = posted;
    expect(first).toEqual({
      body: "Exactly once, everyone",
      delivery: "btw",
      idempotency_key: expect.any(String),
      session_ids: [plannerSession.session_id, reviewerSession.session_id],
    });
    expect(second).toEqual(first);
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
    const reached = Promise.withResolvers<void>();
    const reloaded = Promise.withResolvers<void>();
    let firstAnswer: number | undefined;
    await page.route(`**${broadcasts}`, async (route) => {
      if (route.request().method() !== "POST" || firstAnswer !== undefined) {
        return route.fallback();
      }
      firstAnswer = (await route.fetch()).status();
      reached.resolve();
      await reloaded.promise;
      // The page cancelled the request with the reload; there is nothing left to answer.
      await route.abort("failed").catch(() => undefined);
    });
    const resumed = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === broadcasts &&
        response.status() === 200
    );
    await openAgents(page);
    await broadcast(page, "Kept through the reload");
    await reached.promise;
    expect(firstAnswer).toBe(201);

    await page.reload();
    reloaded.resolve();
    await resumed;
    expect(prompts).toEqual(["beforeunload"]);
    expect(posted).toHaveLength(2);
    const [first, second] = posted;
    expect(first).toEqual({
      body: "Kept through the reload",
      delivery: "btw",
      idempotency_key: expect.any(String),
      session_ids: [plannerSession.session_id, reviewerSession.session_id],
    });
    expect(second).toEqual(first);
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
            Object.keys(window.sessionStorage).filter((name) =>
              name.startsWith("dispatch.keyed-send:")
            ).length
        )
      )
      .toBe(0);
  } finally {
    await alice.close();
  }
});
