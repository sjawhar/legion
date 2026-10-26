import {
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type Route,
  test,
} from "@playwright/test";

import { createAsk, createComment, createIssue, createMessage, createProject } from "./api";
import { type Box, boxesApart } from "./layout";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// The iPhone 13 viewport INFRA-95 measured the landing fault at. At the `phone` layout
// `phone-conversation.e2e.ts` uses (390x844) the composer's top sits near y603, below a newest
// turn left at y≈515 at the top of the page, so a landing row could not fail there.
const iphone = { height: 664, width: 390 };
const session = {
  actor: { kind: "session" as const, id: "e2e-phone-landing" },
  as: "agent" as const,
};

/** Seeds project CORE and an issue titled `title` whose Conversation holds one ask, one root
 *  comment, 8 older messages and then the newest message. The newest is created last, so it is
 *  newer than every ask and comment: a held or failing asks read cannot change which
 *  conversation is newest, only whether the list is on the page yet. */
async function seedConversation(title: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  await createAsk(issue.key, { question: "Which landing should the reader get?" }, session);
  await createComment(issue.key, { body: "A root comment under the ask" });
  for (let index = 0; index < 8; index += 1) {
    await createMessage(issue.key, { body: `Older message ${index}` }, session);
  }
  const newest = await createMessage(issue.key, { body: "The newest message lands here" }, session);
  return { issue, newest };
}

/** Matches the Conversation's asks read for `issueKey`: the asks endpoint with `thread=1`. */
function asksThreadRead(issueKey: string): (url: URL) => boolean {
  return (url) =>
    url.pathname === `/api/v1/issues/${issueKey}/asks` && url.searchParams.get("thread") === "1";
}

/** The newest message's turn in the Conversation's list. */
function newestTurn(page: Page, messageId: string): Locator {
  return page.locator(`ol[aria-label="Conversation turns"] [data-turn="message:${messageId}"]`);
}

/** A new page in `context` at the `iphone` viewport. */
async function openPhonePage(context: BrowserContext): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize(iphone);
  return page;
}

/** Waits until `card` has finished rendering: visible, then its box unchanged across 30
 *  consecutive samples 50 ms apart, all within 20 s. It only samples: nothing here scrolls,
 *  focuses, taps or hovers, so where the page put the card is where it is measured. */
async function settle(card: Locator, label: string): Promise<void> {
  const deadline = Date.now() + 20_000;
  await expect(card, `${label}: the newest card`).toBeVisible({ timeout: 20_000 });
  let last: Box | null = null;
  let unchanged = 0;
  while (unchanged < 30) {
    if (Date.now() > deadline) {
      throw new Error(
        `${label}: the newest card's box never held still for 30 samples within 20 s (last ${JSON.stringify(last)})`
      );
    }
    const tick = Promise.withResolvers<void>();
    setTimeout(tick.resolve, 50);
    await tick.promise;
    const box = await card.boundingBox();
    const same =
      box !== null &&
      last !== null &&
      box.x === last.x &&
      box.y === last.y &&
      box.width === last.width &&
      box.height === last.height;
    unchanged = same ? unchanged + 1 : 0;
    last = box;
  }
}

/** Where the page has the newest card now, measured once: whether it is wholly on screen, and
 *  whether it shares no point with the docked composer. */
async function landing(
  page: Page,
  card: Locator
): Promise<{ apart: boolean; inView: boolean; where: string }> {
  const box = await card.boundingBox();
  const composer = await page.getByRole("form", { name: "Comment composer" }).boundingBox();
  const view = await page.evaluate(() => ({
    height: window.innerHeight,
    scrollY: window.scrollY,
    width: window.innerWidth,
  }));
  if (box === null) throw new Error("expected the newest card to have a box");
  if (composer === null) throw new Error("expected the docked composer to have a box");
  const inView =
    box.x >= 0 &&
    box.y >= 0 &&
    box.x + box.width <= view.width &&
    box.y + box.height <= view.height;
  const apart = boxesApart(box, composer);
  const where = `the newest card at ${JSON.stringify(box)}, the composer at ${JSON.stringify(composer)}, the ${view.width}x${view.height} viewport at window.scrollY ${view.scrollY}`;
  return { apart, inView, where };
}

