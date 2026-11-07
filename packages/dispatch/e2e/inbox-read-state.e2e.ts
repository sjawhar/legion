import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createIssue, createProject, listMyReplies } from "./api";
import { selectedRow, selectRow } from "./inbox-pane";
import { replyRowId, seedCommentThread, THREAD_SESSION, unreadThreadRows } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * Reading a thread in the Inbox marks it read (INFRA-450, A7): "a thread is marked when its
 * newest message has been on screen, in the pane or on a card ... three quarters of the message
 * visible for one second, or, for a message taller than the screen, the message filling three
 * quarters of the screen for one second". The row stays where it is, drawn as read: a thread
 * leaves the Inbox only when the reader dismisses it (`e2e/inbox-dismiss.e2e.ts`).
 *
 * The reading pane is an inner scroller about 200 px shorter than the window, so a reply taller
 * than the pane can never cover three quarters of the *window*: at the suite's own viewport
 * (`Desktop Chrome`, 1280x720) the pane is 519 px tall and the rule asks for 540 px. A reader who
 * scrolls such a reply through the pane for as long as they like never clears its row, which is
 * the complaint this issue exists to answer.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** A reply far taller than the reading pane at any window height the suite runs at. */
const LONG_REPLY = Array.from(
  { length: 40 },
  (_, index) => `Line ${index + 1} of the replay answer.`
).join("\n\n");

test.beforeEach(async () => {
  await resetDatabase();
});

test("a reply taller than the reading pane is marked read by reading it, and its row stays", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(issue.key, "Why does it give up?", LONG_REPLY);
  // A second thread, so the selection has somewhere to move once the long one has been read.
  const other = await seedCommentThread(
    issue.key,
    "And how far back does the replay look?",
    "One hour, then it gives up."
  );
  expect(await unreadThreadRows()).toBe(2);
  // Whether the long thread itself is still unread. The other row is read as soon as the column
  // draws it, so the count of every unread thread would not say which of the two this is about.
  const longUnread = async (): Promise<boolean> =>
    (await listMyReplies({ state: "unread" })).rows.some((row) => row.thread_id === thread);
  expect(await longUnread()).toBe(true);

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(row).toBeVisible();
    test.skip(
      (await page.locator("[data-inbox-thread-body]").count()) === 0,
      "the reading pane is this row's subject"
    );
    await row.locator("time").first().click();
    await expect(row).toHaveAttribute("data-inbox-selected", "true");
    const reply = page.locator("[data-inbox-thread-turn]").last();
    await expect(reply).toBeVisible();

    // The reader scrolls the reply through the pane, a part of a screen at a time, pausing on
    // each - the dwell the read mark asks for, given ten times over.
    const pane = await reply.boundingBox();
    expect(pane).not.toBeNull();
    for (let step = 0; step < 10; step += 1) {
      await page.mouse.move((pane?.x ?? 0) + 10, 400);
      await page.mouse.wheel(0, 300);
      await page.waitForTimeout(1200);
      if (!(await longUnread())) break;
    }
    expect(await longUnread()).toBe(false);
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);

    // The row stays where it stood, drawn as read: only a dismiss takes a thread out of the
    // Inbox. The selection moves to the other row, and a reload reads both rows back.
    await selectRow(page, replyRowId(other));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", replyRowId(other));
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
    await page.reload();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(2);
    await expect(row).toBeVisible();
    await expect(row.locator("[aria-label='Unread']")).toHaveCount(0);
  } finally {
    await context.close();
  }
});
