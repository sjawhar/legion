import { type AddressInfo, createServer, type Socket } from "node:net";

import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Event } from "../web/src/api/types";
import {
  baseUrl,
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
  userHeaders,
} from "./api";
import {
  dropFirstAnswer,
  keptWrites,
  recordUploadAttempts,
  replayed,
  watchAnswers,
  watchWrites,
} from "./keyed-writes";
import { httpsTarget, plainHttpSkipReason } from "./plain-http-origin";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asPlainHttpUser, asUser } from "./users";

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

/** The `issue.updated` events on `issue` that set its status to `status`, read with the agent
 *  token, which a person's sign-out leaves working. */
async function statusEvents(issue: string, status: string): Promise<Event[]> {
  return (await getIssueEvents(issue, {}, agent)).filter(
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

/** Holds `page`'s status changes to `issue` at the network - each goes out and is never answered -
 *  and lets every other request through; then picks `backlog` and adds `label` behind it, which
 *  waits its turn on the issue's line, never sent. */
async function queueBehindAHeldStatus(page: Page, issue: string, label: string): Promise<void> {
  await page.route(`**/api/v1/issues/${issue}`, (route) => {
    const request = route.request();
    if (request.method() === "PATCH" && request.postData()?.includes('"status"')) return;
    return route.fallback();
  });
  await page.bringToFront();
  await page.getByLabel("Status").selectOption("backlog");
  await expect(pendingWrites(page).getByRole("status")).toHaveText("Saving 1 change…");
  const editLabels = page.getByRole("button", { name: "Edit labels" });
  await editLabels.click();
  const labelSearch = page.getByRole("combobox", { name: "Search or create label" });
  await labelSearch.fill(label);
  await expect(page.getByRole("option", { name: `Create "${label}"` })).toBeVisible();
  await labelSearch.press("Enter");
  await editLabels.click();
  await expect(pendingWrites(page)).toContainText(`Labels of ${issue} — Waiting to send`);
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

// A repeat of an upload whose first request still runs is answered `409 KEY_IN_FLIGHT`, and each
// engine must hand the page that answer rather than a failed connection, which the page would take
// for a transport failure and back off from. WebKit and Firefox send the whole of
// an upload before they read its answer: an answer Dispatch wrote with the body still coming reached
// them as a reset connection, so Dispatch reads and discards the rest of the body first
// (`drainBeforeAnswer`). The slow-write hook holds the first run 25 s once its key is claimed, so
// the page gives up on the silent attempt at 15 s and asks again under its key, with the whole
// 20 MB body each time, while that run still holds the key. On Chromium's projects the upload is
// throttled to take 8 s; no other engine can be throttled from Playwright. The page's own
// `XMLHttpRequest` is the witness of how each attempt ended.
test("a 20 MB upload repeated while its first request runs is answered 409 KEY_IN_FLIGHT, never a failed connection, and lands once", async ({
  browser,
  browserName,
}, testInfo) => {
  test.setTimeout(150_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "A repeated upload" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const bytes = 20 * 1024 * 1024;
  await slowWrites("POST", path, 25_000);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const attempts = await recordUploadAttempts(page, path);
    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      const reviewPanel = page.getByRole("button", { name: /review panel/i });
      if ((await reviewPanel.getAttribute("aria-expanded")) === "true") await reviewPanel.click();
    }
    await page.getByRole("tab", { name: "Artifacts" }).click();
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.alloc(bytes, 7),
      mimeType: "application/octet-stream",
      name: "in-flight.bin",
    });
    if (browserName === "chromium") {
      const throttle = await alice.newCDPSession(page);
      await throttle.send("Network.emulateNetworkConditions", {
        downloadThroughput: -1,
        latency: 0,
        offline: false,
        uploadThroughput: Math.floor(bytes / 8),
      });
    }
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    await expect
      .poll(async () => (await attempts()).at(-1), { timeout: 120_000 })
      .toMatchObject({ ended: "load", status: 201 });

    const [first, ...repeats] = await attempts();
    // The first attempt got no answer while Dispatch held its run: the page gave it up after 15 s
    // of silence (`abort`), or the browser dropped the quiet connection first (`error`) - Firefox
    // closes an HTTP connection that carries no traffic about 6 s after the host's network changes,
    // which a host that adds and removes an interface while the row runs does. Either way it is a
    // transport failure the page asks again after, under the same key.
    expect(["abort", "error"]).toContain(first?.ended);
    expect(first?.status).toBeUndefined();
    // Every repeat was handed Dispatch's answer: none ended as a failed connection.
    expect(repeats.map(({ ended }) => ended)).toEqual(repeats.map(() => "load"));
    const waits = repeats.slice(0, -1);
    expect(waits.length).toBeGreaterThanOrEqual(1);
    for (const wait of waits) {
      expect(wait).toMatchObject({ code: "KEY_IN_FLIGHT", status: 409 });
    }
    const key = first?.key;
    expect(key).toEqual(expect.any(String));
    expect(repeats.every((repeat) => repeat.key === key)).toBe(true);
    await expect(page.getByTestId("artifact-in-flight-bin")).toContainText("1 version");
    expect(
      await sql(
        `select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '${issue.key}' and a.name = 'in-flight.bin'`
      )
    ).toBe("1");
  } finally {
    await slowWrites("POST", path, 0);
    await alice.close();
  }
});

