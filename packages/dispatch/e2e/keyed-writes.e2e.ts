import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Event } from "../web/src/api/types";
import {
  createAsk,
  createIssue,
  createMessage,
  createProject,
  devSignInPath,
  getAsk,
  getIssue,
  getIssueEvents,
  getMyState,
  slowWrites,
} from "./api";
import { dropFirstAnswer, keptWrites, replayed, watchAnswers, watchWrites } from "./keyed-writes";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Every write the page makes goes under an `Idempotency-Key` minted for the person's one action,
// and the page tries it again under that key until Dispatch answers it (`ApiClient.send`). These
// rows drive the general path against the real server, for three surfaces that are not a send to
// an agent: a comment, a status change and an ask's answer. In each the first attempt reaches
// Dispatch and its answer is lost on the way back (`route.fetch()`, then `route.abort`), so
// Dispatch did the write and the page does not know it; the page asks again under the same key,
// and Dispatch answers the repeat with the answer it stored (`Idempotency-Replayed: ?1`) rather
// than writing a second time.

const agent = {
  actor: { kind: "session" as const, id: "e2e-keyed-writes" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** The shell's one account of the writes Dispatch has not confirmed yet that no composer shows
 *  (`PendingWritesIndicator`); gone while none is pending. */
function pendingWrites(page: Page): Locator {
  return page.getByRole("region", { name: "Pending changes" });
}

/** The events of `type` on `issue`. */
async function eventsOf(issue: string, type: Event["type"]): Promise<Event[]> {
  return (await getIssueEvents(issue)).filter((event) => event.type === type);
}

/** The `issue.updated` events on `issue` that set its status to `status`. */
async function statusEvents(issue: string, status: string): Promise<Event[]> {
  return (await getIssueEvents(issue)).filter(
    (event) => event.type === "issue.updated" && event.payload.status === status
  );
}

/** Fails every `method` request matching `pattern` at the connection until `end`, as a network
 *  that is down fails it: none of them reaches Dispatch. The route stays through a reload. */
async function outage(page: Page, pattern: string, method: string): Promise<{ end: () => void }> {
  let down = true;
  await page.route(pattern, (route) =>
    down && route.request().method() === method
      ? route.abort("internetdisconnected")
      : route.fallback()
  );
  return {
    end: () => {
      down = false;
    },
  };
}

test("a comment whose answer is lost lands once, under one key", async ({ browser }) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A comment once" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const first = dropFirstAnswer(page, `**${path}`, "POST");
    const landed = replayed(page, "POST", path);
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Said once");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    expect(await first).toBe(201);
    const answer = await landed;
    expect(answer.status()).toBe(201);
    expect(posts).toHaveLength(2);
    expect(posts[0]).toEqual({
      input: expect.objectContaining({ body: "Said once" }),
      key: expect.any(String),
    });
    expect(posts[1]).toEqual(posts[0]);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${(await answer.json()).id}"]`)).toHaveCount(1);
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// The status change is the one kept through a reload: the page keeps an unconfirmed write in the
// tab's storage, asks before the tab is left, and the reloaded page sends it again under the same
// key. The shell's pending-writes indicator names it while it is out and clears once Dispatch
// answers it.
test("a status change whose answer is lost survives a reload, lands once, and the pending-writes indicator shows and clears", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A status once" });
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const patches = watchWrites(page, "PATCH", path);
    const prompts: string[] = [];
    page.on("dialog", (dialog) => {
      prompts.push(dialog.type());
      void dialog.accept();
    });
    // The first PATCH reaches Dispatch, which writes the status; its answer is held until the
    // page that asked is gone.
    const reloaded = Promise.withResolvers<void>();
    const reached = dropFirstAnswer(page, `**${path}`, "PATCH", reloaded.promise);
    const resumed = replayed(page, "PATCH", path);
    await page.goto(`/issues/${issue.key}`);
    await page.getByLabel("Status").selectOption("todo");
    expect(await reached).toBe(200);
    const indicator = pendingWrites(page);
    await expect(indicator.getByRole("status")).toHaveText("Saving 1 change…");

    await page.reload();
    reloaded.resolve();
    expect((await resumed).status()).toBe(200);
    expect(prompts).toEqual(["beforeunload"]);
    expect(patches).toHaveLength(2);
    expect(patches[0]).toEqual({ input: { status: "todo" }, key: expect.any(String) });
    expect(patches[1]).toEqual(patches[0]);
    await expect(indicator).toHaveCount(0);
    await expect(page.getByLabel("Status")).toHaveValue("todo");
    expect(await getIssue(issue.key)).toMatchObject({ status: "todo" });
    expect(await statusEvents(issue.key, "todo")).toHaveLength(1);
    // A landed write leaves the tab's queue, so the next load resumes nothing.
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

test("an ask answer whose answer is lost lands once, under one key", async ({ browser }) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "An answer once" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    agent
  );
  const path = `/api/v1/asks/${ask.id}/answer`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const first = dropFirstAnswer(page, `**${path}`, "POST");
    const landed = replayed(page, "POST", path);
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = page.getByRole("region", { name: "Conversation" }).getByTestId(`ask-${ask.id}`);
    await card.getByRole("radio", { name: "Ship" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    expect(await first).toBe(200);
    expect((await landed).status()).toBe(200);
    expect(posts).toHaveLength(2);
    expect(posts[0]).toEqual({
      input: expect.objectContaining({ selected: ["Ship"] }),
      key: expect.any(String),
    });
    expect(posts[1]).toEqual(posts[0]);
    await expect(card).toContainText(/Answered by alice/);
    expect(await getAsk(ask.id)).toMatchObject({ ask: { state: "answered" } });
    expect(await eventsOf(issue.key, "ask.answered")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// A repeat that reaches Dispatch while the first attempt under its key is still running is
// `409 KEY_IN_FLIGHT` with `Retry-After: 1`, and the page asks again under the same key. The
// first attempt is held in Dispatch by a transaction that holds the issue's row, so the 409 is
// the real server's.
test("a repeat while the first is still running is answered 409 KEY_IN_FLIGHT, and the page asks again until it lands once", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "In flight" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const answers = watchAnswers(page, "POST", path);
    await page.goto(`/issues/${issue.key}/conversation`);

    // Hold the issue's row for four seconds, so the comment's first attempt waits in Dispatch.
    const holding = sql(
      "begin",
      `select 1 from issues where key = '${issue.key}' for update`,
      "select pg_sleep(4)",
      "commit"
    );
    await expect
      .poll(() =>
        sql(
          "select count(*) from pg_stat_activity where state = 'active' and query like 'select pg_sleep(4)%'"
        )
      )
      .toBe("1");
    // The first attempt goes to Dispatch and waits there; the page is told it failed at once,
    // so it asks again under the key while the first still runs.
    let first = true;
    const firstAnswer = Promise.withResolvers<number>();
    await page.route(`**${path}`, async (route) => {
      if (route.request().method() !== "POST" || !first) return route.fallback();
      first = false;
      void route.fetch().then((response) => firstAnswer.resolve(response.status()));
      return route.abort("failed");
    });
    const landed = replayed(page, "POST", path);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Through a busy key");
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    await holding;
    expect(await firstAnswer.promise).toBe(201);
    expect((await landed).status()).toBe(201);
    expect(answers).toContainEqual({
      code: "KEY_IN_FLIGHT",
      key: posts[0]?.key,
      replayed: undefined,
      retryAfter: "1",
      status: 409,
    });
    const key = posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect(posts.every((post) => post.key === key)).toBe(true);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

// A write Dispatch takes longer to finish than the page waits on a silent attempt. Dispatch is
// slow on its own side here (the slow-write hook holds every run of the route 20 s once its key
// is claimed), not held in the browser: the page's attempt goes silent, it gives up on it at 15 s
// and asks again under the same key, and the first run, which the page's leaving does not cancel,
// still finishes. Every repeat while it runs is `409 KEY_IN_FLIGHT`; the one after is its answer.
// A run that the leaving cancelled would never finish, since every run is held as long.
test("a comment Dispatch takes 20 s to make lands once from the page, every repeat waiting on the first", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A slow comment" });
  const path = `/api/v1/issues/${issue.key}/comments`;
  await slowWrites("POST", path, 20_000);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = watchWrites(page, "POST", path);
    const answers = watchAnswers(page, "POST", path);
    const landed = replayed(page, "POST", path);
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" }).last();
    await composer.getByLabel("Comment").fill("Slow on the server");
    const pressed = Date.now();
    await composer.getByRole("button", { exact: true, name: "Send" }).click();

    const answer = await landed;
    expect(answer.status()).toBe(201);
    expect(Date.now() - pressed).toBeGreaterThanOrEqual(19_000);
    expect(answers).toContainEqual({
      code: "KEY_IN_FLIGHT",
      key: posts[0]?.key,
      replayed: undefined,
      retryAfter: "1",
      status: 409,
    });
    // Every answer is the first run's verdict or a wait on it: nothing else ran.
    expect(answers.every(({ code, status }) => status === 201 || code === "KEY_IN_FLIGHT")).toBe(
      true
    );
    expect(posts.length).toBeGreaterThanOrEqual(2);
    const key = posts[0]?.key;
    expect(key).toEqual(expect.any(String));
    expect(posts.every((post) => post.key === key)).toBe(true);
    await expect(composer.getByLabel("Comment")).toHaveValue("");
    await expect(page.getByText(/^Couldn't send/)).toHaveCount(0);
    await expect(page.locator(`[data-turn="comment:${(await answer.json()).id}"]`)).toHaveCount(1);
    expect(await eventsOf(issue.key, "comment.created")).toHaveLength(1);
  } finally {
    await slowWrites("POST", path, 0);
    await alice.close();
  }
});

// An attempt is cut off only after 15 s with no progress, and a body still going up is progress:
// an upload over a link slow enough that sending it takes 30 s is one attempt, and lands once.
// Chromium's DevTools protocol paces the page's own upload (`Network.emulateNetworkConditions`),
// so the body goes up at the throttle's rate and the page sees its progress as it goes; no other
// engine the suite runs can be throttled that way, so the row runs on Chromium's projects.
test("an upload throttled to take 30 s is one attempt and lands once", async ({
  browser,
  browserName,
}, testInfo) => {
  test.skip(browserName !== "chromium", "only Chromium's DevTools protocol paces a page's upload");
  test.setTimeout(120_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A slow upload" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const bytes = 4 * 1024 * 1024;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // A multipart body is no JSON, so the row keeps each attempt's key alone.
    const keys: (string | undefined)[] = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === path) {
        keys.push(request.headers()["idempotency-key"]);
      }
    });
    const answers = watchAnswers(page, "POST", path);
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      const reviewPanel = page.getByRole("button", { name: /review panel/i });
      if ((await reviewPanel.getAttribute("aria-expanded")) === "true") await reviewPanel.click();
    }
    await page.getByRole("tab", { name: "Artifacts" }).click();
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.alloc(bytes, 7),
      mimeType: "application/octet-stream",
      name: "slow-link.bin",
    });
    const throttle = await alice.newCDPSession(page);
    await throttle.send("Network.emulateNetworkConditions", {
      downloadThroughput: -1,
      latency: 0,
      offline: false,
      uploadThroughput: Math.floor(bytes / 30),
    });
    const created = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === path &&
        response.status() === 201,
      { timeout: 100_000 }
    );
    const pressed = Date.now();
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    await created;
    expect(Date.now() - pressed).toBeGreaterThanOrEqual(25_000);
    await throttle.send("Network.emulateNetworkConditions", {
      downloadThroughput: -1,
      latency: 0,
      offline: false,
      uploadThroughput: -1,
    });

    expect(keys).toEqual([expect.any(String)]);
    expect(answers).toEqual([expect.objectContaining({ status: 201 })]);
    await expect(page.getByTestId("artifact-slow-link-bin")).toContainText("1 version");
  } finally {
    await alice.close();
  }
});

// A pin is a write like any other: pressed while the network is down, it is kept in the tab, the
// shell's pending-writes indicator names it, and the reloaded page sends it again once the network
// is back. The Conversation's pin on a turn goes through the pin queue; the issue header's pin is
// a `PUT` of the issue's own state, which a reload resumes as it resumes a `POST` or a `PATCH`.
test("a pin on a turn pressed during an outage survives a reload and lands once the network is back", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A pin through a reload" });
  await createMessage(issue.key, { body: "Pinned through the outage" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    const network = await outage(page, "**/api/v1/me/issues/*/state", "PUT");
    await page.goto(`/issues/${issue.key}/conversation`);
    await page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ has: page.getByText("Pinned through the outage", { exact: true }) })
      .getByRole("button", { name: "Pin" })
      .click();
    await expect(pendingWrites(page)).toBeVisible();
    await expect.poll(() => keptWrites(page)).toBe(1);
    expect((await getMyState())[issue.key]?.dismissed ?? []).toEqual([]);

    await page.reload();
    network.end();
    await expect
      .poll(async () => (await getMyState())[issue.key]?.dismissed ?? [], { timeout: 30_000 })
      .toHaveLength(1);
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
  } finally {
    await alice.close();
  }
});

