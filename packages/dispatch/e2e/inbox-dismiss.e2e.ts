import { INBOX_DISMISSED_PAGE, type InboxDismissSeen, inboxThreadRow } from "@legion/contracts";
import { type BrowserContext, expect, type Page, type Request, test } from "@playwright/test";
import { GROUP_PAGE } from "../web/src/features/inbox/InboxGroups";

import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  listMyReplies,
  patchIssue,
} from "./api";
import { typeFilterCount } from "./inbox-filter";
import { inboxList, inboxPane, READING_PANE_WIDTH, selectedRow, selectRow } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  expectSidebarInboxCount,
  holdReadMarks,
  replyRowId,
  seedAgentThread,
  seedBulkReplyThreads,
  seedCommentThread,
  THREAD_SESSION,
  unreadThreadRows,
} from "./inbox-threads";
import { resetDatabase } from "./seed";
import { refusePosts, WRITE_REFUSED } from "./sends";
import { asUser } from "./users";

/**
 * The Inbox's **Dismiss** (INFRA-450, A12): a thread stays in the Inbox, read or not, until the
 * reader dismisses it, as an email stays in its inbox until it is archived. Reading a thread only
 * changes how its row is drawn (`e2e/inbox-read-state.e2e.ts`), and so does **Mark all read**
 * (`e2e/inbox-mark-all.e2e.ts`); **Dismiss** is what takes a row out, by its own control, by the
 * key `e`, and over a selection from the bulk bar. The **Dismissed** view lists what was taken
 * out and **Move to Inbox** puts it back; a reply somebody else writes brings a dismissed thread
 * back unread, and the reader's own reply leaves it where it is.
 *
 * A press that names its rows carries what the page drew in each of them (`seen_replies`) - a
 * Reply row's own version, an Agent row's newest session reply and the ids it drew as unread -
 * so the server writes no mark over a write the page never drew; a **Move to Inbox** carries
 * none, which is what the route takes a restore as.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The route one press of **Dismiss** or of **Move to Inbox** posts to. */
const DISMISS_PATH = "/api/v1/me/inbox/dismiss";
const DISMISS_ROUTE = `**${DISMISS_PATH}`;

/** The root of the comment thread this file dismisses, as its row and its control name it. */
const REPLY_SUBJECT = "How far back does a replay go?";

interface Seeded {
  /** The Agent row's root message, a one-off message to the session with a reply under it. */
  readonly agent: string;
  /** An open ask waiting on the reader, the one row of the three that has no **Dismiss**. */
  readonly ask: string;
  readonly issueKey: string;
  /** The comment thread's root, which is its Reply row. */
  readonly reply: string;
}

/**
 * Three rows: a Reply thread and an open ask on two P2 issues, and an Agent thread, which carries
 * no issue and so stands in the band for no priority. Both threads are unread, so what a dismiss
 * takes off the reader's Inbox count is visible.
 */
async function seedInbox(): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const asked = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(asked.key, { priority: 2 });
  const ask = await createAsk(asked.key, { question: "Ship the lease handover?" }, asSession());
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  await patchIssue(issue.key, { priority: 2 });
  const reply = await seedCommentThread(issue.key, REPLY_SUBJECT, "One hour, then it gives up.");
  const agent = await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });
  expect(await unreadThreadRows()).toBe(1);
  return { agent, ask: ask.id, issueKey: issue.key, reply };
}

/**
 * A page at the width that draws the reading column, with every read mark held. The column is
 * where this file works: the selected row always draws its **Dismiss**, the column's head draws
 * one too, and the selection a dismiss moves on is the column's. The marks are held so a thread
 * the column draws stays unread, which is what makes the Inbox count's drop this file's doing
 * rather than the column's.
 */
async function inboxPage(context: BrowserContext, address = "/"): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
  await holdReadMarks(page);
  await page.goto(address);
  return page;
}

/** Every band heading the list is drawing, top to bottom, with its count. */
async function bandHeadings(page: Page): Promise<string[]> {
  return inboxList(page)
    .locator("h2 button")
    .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim()));
}