// Signing out discards the person's unsent writes and starts none of them. In each of two tabs of
// alice's, a status change is held at the network, out and never answered, and a label added
// behind it on the same issue waits its turn. Tab 1 signs out and the person confirms the discard:
// tab 1 drops its writes, and tab 2, told over the sign-in channel, drops alice's. Dropping the
// held write must not start the one queued behind it - a page that advanced the line as it
// dropped sent the label at the confirm, and it landed: neither tab makes another write after the
// confirm, and neither issue changes. Both tabs are loaded before either write is pressed, so the
// sign-out comes seconds after the held attempts, well inside the 15 s the page waits on a silent
// one before asking again. The `chromium-plain-http-sign-out` project runs it on a page that is no
// secure context, whose browser has no `navigator.locks`: there the other tab's hold is the lease
// the signing-out tab renews, not its lock.
test("a sign-out confirmed with a write queued behind a held one sends nothing after the confirm, in either tab", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name.startsWith("chromium-plain-http") && httpsTarget,
    plainHttpSkipReason
  );
  test.setTimeout(90_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issues = [
    await createIssue({ project: "KW", title: "Tab one's change" }),
    await createIssue({ project: "KW", title: "Tab two's change" }),
  ];
  const before = await Promise.all(issues.map((issue) => getIssue(issue.key)));
  const alice = testInfo.project.name.startsWith("chromium-plain-http")
    ? await asPlainHttpUser(browser, "alice")
    : await asUser(browser, "alice");
  try {
    let confirmedAt: number | undefined;
    const writes: { at: number; body: string }[] = [];
    const tabs: Page[] = [];
    for (const issue of issues) {
      const page = await alice.newPage();
      const path = `/api/v1/issues/${issue.key}`;
      page.on("dialog", (dialog) => {
        if (dialog.type() === "confirm") confirmedAt ??= Date.now();
        void dialog.accept();
      });
      page.on("request", (request) => {
        if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
          writes.push({ at: Date.now(), body: request.postData() ?? "" });
        }
      });
      await page.goto(`/issues/${issue.key}`);
      await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
      tabs.push(page);
    }
    for (const [index, page] of tabs.entries()) {
      await queueBehindAHeldStatus(page, issues[index]?.key ?? "", `discarded-${index + 1}`);
    }
    // The labels wait behind the held status changes: none has gone out.
    expect(writes.filter(({ body }) => body.includes('"labels"'))).toEqual([]);

    const [signingOut] = tabs;
    if (signingOut === undefined) throw new Error("no tab was opened");
    await signingOut.bringToFront();
    if (testInfo.project.name === "iphone") {
      await signingOut.getByRole("button", { name: "Open navigation" }).click();
    }
    await signingOut.getByRole("button", { name: "Sign out" }).click();
    await expect(signingOut.getByRole("link", { name: "Sign in with Google" })).toBeVisible();
    expect(confirmedAt).toEqual(expect.any(Number));
    for (const page of tabs) {
      await expect(pendingWrites(page)).toHaveCount(0);
      await expect.poll(() => keptWrites(page)).toBe(0);
    }
    // A write a drop started would have gone out at once; give a straggler time to show.
    await signingOut.waitForTimeout(3_000);

    expect(writes.filter(({ at }) => at >= (confirmedAt ?? 0))).toEqual([]);
    const after = await Promise.all(issues.map((issue) => getIssue(issue.key, agent)));
    expect(after.map(({ labels, status }) => ({ labels, status }))).toEqual(
      before.map(({ labels, status }) => ({ labels, status }))
    );
  } finally {
    await alice.close();
  }
});