test("the issue header's pin pressed during an outage survives a reload and lands once the network is back", async ({
  browser,
}) => {
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A header pin through a reload" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    page.on("dialog", (dialog) => void dialog.accept());
    const puts = watchWrites(page, "PUT", `/api/v1/me/issues/${issue.key}/state`);
    const network = await outage(page, "**/api/v1/me/issues/*/state", "PUT");
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { name: "Pin issue" }).click();
    await expect(pendingWrites(page)).toBeVisible();
    await expect.poll(() => keptWrites(page)).toBe(1);
    expect((await getMyState())[issue.key]?.pinned ?? false).toBe(false);

    await page.reload();
    network.end();
    await expect
      .poll(async () => (await getMyState())[issue.key]?.pinned ?? false, { timeout: 30_000 })
      .toBe(true);
    await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
    // The same press before and after the reload: one body, the header's own write.
    expect(puts.length).toBeGreaterThanOrEqual(2);
    for (const put of puts) expect(put.input).toEqual({ pinned: true });
  } finally {
    await alice.close();
  }
});

// A write belongs to the person who pressed it. Alice changes a status while the network is down;
// then bob signs in from another tab of the same browser, which replaces the cookie every tab
// sends. Alice's write is dropped, in memory and in the tab's storage, and nothing runs as bob: an
// attempt that still reaches Dispatch names alice (`Dispatch-Login`), so Dispatch refuses it
// `409 LOGIN_CHANGED` before anything runs.
test("a write pending for alice is dropped when bob signs in from another tab, and nothing runs as bob", async ({
  browser,
}) => {
  test.setTimeout(90_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Alice's change" });
  const before = (await getIssue(issue.key)).status;
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const answers = watchAnswers(page, "PATCH", path);
    const logins: (string | undefined)[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
        logins.push(request.headers()["dispatch-login"]);
      }
    });
    const network = await outage(page, `**${path}`, "PATCH");
    await page.goto(`/issues/${issue.key}`);
    await page.getByLabel("Status").selectOption("todo");
    await expect(pendingWrites(page).getByRole("status")).toHaveText("Saving 1 change…");
    await expect.poll(() => keptWrites(page)).toBe(1);

    const other = await alice.newPage();
    await other.goto(devSignInPath("bob"));
    await expect
      .poll(() =>
        other.evaluate(async () => {
          const whoami: unknown = await (await fetch("/auth/whoami")).json();
          return typeof whoami === "object" && whoami !== null && "login" in whoami
            ? whoami.login
            : null;
        })
      )
      .toBe("bob");
    network.end();

    await expect(pendingWrites(page)).toHaveCount(0, { timeout: 45_000 });
    await expect.poll(() => keptWrites(page)).toBe(0);
    // Every attempt named alice, and each that reached Dispatch was refused before it ran.
    expect(logins.every((login) => login === "alice")).toBe(true);
    for (const answer of answers) {
      expect(answer).toMatchObject({ code: "LOGIN_CHANGED", status: 409 });
    }
    expect((await getIssue(issue.key)).status).toBe(before);
    expect(await statusEvents(issue.key, "todo")).toEqual([]);
  } finally {
    await alice.close();
  }
});