/** One-shot: the newest card is wholly on screen and clear of the docked composer. */
async function expectNewestInView(page: Page, card: Locator, label: string): Promise<void> {
  const { apart, inView, where } = await landing(page, card);
  expect(inView, `${label}: the newest card is not wholly in the viewport: ${where}`).toBe(true);
  expect(apart, `${label}: the newest card shares a point with the composer: ${where}`).toBe(true);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// INFRA-303: the Conversation's list is built from three reads, asks, comments and messages, and
// shows nothing until all three answer. The first placement on the newest turn must not be spent
// while one of them is still pending: a newest message known from the messages read before the
// asks read answers otherwise lands the reader at the top of the page, the newest message under
// the docked composer, and the asks answering later changes nothing that brings them back.
test("on a phone, the Conversation opens with its newest message in view whichever read answers last", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const { issue, newest } = await seedConversation("Landing on the newest message");
  const conversation = `/issues/${issue.key}/conversation`;
  const alice = await asUser(browser, "alice");

  try {
    // Forced: the messages and comments reads answer while the asks read is held, then it answers.
    const forced = await openPhonePage(alice);
    const heldAsks: Route[] = [];
    await forced.route(asksThreadRead(issue.key), (route) => {
      heldAsks.push(route);
    });
    const reads = ["messages", "comments"].map((read) =>
      forced.waitForEvent("requestfinished", {
        predicate: (request) =>
          new URL(request.url()).pathname === `/api/v1/issues/${issue.key}/${read}`,
      })
    );
    await forced.goto(conversation);
    await Promise.all(reads);
    await expect.poll(() => heldAsks.length).toBe(1);
    // No in-page signal says the page has seen the partial data, so this fixed margin - two
    // macrotasks, two frames and 300 ms - is the defended choice; the repeat-each red against
    // the unfixed page is its proof.
    await forced.evaluate(async () => {
      for (let i = 0; i < 2; i++) {
        const turn = Promise.withResolvers<void>();
        setTimeout(turn.resolve, 0);
        await turn.promise;
      }
      for (let i = 0; i < 2; i++) {
        const frame = Promise.withResolvers<void>();
        requestAnimationFrame(() => frame.resolve());
        await frame.promise;
      }
      const margin = Promise.withResolvers<void>();
      setTimeout(margin.resolve, 300);
      await margin.promise;
    });
    expect(heldAsks, "the tab made exactly one asks read while it was held").toHaveLength(1);
    await heldAsks[0]?.continue();
    await settle(newestTurn(forced, newest.id), "forced open");
    await expectNewestInView(forced, newestTurn(forced, newest.id), "forced open");
    await forced.close();

    // Unforced: each open a fresh page, the reads answering in whatever order they do.
    let page: Page | undefined;
    for (let open = 1; open <= 20; open += 1) {
      const label = `unforced open ${open}`;
      page = await openPhonePage(alice);
      // A new page has no history entry to restore a scroll position from: it starts at the top.
      expect(
        await page.evaluate(() => ({ history: history.length, scrollY: window.scrollY })),
        `${label}: the new page before it navigates`
      ).toEqual({ history: 1, scrollY: 0 });
      await page.goto(conversation);
      await settle(newestTurn(page, newest.id), label);
      await expectNewestInView(page, newestTurn(page, newest.id), label);
      if (open < 20) await page.close();
    }

    // The fixture still hides the newest message at the top of the page, which is where the
    // fault leaves the reader; otherwise every assertion above would pass whether or not it did.
    if (page === undefined) throw new Error("expected the last unforced open's page");
    await page.evaluate(() => window.scrollTo(0, 0));
    await settle(newestTurn(page, newest.id), "at the top of the page");
    const top = await landing(page, newestTurn(page, newest.id));
    expect(
      top.inView && top.apart,
      `the fixture no longer hides the newest message at the top of the page, so this row cannot catch the landing fault: ${top.where}`
    ).toBe(false);
  } finally {
    await alice.close();
  }
});

// INFRA-303's second path: a read that fails shows "Could not load the conversation." in the
// list's place, and the first placement must not be spent on that text either. When a later
// refetch brings the list without changing which conversation is newest, nothing else would
// bring the reader to it. The refetch here is the event stream's, not TanStack Query's own
// refetch on focus (its focusManager listens on `window`, and the `visibilitychange` this row
// dispatches on `document` does not bubble there): `useEventStream` in `web/src/api/sse.ts`
// listens for `visibilitychange` on `document`, `handleVisibilityChange` calls `forceReconnect`,
// and the reopened stream's `onOpen` calls `refreshEverything()`, whose flush invalidates every
// query, the errored asks read among them. The row depends on that path: if `visibilitychange`
// stops reopening the stream, or the whole-cache refresh throttle (`WHOLE_CACHE_REFRESH_MS`)
// stops letting a connection's first refresh through at once, this row stops seeing a refetch.
test("on a phone, a Conversation whose read failed opens on its newest message once a refetch brings the list", async ({
  browser,
}) => {
  const { issue, newest } = await seedConversation("Landing after a failed read");
  const alice = await asUser(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = asksThreadRead(issue.key);
    let failing = true;
    let failed = 0;
    await page.route(asksRead, async (route) => {
      if (!failing) {
        await route.continue();
        return;
      }
      failed += 1;
      await route.fulfill({
        body: JSON.stringify({ code: "SERVER_ERROR", error: "boom" }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    // Shown once the app's own retries of the asks read have failed too.
    await expect(page.getByText("Could not load the conversation.")).toBeVisible({
      timeout: 15_000,
    });
    expect(failed, "the asks read and each of its retries failed").toBeGreaterThan(0);

    failing = false;
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    await answered;
    const newestCard = newestTurn(page, newest.id);
    await settle(newestCard, "after the refetch");
    await expectNewestInView(page, newestCard, "after the refetch");
  } finally {
    await alice.close();
  }
});

// INFRA-303: a reader who leaves the Conversation while it shows only "Loading conversation…"
// was never in the list, so coming back is a first open, not a return to a place: they land on
// the newest conversation. Returning to a tab they had scrolled still restores that scroll;
// `doc.e2e.ts` 'tab round-trips keep one document connection, the typed text, and the
// Conversation scroll position' holds that.
test("on a phone, a reader who leaves the Conversation while it loads lands on its newest message when they come back", async ({
  browser,
}) => {
  const { issue, newest } = await seedConversation("Leaving while it loads");
  const alice = await asUser(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = asksThreadRead(issue.key);
    const held: Route[] = [];
    await page.route(asksRead, (route) => {
      held.push(route);
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Loading conversation…")).toBeVisible();
    await expect.poll(() => held.length).toBe(1);

    await page.getByRole("tab", { name: "Activity" }).click();
    await expect(page.getByRole("tab", { name: "Activity" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    await held[0]?.continue();
    await answered;
    const newestCard = newestTurn(page, newest.id);
    // The list renders in the hidden panel before the reader comes back.
    await expect(newestCard).toBeAttached();

    await page.getByRole("tab", { name: "Conversation" }).click();
    await settle(newestCard, "back on the Conversation");
    await expectNewestInView(page, newestCard, "back on the Conversation");
  } finally {
    await alice.close();
  }
});