// A sign-out whose tab goes before it hears how the sign-out ended leaves the other tabs to ask
// Dispatch. Alice's status change in tab 2 is failing at the connection when tab 1 signs her out,
// so tab 2 holds it, `Waiting for a sign-out in another tab`. Tab 1's `POST /auth/logout` goes to
// a server that takes it and never answers, so the page never hears how it ended: a route left
// unanswered is not enough, since Chromium sends a paused request on to Dispatch once its page
// closes. Either nothing else ends alice's session (the sign-out did not land), or the test ends
// it at Dispatch itself (it landed). Then tab 1 closes, with the network back. Tab 2 reads who is
// signed in: still alice, and the change goes out and lands once; nobody, and it is discarded with
// nothing sent. A page where a closing tab said its sign-out was still out let the change go out
// under a session Dispatch had ended, and kept it for the next sign-in. The
// `chromium-plain-http-sign-out` project runs these on the lease a page without `navigator.locks`
// holds by.
for (const landed of [false, true]) {
  test(`a sign-out ${landed ? "that landed" : "that never reached Dispatch"}, its tab closed before the answer, ${landed ? "discards" : "lets go on"} the write another tab held for it`, async ({
    browser,
  }, testInfo) => {
    test.skip(
      testInfo.project.name.startsWith("chromium-plain-http") && httpsTarget,
      plainHttpSkipReason
    );
    test.setTimeout(90_000);
    await createProject({ key: "KW", name: "Keyed writes" });
    const issue = await createIssue({ project: "KW", title: "Held for a sign-out" });
    const before = await getIssue(issue.key);
    const path = `/api/v1/issues/${issue.key}`;
    const alice = testInfo.project.name.startsWith("chromium-plain-http")
      ? await asPlainHttpUser(browser, "alice")
      : await asUser(browser, "alice");
    // Where the page's sign-out goes in place of Dispatch: it takes the request and never answers.
    const held: Socket[] = [];
    const silent = createServer((socket) => held.push(socket));
    await new Promise<void>((resolve) => silent.listen(0, "127.0.0.1", resolve));
    const { port } = silent.address() as AddressInfo;
    try {
      const holding = await alice.newPage();
      const writes: number[] = [];
      holding.on("request", (request) => {
        if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
          writes.push(Date.now());
        }
      });
      const network = await outage(holding, `**${path}`, "PATCH");
      await holding.goto(`/issues/${issue.key}`);
      const signingOut = await alice.newPage();
      signingOut.on("dialog", (dialog) => void dialog.accept());
      const sent = Promise.withResolvers<void>();
      await signingOut.route("**/auth/logout", async (route) => {
        sent.resolve();
        await route.continue({ url: `http://127.0.0.1:${port}/auth/logout` });
      });
      await signingOut.goto("/");

      await holding.bringToFront();
      await holding.getByLabel("Status").selectOption("backlog");
      await expect(pendingWrites(holding).getByRole("status")).toHaveText("Saving 1 change…");
      await signingOut.bringToFront();
      if (testInfo.project.name === "iphone") {
        await signingOut.getByRole("button", { name: "Open navigation" }).click();
      }
      await signingOut.getByRole("button", { name: "Sign out" }).click();
      await sent.promise;
      // The hold is read before alice's session ends: Dispatch closes the holding tab's live
      // socket as soon as it does, and that tab then reads whoami, so its lapse would otherwise
      // overtake the hold and leave the sign-in page, with no indicator to read.
      await expect(pendingWrites(holding)).toContainText("Waiting for a sign-out in another tab");
      if (landed) {
        const ended = await fetch(new URL("/auth/logout", baseUrl), {
          headers: await userHeaders("alice"),
          method: "POST",
        });
        expect(ended.status).toBe(200);
      }
      network.end();
      const closed = Date.now();
      await signingOut.close();

      if (landed) {
        await expect(pendingWrites(holding)).toHaveCount(0, { timeout: 30_000 });
        await expect.poll(() => keptWrites(holding)).toBe(0);
        // A write the discard started would go out at once; give a straggler time to show.
        await holding.waitForTimeout(3_000);
        expect(writes.filter((at) => at >= closed)).toEqual([]);
        expect((await getIssue(issue.key, agent)).status).toBe(before.status);
        expect(await statusEvents(issue.key, "backlog")).toEqual([]);
      } else {
        await expect
          .poll(async () => (await getIssue(issue.key, agent)).status, { timeout: 30_000 })
          .toBe("backlog");
        await expect(pendingWrites(holding)).toHaveCount(0);
        await expect.poll(() => keptWrites(holding)).toBe(0);
        expect(await statusEvents(issue.key, "backlog")).toHaveLength(1);
      }
    } finally {
      await alice.close();
      for (const socket of held) socket.destroy();
      silent.close();
    }
  });
}

