import { expect, type Locator, type Page, test } from "@playwright/test";
import type { InboxRow } from "../web/src/api/types";

import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getInbox,
  listComments,
  listMyReplies,
  markThreadRead,
} from "./api";
import { inboxPane } from "./inbox-pane";
import { asSession, replyRowId, THREAD_SESSION } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * One thread order everywhere (INFRA-450): the reader picks **Oldest first** or **Newest first**
 * once, in the Inbox's **View** menu, and every surface that draws a thread - the Inbox's
 * reading pane and its cards, an ask's thread, a document block's thread, a document page's
 * margin thread and the Conversation tab - draws its replies, its fold and its reply box that
 * way.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

const ROOT = "How far back does a replay go?";
/** The six replies, oldest first; the newest two are the ones a collapsing surface keeps. */
const REPLIES = [1, 2, 3, 4, 5, 6].map((place) => `Reply ${place} about the replay budget.`);
const DECISION_BLOCK = "replay-window";
const DECISION_QUESTION = "Which window does the mirror replay?";
const decisionSpec = [
  "# Replay window",
  "",
  "The mirror replays the ledger on a cold start.",
  "",
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  DECISION_QUESTION,
  "",
  "- The last hour",
  "- The last day",
  ":::",
  "",
].join("\n");

test.beforeEach(async () => {
  await resetDatabase();
});

/** The Inbox row `match` names, once the server has indexed the block that carries it. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  await expect.poll(async () => (await getInbox()).some(match)).toBe(true);
  const row = (await getInbox()).find(match);
  if (row === undefined) throw new Error("the seeded question left the Inbox before it was read");
  return row;
}

interface Seeded {
  readonly askId: string;
  readonly askIssue: string;
  readonly blockAsk: string;
  readonly blockIssue: string;
  readonly comment: string;
  readonly commentIssue: string;
}

/**
 * Three threads of a root and six replies: a comment thread the reader wrote in, an answered
 * ask's thread, and the thread of an answered decision block in an issue's spec. Each comment
 * thread is read through its fourth reply, so replies 5 and 6 are the unread ones.
 */
async function seedThreads(replies: readonly string[] = REPLIES): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });

  const commentIssue = await createIssue({
    project: "CORE",
    spec: "# Replay budget\n\nThe mirror replays the ledger on a cold start.\n",
    title: "Replay budget",
  });
  // Anchored, so the issue's margin lists the thread beside the passage it is about.
  const comment = await createComment(commentIssue.key, {
    anchor: { artifact: "spec", quote: "replays the ledger" },
    body: ROOT,
  });
  for (const body of replies) {
    await createComment(commentIssue.key, { body, reply_to: comment.id }, asSession());
  }

  const askIssue = await createIssue({ project: "CORE", title: "Lease handover" });
  const ask = await createAsk(
    askIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the lease handover?" },
    asSession()
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });
  for (const body of replies) {
    await createComment(askIssue.key, { ask_id: ask.id, body }, asSession());
  }

  const blockIssue = await createIssue({
    project: "CORE",
    spec: decisionSpec,
    title: "Replay window",
  });
  const block = await indexedRow((row) => row.block_id === DECISION_BLOCK);
  await answerAsk(block.id, { expected_edited_at: null, selected: ["The last hour"] });
  for (const body of replies) {
    await createComment(blockIssue.key, { ask_id: block.id, body }, asSession());
  }

  return {
    askId: ask.id,
    askIssue: askIssue.key,
    blockAsk: block.id,
    blockIssue: blockIssue.key,
    comment: comment.id,
    commentIssue: commentIssue.key,
  };
}

/** Reads the comment thread through its `through`th reply, leaving the rest unread. */
async function readThrough(issueKey: string, root: string, through: number): Promise<void> {
  const replies = (await listComments(issueKey))
    .filter((comment) => comment.reply_to === root)
    .sort((left, right) => (left.created_at < right.created_at ? -1 : 1));
  await markThreadRead(root, replies[through - 1]?.created_at ?? "");
}

/**
 * Reads an ask's own thread through its `through`th reply, leaving the rest unread. The ask's
 * replies are the only comments on its issue, so they are read in the order they were written.
 * An unread reply is never folded (`thread-layout.ts`), so a surface that reads the reader's
 * unread set - a document block's card does - folds nothing until the thread has been read.
 */
async function readAskThrough(issueKey: string, askId: string, through: number): Promise<void> {
  const replies = (await listComments(issueKey)).sort((left, right) =>
    left.created_at < right.created_at ? -1 : 1
  );
  await markThreadRead(askId, replies[through - 1]?.created_at ?? "");
}

