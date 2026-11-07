/**
 * Opening a comment's thread in the Conversation, for the `view` specs (dispatch://INFRA-682).
 *
 * A `view` block in a reply is only on the page while its thread is open, and a page of blocks
 * is the one page where opening a thread is not a single click: the conversation list re-renders
 * while a reader is in it - as its reads answer, and as each block sizes the frame it holds -
 * and a re-render that lands on the click leaves the thread closed behind its clipped preview.
 * That is how `view-block.e2e.ts`'s comment and reply assertions failed in `firefox` and
 * `webkit`, with the preview `Attempts, in a comment. 1 reply · last reply just now` in the
 * failure's snapshot. Both specs open a thread through here rather than each holding its own
 * timing.
 */
import { expect, type Locator, type Page } from "@playwright/test";

/**
 * The thread of the comment `rootId` opened, and the Replies list its replies are drawn in:
 * inline at a desktop width, in the thread view on a phone. The click is retried until that list
 * is there - the thread's own content, drawn nowhere else, since a collapsed card shows one
 * clipped line and no reply at all - rather than waited for on the clock, and only while the
 * thread is still closed, so a thread that opened is never toggled shut again. A block read
 * inside the list the caller is given is a block the open thread was showing.
 *
 * The comment must have at least one reply: a thread with none draws no list.
 */
export async function openCommentThread(page: Page, rootId: string): Promise<Locator> {
  const expand = page
    .locator(`[data-turn="comment:${rootId}"]`)
    .getByRole("button", { exact: true, name: "Expand thread" })
    .first();
  const replies = page.getByRole("list", { exact: true, name: "Replies" }).first();
  await expect(async () => {
    if (await expand.isVisible()) {
      await expand.click();
    }
    await expect(replies).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 30_000 });
  return replies;
}