// A write belongs to the person who pressed it, and dropping a line of writes starts none of them.
// Alice's status change is held at the network, out and never answered, and a label she added
// waits behind it on the same issue; then bob signs in from another tab of the same browser. Both
// of alice's writes are dropped, and nothing goes out after bob's sign-in: neither the label nor
// another attempt of the held change. A page that advanced the line as it dropped it sent the
// label under bob's cookie, which only Dispatch's `409 LOGIN_CHANGED` stopped.
test("alice's write queued behind her held one sends nothing once bob signs in from another tab", async ({
  browser,
}) => {
  test.setTimeout(90_000);
  await createProject({ key: "KW", name: "Keyed writes" });
  const issue = await createIssue({ project: "KW", title: "Alice's line" });
  const before = await getIssue(issue.key);
  const path = `/api/v1/issues/${issue.key}`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const writes: { at: number; body: string }[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH" && new URL(request.url()).pathname === path) {
        writes.push({ at: Date.now(), body: request.postData() ?? "" });
      }
    });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("button", { name: "Edit labels" })).toBeVisible();
    await queueBehindAHeldStatus(page, issue.key, "alice-only");

    const other = await alice.newPage();
    const bobSignedIn = Date.now();
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
    await expect(pendingWrites(page)).toHaveCount(0);
    await expect.poll(() => keptWrites(page)).toBe(0);
    // A write the drop started would go out at once; give a straggler time to show.
    await page.waitForTimeout(3_000);

    expect(writes.filter(({ at }) => at >= bobSignedIn)).toEqual([]);
    const after = await getIssue(issue.key, agent);
    expect({ labels: after.labels, status: after.status }).toEqual({
      labels: before.labels,
      status: before.status,
    });
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