/** The names of `items`, in the order the page draws them down the screen. */
async function verticalOrder(items: Record<string, Locator>): Promise<string[]> {
  const placed: [string, number][] = [];
  for (const [name, locator] of Object.entries(items)) {
    const box = await locator.first().boundingBox();
    if (box === null) throw new Error(`${name} is not on the page`);
    placed.push([name, box.y]);
  }
  return placed.sort((left, right) => left[1] - right[1]).map(([name]) => name);
}

/** The parts of a collapsing thread, named: the root, the fold, the two newest replies, and the
 *  box the reader writes in. */
function collapsedParts(scope: Locator, folded: number, box: Locator): Record<string, Locator> {
  return {
    box,
    fold: scope.getByRole("button", {
      name: `Show ${folded} earlier ${folded === 1 ? "reply" : "replies"}`,
    }),
    root: scope.getByText(ROOT).first(),
    "reply-5": scope.getByText(REPLIES[4] ?? "").first(),
    "reply-6": scope.getByText(REPLIES[5] ?? "").first(),
  };
}

/** The same parts without the root, for a surface whose thread section holds the replies, the
 *  fold and the reply box while the question above it is the card's. */
function foldParts(scope: Locator, folded: number): Record<string, Locator> {
  return {
    box: scope.getByRole("textbox", { name: "Reply" }),
    fold: scope.getByRole("button", {
      name: `Show ${folded} earlier ${folded === 1 ? "reply" : "replies"}`,
    }),
    "reply-5": scope.getByText(REPLIES[4] ?? "").first(),
    "reply-6": scope.getByText(REPLIES[5] ?? "").first(),
  };
}

/**
 * Opens a thread card that is still collapsed; an open one is left alone. The card may carry
 * its own `aria-expanded`, so the open state is read on the replies it reveals.
 *
 * The control arrives with the card, and `count()` waits for nothing, so a card still being
 * drawn would be read as a card with nothing to open and never opened at all: the wait is for
 * the card to have settled into one state or the other.
 */
async function expand(card: Locator): Promise<void> {
  const replies = card.getByRole("list", { name: "Replies" });
  const collapsed = card.locator('button[aria-expanded="false"]').first();
  await expect(collapsed.or(replies).first()).toBeVisible();
  if ((await collapsed.count()) > 0) await collapsed.click();
  await expect(replies).toHaveCount(1);
}

/** Picks one of the two thread orders in the Inbox's **View** menu, which keeps it for this
 *  browser; every other surface reads the same choice. */
async function setThreadOrder(page: Page, value: "newest" | "oldest"): Promise<void> {
  if (!page.url().endsWith("/")) await page.goto("/");
  await page.locator("[data-inbox-view]").click();
  const menu = page.getByRole("menu", { name: "View" });
  await menu
    .getByRole("group", { name: "Thread order" })
    .locator(`[data-inbox-view-choice="${value}"]`)
    .click();
  await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(0);
}

/** Selects a thread row and hands back the pane's thread body. */
async function openInPane(page: Page, id: string): Promise<Locator> {
  const row = page.locator(`[data-inbox-row="${id}"]`);
  await expect(row).toBeVisible();
  await row.locator("time").first().click();
  await expect(row).toHaveAttribute("data-inbox-selected", "true");
  const body = inboxPane(page).locator("[data-inbox-thread-body]");
  await expect(body).toBeVisible();
  return body;
}

/**
 * Holds every read mark the page writes, so a thread drawn for a second keeps saying what it
 * has that the reader has not read, and the fold under it stays where the row put it. The marks
 * themselves are `inbox-read-state.e2e.ts`'s subject, and `inbox-thread-pane.e2e.ts` holds them
 * the same way.
 */
async function holdReadMarks(page: Page): Promise<void> {
  await page.route("**/api/v1/me/threads/*/read", (route) =>
    route.request().method() === "GET"
      ? route.fallback()
      : route.fulfill({ body: "{}", contentType: "application/json", status: 503 })
  );
}

