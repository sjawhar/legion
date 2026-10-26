import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Event } from "../web/src/api/types";
import { createAsk, createIssue, createProject, getAsk, getIssue, getIssueEvents } from "./api";
import { dropFirstAnswer, replayed, watchWrites } from "./keyed-writes";
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
    const answers: { status: number; code?: string; retryAfter?: string; replayed?: string }[] = [];
    page.on("response", async (response) => {
      if (response.request().method() !== "POST" || new URL(response.url()).pathname !== path) {
        return;
      }
      const body: unknown = await response.json().catch(() => null);
      answers.push({
        code:
          body !== null && typeof body === "object" && "code" in body
            ? String(body.code)
            : undefined,
        replayed: response.headers()["idempotency-replayed"],
        retryAfter: response.headers()["retry-after"],
        status: response.status(),
      });
    });
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
