import { expect, type Locator, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, listMyReplies } from "./api";
import {
  asSession,
  compactInboxLink,
  expectInboxCount,
  expectSidebarInboxCount,
  replyRowId,
  seedAcceptedSend,
  seedAgentThread,
  seedBulkAgentThreads,
  seedBulkReplyThreads,
  seedCommentThread,
  THREAD_SESSION,
  unreadThreadRows,
} from "./inbox-threads";
import { resetDatabase } from "./seed";
import { holdPosts, refusePosts } from "./sends";
import { asUser } from "./users";

/**
 * The Inbox's **Mark all read** (INFRA-450): one click over a list of six hundred and twenty
 * unread threads, what it takes off the page before the server answers, what the server's
 * answer leaves behind, and what comes back when it refuses.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** How many Reply threads and how many Agent threads one click has to carry. */
const REPLY_THREADS = 600;
const AGENT_THREADS = 20;
/** Two open asks waiting on the reader, so the Inbox count has something left after the click. */
const ASKS = 2;

/** The route one press of **Mark all read** posts to. */
const MARK_ALL = "**/api/v1/me/inbox/read";

interface Seeded {
  readonly issueKey: string;
}

async function seedMany(): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger rollout", "Mirror retries"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: ship tonight?` }, asSession());
  }
  const threads = await createIssue({ project: "CORE", title: "Replay budget" });
  await seedBulkReplyThreads({ count: REPLY_THREADS, issueKey: threads.key, prefix: "Replay" });
  await seedBulkAgentThreads({ count: AGENT_THREADS, prefix: "Rollout" });
  // A Send the session took as a turn of its live conversation: no Inbox row, so one press
  // never marks its reply, which the Agents page goes on counting.
  await seedAcceptedSend({
    body: "Steer: take this as your own turn.",
    reply: "Took it, and here is the answer.",
  });
  expect(await unreadThreadRows()).toBe(REPLY_THREADS);
  expect((await listMyReplies({ kind: ["reply", "agent"], limit: 1 })).total).toBe(
    REPLY_THREADS + AGENT_THREADS
  );
  return { issueKey: threads.key };
}

/** The **View** menu, opened. */
async function openView(page: Page, within: Locator): Promise<Locator> {
  const menu = page.getByRole("menu", { name: "View" });
  if ((await menu.count()) === 0) await within.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(1);
  return menu;
}

/** What the **Unread** side of the read-threads switch says it holds. The menu is shut again by
 *  its own trigger: Escape there is the drawer's, which would close the peek with it. */
async function unreadSwitchLabel(page: Page, within: Locator): Promise<string> {
  const menu = await openView(page, within);
  const label = (
    (await menu
      .getByRole("group", { name: "Show read threads" })
      .locator('[data-inbox-view-choice="unread"]')
      .locator("xpath=..")
      .textContent()) ?? ""
  ).trim();
  await within.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(0);
  return label;
}