/** Every `POST` to the dismiss route this page has made, in order, so a press that must be one
 *  request can be held to it. */
function watchDismisses(page: Page): Request[] {
  const posted: Request[] = [];
  page.on("request", (request) => {
    if (request.method() === "POST" && new URL(request.url()).pathname === DISMISS_PATH) {
      posted.push(request);
    }
  });
  return posted;
}

/** The next `POST` to the dismiss route, as a promise to await before pressing. */
function nextDismiss(page: Page): Promise<Request> {
  return page.waitForRequest(
    (request) => request.method() === "POST" && new URL(request.url()).pathname === DISMISS_PATH
  );
}

/** What the reader has dismissed out of their Inbox, read back from the route a page at a time.
 *  Under `state=dismissed` the answer's `total` is the page it holds rather than the whole pile -
 *  nothing counts that view, and counting it is the most expensive read the Inbox has (a14) - so
 *  the count is the rows, followed through `next_cursor` to the last page. */
async function dismissedThreads(): Promise<number> {
  let counted = 0;
  let cursor: string | undefined;
  do {
    const page = await listMyReplies({ cursor, kind: ["reply", "agent"], state: "dismissed" });
    counted += (page.rows ?? []).length;
    cursor =
      page.next_cursor === undefined || page.next_cursor === "" ? undefined : page.next_cursor;
  } while (cursor !== undefined);
  return counted;
}

/** What the reader's Inbox holds, read or not, read back from the route. */
async function inboxThreads(): Promise<number> {
  return (await listMyReplies({ kind: ["reply", "agent"], state: "inbox" })).total;
}

/** What the page drew in each Inbox row, read out of the same list route the page reads and
 *  keyed by that row's own id, which is exactly what a dismissal of it carries in
 *  `seen_replies`: a Reply row's own `version`, and an Agent row's newest session reply with the
 *  ids it answered as unread. Read before the press, since the rows it takes out are gone from
 *  this state afterwards. */
async function seenReplies(): Promise<Record<string, InboxDismissSeen>> {
  const { rows } = await listMyReplies({ kind: ["reply", "agent"], state: "inbox" });
  return Object.fromEntries(
    (rows ?? [])
      .map(inboxThreadRow)
      .map((row) => [
        row.kind === "agent" ? agentRowId(row.thread_id) : replyRowId(row.thread_id),
        row.kind === "agent"
          ? { newest_reply: row.reply.id, unread_replies: row.unread_reply_ids }
          : { version: row.version },
      ])
  );
}

