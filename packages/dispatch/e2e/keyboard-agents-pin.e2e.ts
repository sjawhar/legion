import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  agentRow,
  type FakeSession,
  holdPosts,
  openAgents,
  plannerSession,
  reviewerSession,
  seedAgents,
  setLiveSessions,
  shownAgentRows,
} from "./agents";
import { createAgentMessage, createMessage, replyToMessageDelivery } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Shift+P and what a pin moves. A pin takes a row between the open list and a fold, and the page
// keeps every row in one keyed list (`AgentsPage.tsx`), so the row that moves is the same row:
// these rows pin what it still holds when it lands - focus, whether it is open, its draft, its
// issue, a reply in progress, a send or an upload still out, and the unread set it opened with.

/** `seedAgents`, plus `Quiet` and `Silent`: live sessions with no Dispatch activity, which fold
 *  under `No Dispatch activity` until one is pinned - the rows a pin moves across. */
async function seedWithFold(): Promise<void> {
  await seedAgents();
  const folded = (id: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/srv/${id}`,
    machine_id: "box-1",
    roles: ["tester"],
    session_id: `${id}-session`,
    title,
  });
  await setLiveSessions([
    plannerSession,
    reviewerSession,
    folded("quiet", "Quiet"),
    folded("silent", "Silent"),
  ]);
}

/** Opens the fold and roves to its last row, `Silent`, the way a keyboard reader gets there. */
async function roveToSilent(page: Page): Promise<Locator> {
  const row = agentRow(page, "silent-session");
  await page.getByRole("button", { name: /^No Dispatch activity/ }).click();
  await expect(row).toBeVisible();
  await page.locator("body").focus();
  for (const _ of [1, 2, 3, 4]) await page.keyboard.press("j");
  await expect(row).toBeFocused();
  return row;
}

/** Back on the row itself - a click beside its controls, which is how a reader leaves a written
 *  composer without its Discard prompt - and pinned from the keyboard. */
async function pinFromRow(row: Locator, place: string): Promise<void> {
  await row.getByText(place).click();
  await expect(row).toBeFocused();
  await row.page().keyboard.press("Shift+P");
}

test.beforeEach(async () => {
  await resetDatabase();
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([]);
  }
});

test.describe("agents page pins", () => {
  // A pin moves the row's node, which can drop focus to the document, or hides it in a closed
  // fold. Focus follows the row to where it lands - or, when it lands in a closed fold, to that
  // fold's toggle - so the next `j`/`k` go on from the reader's place instead of from the top.
  test("Shift+P keeps focus on the row it moves, or on the fold it moves into", async ({
    browser,
  }) => {
    await seedWithFold();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const fold = page.getByRole("button", { name: /^No Dispatch activity/ });
      const silentRow = await roveToSilent(page);

      // Out of the fold and to the top of the open list; `j` then goes on from there.
      await page.keyboard.press("Shift+P");
      await expect(silentRow.getByRole("button", { name: "Unpin Silent" })).toBeVisible();
      await expect(rows.nth(0)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(silentRow).toBeFocused();
      await page.keyboard.press("j");
      await expect(rows.nth(1)).toBeFocused();
      await page.keyboard.press("k");
      await expect(silentRow).toBeFocused();

      // Back into the open fold.
      await page.keyboard.press("Shift+P");
      await expect(silentRow.getByRole("button", { name: "Pin Silent" })).toBeVisible();
      await expect(rows.nth(3)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(silentRow).toBeFocused();

      // Into a closed fold, where the row is hidden: the fold's toggle takes focus.
      await page.keyboard.press("Shift+P");
      await expect(silentRow).toBeFocused();
      await fold.click();
      await expect(fold).toHaveAttribute("aria-expanded", "false");
      await page.locator("body").focus();
      await page.keyboard.press("j");
      await expect(silentRow).toBeFocused();
      await page.keyboard.press("Shift+P");
      await expect(silentRow).toBeHidden();
      await expect(fold).toHaveAccessibleName("No Dispatch activity (2)");
      await expect(fold).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // Whether the row is open, the reader's draft and the issue that draft is addressed to are the
  // row's own, so the moved row comes back as it was left - and its draft still goes where it
  // was going.
  test("Shift+P carries the row's open state, its draft and its issue across the move", async ({
    browser,
  }) => {
    await seedWithFold();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const silentRow = await roveToSilent(page);
      const opener = silentRow.getByRole("button", { exact: true, name: "Silent" });
      const toggle = silentRow.getByRole("button", { name: "Choose issue" });
      const field = silentRow.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await silentRow.getByRole("combobox", { name: "Issue" }).selectOption("CORE-1");
      await field.click();
      await page.keyboard.press("End");
      await page.keyboard.type(" can you take this");
      await expect(field).toHaveValue("@Silent can you take this");

      await pinFromRow(silentRow, "box-1 · /srv/silent");
      await expect(rows.nth(0)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(silentRow).toBeFocused();
      await expect(opener).toHaveAttribute("aria-expanded", "true");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue("@Silent can you take this");

      // The mention travelled as a record, not just as text: it is on the wire.
      const posted = page.waitForRequest(
        (request) =>
          request.method() === "POST" && new URL(request.url()).pathname.endsWith("/comments")
      );
      await field.click();
      await page.keyboard.press("Control+Enter");
      const request = await posted;
      expect(new URL(request.url()).pathname).toBe("/api/v1/issues/CORE-1/comments");
      expect(request.postDataJSON()).toMatchObject({
        body: "@Silent can you take this",
        mentions: [{ target: "session:silent-session" }],
      });
    } finally {
      await context.close();
    }
  });

  // A reply in progress is part of what the row holds: the moved row is still answering the same
  // message, with the reader's words rather than the reply's fresh start.
  test("Shift+P keeps a reply in progress, and the reply still answers its message", async ({
    browser,
  }) => {
    await seedWithFold();
    const asked = await createAgentMessage("silent-session", {
      body: "Are you free?",
      delivery: "btw",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const silentRow = await roveToSilent(page);
      const field = silentRow.getByRole("textbox", { name: "Comment" });
      const cancelReply = silentRow.getByRole("button", { name: "Cancel reply" });

      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await silentRow.getByRole("button", { name: "Reply" }).first().click();
      await expect(cancelReply).toBeVisible();
      await field.fill("Yes, go ahead");
      await pinFromRow(silentRow, "box-1 · /srv/silent");

      await expect(rows.nth(0)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(silentRow).toBeFocused();
      await expect(cancelReply).toBeVisible();
      await expect(field).toHaveValue("Yes, go ahead");

      const posted = page.waitForRequest(
        (request) =>
          request.method() === "POST" && new URL(request.url()).pathname.endsWith("/messages")
      );
      await field.click();
      await page.keyboard.press("Control+Enter");
      const request = await posted;
      expect(new URL(request.url()).pathname).toBe("/api/v1/agents/silent-session/messages");
      expect(request.postDataJSON()).toMatchObject({
        body: "Yes, go ahead",
        in_reply_to: asked.id,
      });
    } finally {
      await context.close();
    }
  });

  // A reply to a message on an issue is written on that issue, whose channel seeds the agent's
  // mention. The reply's own start is what the reader writes after, so neither a pin nor folding
  // the row shut and open again may put the seed in front of the reply.
  test("a reply on an issue keeps exactly its text across a pin and a collapse", async ({
    browser,
  }) => {
    await seedWithFold();
    const root = await createMessage("CORE-1", {
      body: "Can this ship?",
      delivery: "btw",
      target: "session:silent-session",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const silentRow = await roveToSilent(page);
      const opener = silentRow.getByRole("button", { exact: true, name: "Silent" });
      const field = silentRow.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await silentRow.getByRole("button", { name: "Reply" }).first().click();
      await expect(silentRow.getByRole("button", { name: "Cancel reply" })).toBeVisible();
      await field.fill("Yes, go ahead");
      await pinFromRow(silentRow, "box-1 · /srv/silent");
      await expect(silentRow).toBeFocused();
      await expect(field).toHaveValue("Yes, go ahead");
      await opener.click();
      await expect(opener).toHaveAttribute("aria-expanded", "false");
      await opener.click();
      await expect(field).toHaveValue("Yes, go ahead");

      const posted = page.waitForRequest(
        (request) =>
          request.method() === "POST" && new URL(request.url()).pathname.endsWith("/messages")
      );
      await field.click();
      await page.keyboard.press("Control+Enter");
      const request = await posted;
      expect(new URL(request.url()).pathname).toBe("/api/v1/issues/CORE-1/messages");
      expect(request.postDataJSON()).toMatchObject({
        body: "Yes, go ahead",
        in_reply_to: root.id,
      });
    } finally {
      await context.close();
    }
  });

  // A pin made while the row's message is in the air moves the row with the send still out. The
  // composer that made the send is the one the reader sees, so it answers for it: held until the
  // server does, then empty.
  test("a pin mid-send keeps the send's own composer, which empties when it lands", async ({
    browser,
  }) => {
    await seedWithFold();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");
      const silentRow = await roveToSilent(page);
      const field = silentRow.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await pinFromRow(silentRow, "box-1 · /srv/silent");

      await expect(rows.nth(0)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(silentRow).toBeFocused();
      await expect(silentRow.getByRole("button", { exact: true, name: "Silent" })).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      await expect(field).toBeDisabled();
      await expect(field).toHaveValue("Status please");
      send.release();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // An upload in flight holds Send and appends its reference when it lands. Both belong to the
  // composer that started it, so a pin in between must leave Send held and the reference coming.
  test("an upload in flight across a pin holds Send, and its reference reaches the draft", async ({
    browser,
  }) => {
    await seedWithFold();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const upload = await holdPosts(page, "**/api/v1/issues/*/artifacts");
      const silentRow = await roveToSilent(page);
      const field = silentRow.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("Enter");
      await silentRow.getByRole("button", { name: "Choose issue" }).click();
      await silentRow.getByRole("combobox", { name: "Issue" }).selectOption("CORE-1");
      await expect(field).toHaveValue("@Silent");
      // A pasted file, as the clipboard hands one to the field.
      await field.evaluate((node) => {
        const data = new DataTransfer();
        data.items.add(new File(["# Notes\n"], "notes.md", { type: "text/markdown" }));
        node.dispatchEvent(
          new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: data })
        );
      });
      const uploading = silentRow.getByRole("button", { name: "Uploading file…" });
      await expect(uploading).toBeDisabled();

      await pinFromRow(silentRow, "box-1 · /srv/silent");
      await expect(shownAgentRows(page).nth(0)).toHaveAttribute("data-agent-row", "silent-session");
      await expect(uploading).toBeDisabled();
      upload.release();
      await expect(field).toHaveValue(/^@Silent\s+dispatch:\/\/\S*notes/);
      await expect(silentRow.getByRole("button", { name: "Send" })).toBeEnabled();
      expect(upload.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // An exchange shown because its reply was unread when the row opened stays shown while the
  // reader reads it, however long that takes. A pin that took the row's unread set with it
  // would fold those exchanges away behind `Show N older` in front of the reader.
  test("Shift+P keeps the exchanges the row opened unread on screen", async ({ browser }) => {
    await seedAgents();
    const stale: FakeSession = {
      // Steer, so the message the row sends is delivered and the list holds only what is read.
      capabilities: ["aside", "btw", "steer"],
      dir: "/srv/stale",
      // Unseen for ten minutes, so it folds under Inactive whatever it has said.
      last_seen: Date.now() - 45 * 60_000,
      machine_id: "box-1",
      roles: ["tester"],
      session_id: "stale-session",
      title: "Stale",
    };
    await setLiveSessions([plannerSession, reviewerSession, stale]);
    for (const turn of ["First", "Second"]) {
      const asked = await createAgentMessage(stale.session_id, {
        body: `${turn} question`,
        delivery: "btw",
      });
      await replyToMessageDelivery(
        asked.id,
        { attempt: 1, body: `${turn} answer` },
        { id: stale.session_id, kind: "session" }
      );
    }
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const staleRow = agentRow(page, "stale-session");
      const conversation = staleRow.getByRole("list", { name: "Conversation with Stale" });
      const field = staleRow.getByRole("textbox", { name: "Comment" });

      await page.getByRole("button", { name: /^Inactive/ }).click();
      await staleRow.getByRole("button", { exact: true, name: "Stale" }).click();
      await expect(conversation).toContainText("Second answer");
      await expect(conversation).toContainText("First answer");

      // One more exchange, the newest: the two unread ones stay shown under it.
      const sent = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === `/api/v1/agents/${stale.session_id}/messages`
      );
      await field.fill("Third question");
      await field.press("Control+Enter");
      expect((await sent).ok()).toBe(true);
      await expect(conversation).toContainText("Third question");
      await expect(conversation).toContainText("First answer");

      await pinFromRow(staleRow, "box-1 · /srv/stale");
      await expect(shownAgentRows(page).nth(0)).toHaveAttribute("data-agent-row", "stale-session");
      await expect(conversation).toContainText("Third question");
      await expect(conversation).toContainText("Second answer");
      await expect(conversation).toContainText("First answer");
      await expect(staleRow.getByRole("button", { name: /^Show \d+ older$/ })).toHaveCount(0);
    } finally {
      await context.close();
    }
  });
});