/** How many rows the Type filter counts for one of the two thread kinds. */
async function typeCount(page: Page, within: Locator, name: string): Promise<number> {
  if ((await page.locator("[data-inbox-filter-fields]").count()) === 0) {
    await within.locator("[data-inbox-filter]").click();
  }
  const trigger = within.getByRole("button", { name: "Filter by type" });
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
  const options = page.getByRole("listbox", { name: "Type options" });
  const label =
    (await options
      .getByRole("option", { name: new RegExp(`^${name},`) })
      .getAttribute("aria-label")) ?? "";
  await page.keyboard.press("Escape");
  return Number(label.replace(`${name}, `, "").replace(" rows", ""));
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("one click marks six hundred and twenty threads read, and the counts stay down", async ({
  browser,
}) => {
  const seeded = await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    const inbox = page.locator("[data-inbox-bar]");
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    expect(await unreadSwitchLabel(page, inbox)).toBe(`Unread ${REPLY_THREADS + AGENT_THREADS}`);

    // One click, with the request held open: the page is already at zero.
    const held = await holdPosts(page, MARK_ALL);
    await inbox.locator("[data-inbox-mark-all]").click();
    await expect(page.locator("[data-inbox-mark-all]")).toHaveCount(0);
    expect(await unreadSwitchLabel(page, inbox)).toBe("Unread 0");
    expect(await typeCount(page, inbox, "Reply")).toBe(0);
    expect(await typeCount(page, inbox, "Agent message")).toBe(0);
    await expectSidebarInboxCount(page, ASKS);
    await expect(rows).toHaveCount(ASKS);

    // The server answers; a reload reads the same numbers back from it.
    held.release();
    await held.settled();
    await expect.poll(async () => unreadThreadRows()).toBe(0);
    await page.reload();
    await expect(rows).toHaveCount(ASKS);
    await expectSidebarInboxCount(page, ASKS);
    expect(await unreadSwitchLabel(page, inbox)).toBe("Unread 0");

    // The compact header carries the same number, as a link to the Inbox from another page.
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(compactInboxLink(page)).toHaveText(`Inbox ${ASKS}`);
    await page.setViewportSize({ height: 720, width: 1280 });

    // Every thread is still there to read, drawn as read.
    await page.goto("/?threads=all");
    await expect(rows.first()).toBeVisible();
    await expect(page.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(0);

    // The Agents page counts the accepted Send's reply alone: the twenty Agent rows' replies
    // are read, and that one was never an Inbox row to mark.
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expect(page.getByRole("link", { name: "New reply 1" }).first()).toBeVisible();

    // A reply written after the list loaded comes in unread, and a second click marks it.
    await page.goto("/");
    await expect(page.locator("[data-inbox-mark-all]")).toHaveCount(0);
    const late = await seedCommentThread(
      seeded.issueKey,
      "One more question about the replay.",
      "One more answer about the replay."
    );
    await seedAgentThread({ body: "And the rollout?", replies: ["Still green."] });
    await expect(page.locator(`[data-inbox-row="${replyRowId(late)}"]`)).toBeVisible();
    await expect(page.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(2);
    await page.locator("[data-inbox-mark-all]").click();
    await expect(page.locator("[data-inbox-row] [aria-label='Unread']")).toHaveCount(0);
    await expect.poll(async () => unreadThreadRows()).toBe(0);
  } finally {
    await alice.close();
  }
});

test("the drawer's Mark all read clears the same counts @phone", async ({ browser }) => {
  await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await expectInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const drawer = page.getByTestId("inbox-drawer");
    await expect(drawer).toBeVisible();
    await expect(drawer.locator("[data-inbox-row]").first()).toBeVisible();

    const held = await holdPosts(page, MARK_ALL);
    await drawer.locator("[data-inbox-mark-all]").click();
    await expect(drawer.locator("[data-inbox-mark-all]")).toHaveCount(0);
    const bar = drawer.locator("[data-inbox-bar]");
    expect(await unreadSwitchLabel(page, bar)).toBe("Unread 0");
    expect(await typeCount(page, bar, "Reply")).toBe(0);
    await expectInboxCount(page, ASKS);
    await expect(drawer.locator("[data-inbox-row]")).toHaveCount(ASKS);

    held.release();
    await held.settled();
    await expect.poll(async () => unreadThreadRows()).toBe(0);
    await expectInboxCount(page, ASKS);
  } finally {
    await alice.close();
  }
});

test("a refused Mark all read brings the rows and the counts back and says nothing was marked", async ({
  browser,
}) => {
  await seedMany();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.first()).toBeVisible();
    // The asks and the threads are two reads, and `count()` waits for nothing: the threads'
    // own read has to be on the page before the rows are counted, or `before` is the asks
    // alone. Both thread kinds come with that read, so both are waited for.
    await expect(page.locator('[data-inbox-row^="reply:"]').first()).toBeVisible();
    await expect(page.locator('[data-inbox-row^="agent:"]').first()).toBeVisible();
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    const before = await rows.count();
    expect(before).toBeGreaterThan(ASKS);

    const refuse = await refusePosts(page, MARK_ALL);
    await page.locator("[data-inbox-mark-all]").click();
    await expect(rows).toHaveCount(ASKS);
    refuse();

    const alert = page.locator("[data-inbox-mark-all-refused]");
    await expect(alert).toHaveRole("alert");
    await expect(alert).toHaveText("Nothing was marked read.");
    await expect(rows).toHaveCount(before);
    await expectSidebarInboxCount(page, ASKS + REPLY_THREADS + AGENT_THREADS);
    expect(await unreadThreadRows()).toBe(REPLY_THREADS);
  } finally {
    await alice.close();
  }
});