/** Picks one choice of the **View** menu's **Show** section and shuts the menu again. */
async function chooseShow(page: Page, label: string): Promise<void> {
  const menu = page.getByRole("menu", { name: "View" });
  if ((await menu.count()) === 0) await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(1);
  await menu.getByRole("group", { name: "Show" }).getByRole("radio", { name: label }).click();
  if ((await menu.count()) !== 0) await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(0);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// ---------------------------------------------------------------------------
// A thread stays until it is dismissed.
// ---------------------------------------------------------------------------

test("a Reply thread and an Agent thread read in the pane stay in the list after a reload", async ({
  browser,
}) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    // This row is the one place in the file where the read marks are the page's own: reading is
    // what it is about.
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: READING_PANE_WIDTH });
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    const replyRow = page.locator(`[data-inbox-row="${replyRowId(seeded.reply)}"]`);
    const agentRow = page.locator(`[data-inbox-row="${agentRowId(seeded.agent)}"]`);
    await expect(rows).toHaveCount(3);
    await expectSidebarInboxCount(page, 3);

    // Each thread read in the column, which marks it read where it stands.
    await selectRow(page, replyRowId(seeded.reply));
    await expect(
      inboxPane(page).getByText("One hour, then it gives up.", { exact: true })
    ).toBeVisible();
    await expect(replyRow.locator("[aria-label='Unread']")).toHaveCount(0);
    await selectRow(page, agentRowId(seeded.agent));
    await expect(inboxPane(page).getByText("Green.", { exact: true })).toBeVisible();
    await expect(agentRow.locator("[aria-label='Unread']")).toHaveCount(0);
    await expect.poll(async () => unreadThreadRows()).toBe(0);
    // The open ask is all that is left waiting on the reader.
    await expectSidebarInboxCount(page, 1);

    // A reload reads the same three rows back from the server, both threads drawn as read: a
    // thread leaves the Inbox only when it is dismissed, and nothing here dismissed one.
    await page.reload();
    await expect(rows).toHaveCount(3);
    await expect(replyRow).toBeVisible();
    await expect(agentRow).toBeVisible();
    await expect(page.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(0);
    expect(await inboxThreads()).toBe(2);
    expect(await dismissedThreads()).toBe(0);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// Dismiss, by the row's own control and by `e`.
// ---------------------------------------------------------------------------

test("the row's Dismiss takes a thread out of the list, the counts and the Inbox number, and the selection moves on", async ({
  browser,
}) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const posted = watchDismisses(page);
    const bar = page.locator("[data-inbox-bar]");
    const rows = page.locator("[data-inbox-row]");
    const replyRow = page.locator(`[data-inbox-row="${replyRowId(seeded.reply)}"]`);
    await expect(rows).toHaveCount(3);
    await expectSidebarInboxCount(page, 3);
    expect(await bandHeadings(page)).toEqual(["P2 (2)", "No priority (1)"]);
    expect(await typeFilterCount(page, bar, "Reply")).toBe(1);
    expect(await typeFilterCount(page, bar, "Agent message")).toBe(1);

    // The selected row draws its Dismiss twice - on the short row and in the column's head -
    // and both name the thread.
    await selectRow(page, replyRowId(seeded.reply));
    const control = page.locator(`[data-inbox-dismiss="${replyRowId(seeded.reply)}"]`);
    await expect(control).toHaveCount(2);
    await expect(control.first()).toHaveAttribute("aria-label", `Dismiss: ${REPLY_SUBJECT}`);
    await expect(control.first()).toHaveText("Dismiss");

    // One press: the row goes, the selection moves to the next row, and the unread thread it
    // took goes off the reader's Inbox number. The body names the row and the version the list
    // answered for it, which is the version the server writes the mark under.
    const seen = await seenReplies();
    const sent = nextDismiss(page);
    await control.first().click();
    expect((await sent).postDataJSON()).toEqual({
      seen_replies: { [replyRowId(seeded.reply)]: seen[replyRowId(seeded.reply)] },
      threads: [replyRowId(seeded.reply)],
    });
    await expect(replyRow).toHaveCount(0);
    await expect(rows).toHaveCount(2);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seeded.ask);
    await expectSidebarInboxCount(page, 2);

    // And out of the Filter control's count and the band's.
    expect(await bandHeadings(page)).toEqual(["P2 (1)", "No priority (1)"]);
    expect(await typeFilterCount(page, bar, "Reply")).toBe(0);
    expect(await typeFilterCount(page, bar, "Agent message")).toBe(1);

    // The server holds the same: one thread dismissed, one left in the Inbox.
    await expect.poll(async () => dismissedThreads()).toBe(1);
    expect(await inboxThreads()).toBe(1);
    expect(posted).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("e dismisses the selected thread row, and does nothing on an Ask row, which draws no Dismiss", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  // An open ask on somebody else's issue that the reader has written in, with the session's
  // answer after her: the question waits on its holder rather than on her, so her own write in
  // its thread is a Reply row of hers like any other, and the thread's id is the ask's.
  const theirs = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(theirs.key, { assignee: "bob", priority: 2 });
  const theirAsk = await createAsk(
    theirs.key,
    { question: "Which lease is still open?" },
    asSession()
  );
  await createComment(theirs.key, { ask_id: theirAsk.id, body: "The mirror's, I think." });
  await createComment(
    theirs.key,
    { ask_id: theirAsk.id, body: "The mirror's, until tonight." },
    asSession()
  );
  // And an open ask of her own, which the Inbox draws as an Ask row: the one row of the two
  // with no Dismiss at all.
  const mine = await createIssue({ project: "CORE", title: "Replay budget" });
  await patchIssue(mine.key, { priority: 2 });
  const ask = await createAsk(mine.key, { question: "Ship the replay budget?" }, asSession());

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const rows = page.locator("[data-inbox-row]");
    const askRow = page.locator(`[data-inbox-row="${ask.id}"]`);
    const replyRow = page.locator(`[data-inbox-row="${replyRowId(theirAsk.id)}"]`);
    await expect(askRow).toBeVisible();
    await expect(replyRow).toBeVisible();
    await expect(rows).toHaveCount(2);

    // The Ask row carries no Dismiss of its own, and `e` on it takes nothing out.
    await selectRow(page, ask.id);
    await expect(page.locator(`[data-inbox-dismiss="${ask.id}"]`)).toHaveCount(0);
    await page.keyboard.press("e");
    await expect(rows).toHaveCount(2);
    expect(await dismissedThreads()).toBe(0);

    // The Reply row of the open ask's thread dismisses like any Reply row.
    await selectRow(page, replyRowId(theirAsk.id));
    await expect(page.locator(`[data-inbox-dismiss="${replyRowId(theirAsk.id)}"]`)).toHaveCount(2);
    await page.keyboard.press("e");
    await expect(replyRow).toHaveCount(0);
    await expect(rows).toHaveCount(1);
    await expect.poll(async () => dismissedThreads()).toBe(1);
    // The question itself is untouched: it is answered or snoozed, never dismissed.
    await expect(askRow).toBeVisible();
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// The bulk bar: a picked selection, and Select all.
// ---------------------------------------------------------------------------

test("three thread rows picked with x go out together in one bulk Dismiss", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  await patchIssue(issue.key, { priority: 2 });
  const threads: string[] = [];
  for (const question of [
    "How far back does a replay go?",
    "What stops a replay early?",
    "Who widens the replay window?",
    "Where is the replay log kept?",
  ]) {
    threads.push(await seedCommentThread(issue.key, question, `About: ${question}`));
  }
  expect(await unreadThreadRows()).toBe(4);

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const posted = watchDismisses(page);
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(4);

    // The reader walks the list and marks the first three rows, the way the keys leave them:
    // `x` marks the row the column is showing, `j` moves on to the next.
    const drawn = await rows.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );
    const picked = drawn.slice(0, 3);
    await selectRow(page, picked[0] ?? "");
    await selectedRow(page).focus();
    for (let index = 0; index < picked.length; index += 1) {
      if (index > 0) await page.keyboard.press("j");
      await page.keyboard.press("x");
    }

    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("3 selected");
    const dismiss = bar.getByRole("button", { name: "Dismiss selected threads" });
    await expect(dismiss).toBeVisible();

    // One press names all three rows, each with what the list drew in it, and all three leave
    // the list.
    const seen = await seenReplies();
    const sent = nextDismiss(page);
    await dismiss.click();
    const body = (await sent).postDataJSON() as {
      seen_replies?: Record<string, InboxDismissSeen>;
      threads?: string[];
    };
    expect(new Set(body.threads)).toEqual(new Set(picked));
    expect(body.seen_replies).toEqual(Object.fromEntries(picked.map((id) => [id, seen[id]])));
    for (const id of picked) {
      await expect(page.locator(`[data-inbox-row="${id}"]`)).toHaveCount(0);
    }
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toHaveAttribute("data-inbox-row", drawn[3] ?? "");
    await expect.poll(async () => dismissedThreads()).toBe(3);
    expect(await inboxThreads()).toBe(1);
    expect(posted).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("Select all dismisses every row of the group in one request that names each of them, and the Dismissed view pages them", async ({
  browser,
}) => {
  // More rows than one band draws at once and more than one dismissed page holds, so the press
  // covers rows no page has drawn and the Dismissed view has a second page to read.
  const THREADS = INBOX_DISMISSED_PAGE + 50;
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Mirror retries" });
  await patchIssue(issue.key, { priority: 2 });
  await seedBulkReplyThreads({ count: THREADS, issueKey: issue.key, prefix: "Mirror" });
  expect(await unreadThreadRows()).toBe(THREADS);

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const posted = watchDismisses(page);
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(GROUP_PAGE);
    expect(await bandHeadings(page)).toEqual([`P2 (${THREADS})`]);

    // One row ticked, then **Select all**: the band holds every row, drawn or not.
    const drawn = await rows.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );
    const checkbox = rows.first().getByRole("checkbox");
    await expect(checkbox).toHaveAttribute("aria-label", /^Select Mirror root \d+$/);
    await checkbox.check();
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");
    await bar.getByRole("button", { name: "Select all" }).click();
    await expect(bar).toContainText("Every row selected");

    // One request, and it names every row of the group by id and version - the Inbox's own read
    // is unpaged, so all of them are in hand, the ones behind **Show more** as much as the fifty
    // drawn - rather than the narrowing, which only the paged Dismissed view has to fall back on.
    const seen = await seenReplies();
    const sent = nextDismiss(page);
    await bar.getByRole("button", { name: "Dismiss selected threads" }).click();
    const body = (await sent).postDataJSON() as {
      dismiss_through?: string;
      kind?: string[];
      seen_replies?: Record<string, InboxDismissSeen>;
      threads?: string[];
    };
    expect(Object.keys(body).sort()).toEqual(["seen_replies", "threads"]);
    const named = new Set(body.threads);
    expect(named.size).toBe(THREADS);
    expect([...named].every((id) => id.startsWith("reply:"))).toBe(true);
    for (const id of drawn) expect(named.has(id)).toBe(true);
    // One entry per row it names, each what the list answered for that row.
    expect(body.seen_replies).toEqual(seen);

    // Every row of the band goes, the ones behind **Show more** with them.
    await expect(rows).toHaveCount(0);
    await expect.poll(async () => dismissedThreads()).toBe(THREADS);
    expect(await inboxThreads()).toBe(0);
    expect(posted).toHaveLength(1);

    // The Dismissed view reads them a page at a time, and **Show more** reads the next page.
    await chooseShow(page, "Dismissed");
    await expect(page).toHaveURL(/show=dismissed/);
    await expect(rows).toHaveCount(INBOX_DISMISSED_PAGE);
    const more = page.locator('[data-inbox-show-more="dismissed"]');
    await expect(more).toHaveText(`Show ${INBOX_DISMISSED_PAGE} more`);
    await more.click();
    await expect(rows).toHaveCount(THREADS);
    await expect(more).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("Select all under a search dismisses every matching row of a band wider than one page", async ({
  browser,
}) => {
  // A band wider than the fifty rows a page draws, and a word that matches every one of them:
  // the press has to cover the rows below the page as well as the ones on it.
  const MATCHING = GROUP_PAGE + 70;
  const OTHERS = 3;
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rollout" });
  await patchIssue(issue.key, { priority: 2 });
  await seedBulkReplyThreads({ count: MATCHING, issueKey: issue.key, prefix: "Mirror" });
  await seedBulkReplyThreads({
    count: OTHERS,
    issueKey: issue.key,
    olderThan: 2 * MATCHING + 10,
    prefix: "Backup",
  });
  expect(await unreadThreadRows()).toBe(MATCHING + OTHERS);

  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const posted = watchDismisses(page);
    const rows = page.locator("[data-inbox-row]");
    await expect(rows).toHaveCount(GROUP_PAGE);

    // The word narrows the band to the rows it matches, of which the page still draws fifty.
    await page.getByRole("searchbox", { name: "Search asks" }).fill("mirror");
    await expect(page).toHaveURL(/q=mirror/);
    expect(await bandHeadings(page)).toEqual([`P2 (${MATCHING})`]);
    await expect(rows).toHaveCount(GROUP_PAGE);

    const checkbox = rows.first().getByRole("checkbox");
    await checkbox.check();
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");
    await bar.getByRole("button", { name: "Select all" }).click();
    await expect(bar).toContainText("Every row selected");

    // Every row the word matches goes, not the fifty on the page: the server's narrowing knows
    // nothing of that word, so the press names the ids - and only the matching ones, which is
    // why the three the word hides are still in the Inbox afterwards - each with its version.
    const seen = await seenReplies();
    const sent = nextDismiss(page);
    await bar.getByRole("button", { name: "Dismiss selected threads" }).click();
    const body = (await sent).postDataJSON() as {
      dismiss_through?: string;
      seen_replies?: Record<string, InboxDismissSeen>;
      threads?: string[];
    };
    expect(Object.keys(body).sort()).toEqual(["seen_replies", "threads"]);
    expect(new Set(body.threads).size).toBe(MATCHING);
    // One entry per row it named, and each is the version the list answered for that row.
    expect(new Set(Object.keys(body.seen_replies ?? {}))).toEqual(new Set(body.threads));
    for (const [id, drawn] of Object.entries(body.seen_replies ?? {})) {
      expect(drawn).toEqual(seen[id]);
    }

    await expect(rows).toHaveCount(0);
    await expect.poll(async () => dismissedThreads()).toBe(MATCHING);
    expect(await inboxThreads()).toBe(OTHERS);
    expect(posted).toHaveLength(1);

    // What the word hid is untouched, and the Inbox draws it again once the box is empty.
    await page.getByRole("searchbox", { name: "Search asks" }).fill("");
    await expect(rows).toHaveCount(OTHERS);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// The Dismissed view.
// ---------------------------------------------------------------------------

test("Show: Dismissed lists the dismissed threads, survives a reload, and Move to Inbox brings each back", async ({
  browser,
}) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const rows = page.locator("[data-inbox-row]");
    const replyId = replyRowId(seeded.reply);
    const agentId = agentRowId(seeded.agent);
    await expect(rows).toHaveCount(3);

    for (const id of [replyId, agentId]) {
      await page.locator(`[data-inbox-dismiss="${id}"]`).first().click();
      await expect(page.locator(`[data-inbox-row="${id}"]`)).toHaveCount(0);
    }
    await expect(rows).toHaveCount(1);
    await expect.poll(async () => dismissedThreads()).toBe(2);

    // The View menu's **Show** switch, on **Dismissed**: both threads, newest dismissal first,
    // in one list with no bands and no **Mark all read** - there is nothing here left to mark.
    await chooseShow(page, "Dismissed");
    await expect(page).toHaveURL(/show=dismissed/);
    await expect(rows).toHaveCount(2);
    await expect(page.locator(`[data-inbox-row="${replyId}"]`)).toBeVisible();
    await expect(page.locator(`[data-inbox-row="${agentId}"]`)).toBeVisible();
    expect(await bandHeadings(page)).toEqual([]);
    await expect(page.locator("[data-inbox-mark-all]")).toHaveCount(0);

    // The view is in the address, so a reload opens it again.
    await page.reload();
    await expect(rows).toHaveCount(2);
    await expect(page.locator(`[data-inbox-row="${replyId}"]`)).toBeVisible();

    // Here the same control reads **Move to Inbox**, and one press puts the thread back.
    await selectRow(page, replyId);
    const back = page.locator(`[data-inbox-dismiss="${replyId}"]`).first();
    await expect(back).toHaveText("Move to Inbox");
    await expect(back).toHaveAttribute("aria-label", `Move to Inbox: ${REPLY_SUBJECT}`);
    await back.click();
    await expect(page.locator(`[data-inbox-row="${replyId}"]`)).toHaveCount(0);
    await page.locator(`[data-inbox-dismiss="${agentId}"]`).first().click();
    await expect(rows).toHaveCount(0);
    await expect.poll(async () => dismissedThreads()).toBe(0);

    // Both are rows of the Inbox again, drawn as read: a dismiss marked them read, and moving
    // one back does not make it unread.
    await chooseShow(page, "Inbox");
    await expect(page).not.toHaveURL(/show=dismissed/);
    await expect(rows).toHaveCount(3);
    await expect(page.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(0);
    expect(await inboxThreads()).toBe(2);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// What brings a dismissed thread back, and what does not.
// ---------------------------------------------------------------------------

test("a reply somebody else writes brings a dismissed thread back into the Inbox, unread", async ({
  browser,
}) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const rows = page.locator("[data-inbox-row]");
    const replyId = replyRowId(seeded.reply);
    const replyRow = page.locator(`[data-inbox-row="${replyId}"]`);
    await expect(rows).toHaveCount(3);

    await page.locator(`[data-inbox-dismiss="${replyId}"]`).first().click();
    await expect(replyRow).toHaveCount(0);
    await expect.poll(async () => dismissedThreads()).toBe(1);
    // The dismiss marked it read, so it counts nowhere: the open ask and the unread Agent
    // thread are all that is left.
    await expectSidebarInboxCount(page, 2);

    // The session writes in the thread again. The row comes back where it stood, unread, with
    // no reload: the page is told by the live stream, as it is for any write it is watching.
    await createComment(
      seeded.issueKey,
      { body: "Make that two hours, from tonight.", reply_to: seeded.reply },
      asSession()
    );
    await expect(replyRow).toBeVisible();
    await expect(replyRow.locator("[aria-label='Unread']")).toHaveCount(1);
    await expect(rows).toHaveCount(3);
    await expectSidebarInboxCount(page, 3);
    await expect.poll(async () => dismissedThreads()).toBe(0);
  } finally {
    await alice.close();
  }
});

test("a reply the reader sends from the Dismissed view leaves the thread there", async ({
  browser,
}) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const rows = page.locator("[data-inbox-row]");
    const replyId = replyRowId(seeded.reply);
    await expect(rows).toHaveCount(3);
    await page.locator(`[data-inbox-dismiss="${replyId}"]`).first().click();
    await expect(page.locator(`[data-inbox-row="${replyId}"]`)).toHaveCount(0);
    await expect.poll(async () => dismissedThreads()).toBe(1);

    // From the Dismissed view the reader answers the thread in the column's own reply box.
    await chooseShow(page, "Dismissed");
    await expect(rows).toHaveCount(1);
    await selectRow(page, replyId);
    const pane = inboxPane(page);
    await expect(pane.getByText("One hour, then it gives up.", { exact: true })).toBeVisible();
    const box = pane.getByRole("textbox", { name: "Reply" });
    await box.fill("Then we widen the window.");
    await expect(box).toHaveValue("Then we widen the window.");
    await pane.getByRole("button", { name: "Send" }).click();
    await expect(pane.getByText("Then we widen the window.", { exact: true })).toBeVisible();

    // Their own write is no reason to put it back: the thread stays dismissed, and the Inbox
    // goes on holding the two rows it held.
    await expect(rows).toHaveCount(1);
    await expect(page.locator(`[data-inbox-row="${replyId}"]`)).toBeVisible();
    await expect.poll(async () => dismissedThreads()).toBe(1);
    expect(await inboxThreads()).toBe(1);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A dismiss the server refuses.
// ---------------------------------------------------------------------------

test("a refused dismiss puts the row back where it stood and says so", async ({ browser }) => {
  const seeded = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await inboxPage(alice);
    const rows = page.locator("[data-inbox-row]");
    const replyId = replyRowId(seeded.reply);
    const replyRow = page.locator(`[data-inbox-row="${replyId}"]`);
    await expect(rows).toHaveCount(3);
    const before = await rows.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );

    const refuse = await refusePosts(page, DISMISS_ROUTE);
    await selectRow(page, replyId);
    await page.locator(`[data-inbox-dismiss="${replyId}"]`).first().click();
    await expect(replyRow).toHaveCount(0);
    refuse();

    // The row is back where it stood, and the page says what did not happen above the list -
    // the control that sent it went off the page with the row.
    const alert = page.locator("[data-inbox-dismiss-refused]");
    await expect(alert).toHaveRole("alert");
    await expect(alert).toHaveText(`Could not dismiss 1 thread: ${WRITE_REFUSED.error}.`);
    await expect(replyRow).toBeVisible();
    expect(
      await rows.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row")))
    ).toEqual(before);
    await expectSidebarInboxCount(page, 3);
    expect(await dismissedThreads()).toBe(0);
  } finally {
    await alice.close();
  }
});