test("the Inbox's pane and its card read a thread in the chosen order, and keep it over a reload", async ({
  browser,
}) => {
  const seeded = await seedThreads();
  await readThrough(seeded.commentIssue, seeded.comment, 4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const id = replyRowId(seeded.comment);

    // Oldest first: the root, the fold over the four read replies, 5, 6, then the reply box.
    await setThreadOrder(page, "oldest");
    let body = await openInPane(page, id);
    const box = body.getByRole("textbox", { name: "Reply" });
    expect(await verticalOrder(collapsedParts(body, 4, box))).toEqual([
      "root",
      "fold",
      "reply-5",
      "reply-6",
      "box",
    ]);
    // The pane opens where the reader left off: the first unread reply is on screen.
    expect(
      await inboxPane(page).evaluate((pane, text) => {
        let scroller = pane;
        for (const candidate of pane.querySelectorAll<HTMLElement>("*")) {
          const overflow = window.getComputedStyle(candidate).overflowY;
          if (overflow === "auto" || overflow === "scroll") {
            scroller = candidate;
            break;
          }
        }
        const fifth = [...pane.querySelectorAll<HTMLElement>("[data-inbox-thread-turn]")].find(
          (turn) => (turn.textContent ?? "").includes(text)
        );
        if (fifth === undefined) return false;
        const box = scroller.getBoundingClientRect();
        const rect = fifth.getBoundingClientRect();
        return rect.bottom > box.top && rect.top < box.bottom;
      }, REPLIES[4] ?? "")
    ).toBe(true);

    // Newest first: the root, the box, 6, 5, then the fold.
    await setThreadOrder(page, "newest");
    body = await openInPane(page, id);
    expect(
      await verticalOrder(collapsedParts(body, 4, body.getByRole("textbox", { name: "Reply" })))
    ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);

    // The choice is the reader's, kept for their browser across a reload.
    await page.reload();
    body = await openInPane(page, id);
    expect(
      await verticalOrder(collapsedParts(body, 4, body.getByRole("textbox", { name: "Reply" })))
    ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);

    // Below the pane's width the row is its own card, and it reads the same way.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/");
    const card = page.locator(`[data-inbox-row="${id}"] [data-inbox-thread-body]`);
    await expect(card).toBeVisible();
    expect(
      await verticalOrder(collapsedParts(card, 4, card.getByRole("textbox", { name: "Reply" })))
    ).toEqual(["root", "box", "reply-6", "reply-5", "fold"]);
  } finally {
    await alice.close();
  }
});

/** The two orders and the places a collapsing thread puts its fold and its reply box. */
const FOLD_ORDERS = [
  ["oldest", ["fold", "reply-5", "reply-6", "box"]],
  ["newest", ["box", "reply-6", "reply-5", "fold"]],
] as const;

test("an ask's thread folds and places its box by the chosen order", async ({ browser }) => {
  const seeded = await seedThreads();
  // The Inbox's row carries the thread's read mark, and the card folds by it: read through
  // reply 4, so the fold holds those four and replies 5 and 6 stay on screen.
  await readAskThrough(seeded.askIssue, seeded.askId, 4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    for (const [order, expected] of FOLD_ORDERS) {
      await page.goto("/");
      await setThreadOrder(page, order);
      // An answered ask's own thread, as the Inbox's reading pane draws its card.
      const row = page.locator(`[data-inbox-row="${replyRowId(seeded.askId)}"]`);
      await expect(row).toBeVisible();
      await row.locator("time").first().click();
      const thread = inboxPane(page).getByTestId(`thread-${seeded.askId}`);
      await expect(thread, order).toBeVisible();
      expect(await verticalOrder(foldParts(thread, 4)), order).toEqual(expected);
    }
  } finally {
    await alice.close();
  }
});

/**
 * An unread reply is never folded away, in an ask's thread as in every other one (spec v23,
 * A10). The Inbox's row carries the thread's read mark, and the pane passes the unread set it
 * names down to the ask's card, so with replies 1 and 2 read the four unread ones are all on
 * screen and the fold holds exactly those two. Folding by count alone put replies 3 and 4
 * behind **Show 4 earlier replies**, and the row's own mark then read them through reply 6 -
 * two writes the reader never saw, marked read.
 *
 * The read marks are held, so the row keeps saying what it has that the reader has not read
 * while the thread is drawn (`inbox-thread-pane.e2e.ts` holds them the same way).
 */
test("an ask's thread leaves every unread reply on screen and folds only the read ones", async ({
  browser,
}) => {
  const seeded = await seedThreads();
  await readAskThrough(seeded.askIssue, seeded.askId, 2);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await holdReadMarks(page);
    await setThreadOrder(page, "oldest");
    const row = page.locator(`[data-inbox-row="${replyRowId(seeded.askId)}"]`);
    await expect(row).toBeVisible();
    await row.locator("time").first().click();
    const thread = inboxPane(page).getByTestId(`thread-${seeded.askId}`);
    await expect(thread).toBeVisible();
    // The fold holds the two read replies and nothing else.
    await expect(thread.getByRole("button", { name: "Show 2 earlier replies" })).toBeVisible();
    for (const place of [3, 4, 5, 6]) {
      await expect(thread.getByText(REPLIES[place - 1] ?? ""), `reply ${place}`).toBeVisible();
    }
    for (const place of [1, 2]) {
      await expect(thread.getByText(REPLIES[place - 1] ?? ""), `reply ${place}`).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

test("a document block's thread folds and places its box by the chosen order", async ({
  browser,
}) => {
  // Two live documents, each opened with its own room and editor.
  test.slow();
  const seeded = await seedThreads();
  // The block's card marks the thread read as the reader looks at it, so it folds by the
  // reader's unread set rather than by count alone: read through reply 4, leaving 5 and 6.
  await readAskThrough(seeded.blockIssue, seeded.blockAsk, 4);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    for (const [order, expected] of FOLD_ORDERS) {
      await page.goto("/");
      await setThreadOrder(page, order);
      await page.goto(`/issues/${seeded.blockIssue}/spec`);
      // The block holds the thread's section and, beside it, the control that reveals the
      // reply box: a collapsed thread puts the box after its own Write a reply (`AskCard`'s
      // `thread="collapsed"`), so the block is what the order places both in.
      const block = page.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);
      const parts = foldParts(block, 4);
      await expect(block.getByTestId(`thread-${seeded.blockAsk}`), order).toBeVisible();
      await expect(parts.fold, order).toBeVisible();
      const writeReply = block.getByRole("button", { name: "Write a reply" });
      await expect(writeReply, order).toBeVisible();
      await writeReply.click();
      await expect(parts.box, order).toBeVisible();
      expect(await verticalOrder(parts), order).toEqual(expected);
    }
  } finally {
    await alice.close();
  }
});

test("a margin thread and the Conversation show every reply in the chosen order", async ({
  browser,
}) => {
  test.slow();
  const seeded = await seedThreads();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await setThreadOrder(page, "oldest");

    // A thread card is collapsed until the reader opens it, on the tab and in the margin alike,
    // and an opened one shows every reply: neither surface folds.
    await page.goto(`/issues/${seeded.commentIssue}/conversation`);
    const card = page.locator(`[data-turn="comment:${seeded.comment}"]`);
    await expect(card).toBeVisible();
    await expand(card);
    const replies = card.getByRole("list", { name: "Replies" }).getByRole("listitem");
    await expect(replies).toHaveCount(6);
    expect(await replies.allTextContents()).toEqual(
      REPLIES.map((reply) => expect.stringContaining(reply))
    );

    // The issue's margin draws the same thread, the same way.
    await page.goto(`/issues/${seeded.commentIssue}/spec?comment=${seeded.comment}`);
    const margin = page.locator(`[data-margin-item="${seeded.comment}"]`);
    await expect(margin).toBeVisible();
    await expand(margin);
    const marginReplies = margin.getByRole("list", { name: "Replies" }).getByRole("listitem");
    await expect(marginReplies).toHaveCount(6);
    expect(await marginReplies.allTextContents()).toEqual(
      REPLIES.map((reply) => expect.stringContaining(reply))
    );
    const marginBox = margin.getByRole("textbox", { name: "Reply" });
    expect(
      await verticalOrder({
        box: marginBox,
        last: marginReplies.last(),
        root: margin.getByText(ROOT).first(),
      })
    ).toEqual(["root", "last", "box"]);

    // Newest first turns both round and puts the box under the root.
    await setThreadOrder(page, "newest");
    await page.goto(`/issues/${seeded.commentIssue}/conversation`);
    await expand(card);
    await expect(replies).toHaveCount(6);
    expect(await replies.allTextContents()).toEqual(
      [...REPLIES].reverse().map((reply) => expect.stringContaining(reply))
    );

    await page.goto(`/issues/${seeded.commentIssue}/spec?comment=${seeded.comment}`);
    await expect(margin).toBeVisible();
    await expand(margin);
    await expect(marginReplies).toHaveCount(6);
    expect(await marginReplies.allTextContents()).toEqual(
      [...REPLIES].reverse().map((reply) => expect.stringContaining(reply))
    );
    expect(
      await verticalOrder({
        box: margin.getByRole("textbox", { name: "Reply" }),
        first: marginReplies.first(),
        root: margin.getByText(ROOT).first(),
      })
    ).toEqual(["root", "box", "first"]);
  } finally {
    await alice.close();
  }
});

test("an unread reply is never folded: with 3 to 6 unread the fold holds only 1 and 2", async ({
  browser,
}) => {
  const seeded = await seedThreads();
  await readThrough(seeded.commentIssue, seeded.comment, 2);
  expect(
    (await listMyReplies({ state: "unread" })).rows.find((row) => row.thread_id === seeded.comment)
      ?.unread_count
  ).toBe(4);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await setThreadOrder(page, "oldest");
    const body = await openInPane(page, replyRowId(seeded.comment));
    await expect(body.getByRole("button", { name: "Show 2 earlier replies" })).toBeVisible();
    await expect(body.locator("[data-inbox-thread-turn]")).toHaveCount(5);
    for (const reply of REPLIES.slice(2)) {
      await expect(body.getByText(reply)).toBeVisible();
    }
    await expect(body.getByText(REPLIES[1] ?? "")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
