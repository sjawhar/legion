/**
 * What the `view` specs share (dispatch://INFRA-682): the block as an author writes it, the
 * block as a reader's page holds it, the data a page draws, the issue that holds a page file
 * and a data file, and opening a comment's thread in the Conversation.
 *
 * One module, so a change to the directive or to the seed is one edit rather than four:
 * `view-block.e2e.ts`, `view-budget.e2e.ts`, `view-grants.e2e.ts` and `view-sandbox.e2e.ts`
 * each kept a copy of these, and the four copies were the same text. Only the page a row shows
 * stays the row's own, since each spec reads something different of its page.
 */
import { expect, type Locator, type Page } from "@playwright/test";

import { createArtifactGrant, createIssue, createProject, uploadIssueFile } from "./api";

/** The project every `view` row's issue and document live in. */
export const VIEW_PROJECT = "CORE";

/** The one data file of the seed below, as text a page can draw: one bar, whose `.value` is
 *  what a row reads back out of the frame. */
export function attempts(value: number): string {
  return JSON.stringify([{ label: "attempts", value }]);
}

/** A `view` block as an author writes it. `id` is the block's own, for a row that names it. */
export function viewDirective(block: {
  caption: string;
  data: string;
  id?: string;
  page: string;
}): string {
  const id = block.id === undefined ? "" : `#${block.id} `;
  return `:::view{${id}page="${block.page}" data="${block.data}"}\n${block.caption}\n:::\n`;
}

/** The block whose caption is `caption`, in a document or in any body. */
export function viewBlock(page: Page, caption: string): Locator {
  return page.locator('section[data-proof-block-type="view"]').filter({ hasText: caption });
}

/** The project the seed's issue needs. A row that seeds two issues, or that writes a project
 *  document of its own, calls it once itself rather than through the seed. */
export async function seedViewProject(): Promise<void> {
  await createProject({ key: VIEW_PROJECT, name: "Core" });
}

/** An issue holding one page file and one data file, as the rows read them back. */
export interface ViewFixture {
  readonly dataId: string;
  readonly dataRef: string;
  readonly issue: string;
  readonly pageId: string;
  readonly pageRef: string;
}

/**
 * An issue holding `html` as a page file and one data file, granted to that page unless
 * `grant` says otherwise: where nearly every `view` row starts. Both are uploaded as files
 * (multipart), since a JSON upload is stored as markdown, which is a document, and a view reads
 * no document.
 *
 * The project must be there already (`seedViewProject`). The names are a row's own where it
 * seeds two pages on one issue and must tell their references apart.
 */
export async function seedViewIssue(options: {
  /** The data file's bytes; one bar of value 7 where a row does not care. */
  readonly data?: string;
  /** The data file's name, without `.json`. */
  readonly dataName?: string;
  /** Whether the page file is granted that data file. A row about grants seeds without one. */
  readonly grant?: boolean;
  /** The page file's bytes: the one thing every spec brings its own of. */
  readonly html: string;
  /** The page file's name, without `.html`. */
  readonly pageName?: string;
  readonly title: string;
}): Promise<ViewFixture> {
  const dataName = options.dataName ?? "loop";
  const pageName = options.pageName ?? "chart";
  const issue = await createIssue({ project: VIEW_PROJECT, title: options.title });
  const pageFile = await uploadIssueFile(issue.key, {
    content: options.html,
    name: `${pageName}.html`,
    type: "text/html; charset=utf-8",
  });
  const dataFile = await uploadIssueFile(issue.key, {
    content: options.data ?? attempts(7),
    name: `${dataName}.json`,
    type: "application/json",
  });
  const dataRef = `dispatch://${issue.key}/artifact/${dataName}-json`;
  if (options.grant !== false) {
    await createArtifactGrant(pageFile.artifact.id, dataRef);
  }
  return {
    dataId: dataFile.artifact.id,
    dataRef,
    issue: issue.key,
    pageId: pageFile.artifact.id,
    pageRef: `dispatch://${issue.key}/artifact/${pageName}-html`,
  };
}

/**
 * The thread of the comment `rootId` opened, and the Replies list its replies are drawn in:
 * inline at a desktop width, in the thread view on a phone. The click is retried until that list
 * is there - the thread's own content, drawn nowhere else, since a collapsed card shows one
 * clipped line and no reply at all - rather than waited for on the clock, and only while the
 * thread is still closed, so a thread that opened is never toggled shut again. A block read
 * inside the list the caller is given is a block the open thread was showing.
 *
 * A `view` block in a reply is only on the page while its thread is open, and a page of blocks
 * is the one page where opening a thread is not a single click: the conversation list re-renders
 * while a reader is in it - as its reads answer, and as each block sizes the frame it holds -
 * and a re-render that lands on the click leaves the thread closed behind its clipped preview.
 * That is how `view-block.e2e.ts`'s comment and reply assertions failed in `firefox` and
 * `webkit`, with the preview `Attempts, in a comment. 1 reply · last reply just now` in the
 * failure's snapshot. Both specs open a thread through here rather than each holding its own
 * timing.
 *
 * Both locators are read inside this comment's own turn, and the pass condition is the returned
 * value: another thread open elsewhere on the page can neither end the retry on an attempt whose
 * click was lost nor hand the caller another comment's replies. `ThreadReplies` draws its
 * `<ol aria-label="Replies">` inside the root comment's `<li data-turn="comment:…">` on both
 * layouts - on a phone the thread dialog holds a turn of its own, with the same `data-turn`,
 * while the list behind it is `aria-hidden`, so the turn that answers a role query there is the
 * dialog's.
 *
 * The comment must have at least one reply: a thread with none draws no list.
 */
export async function openCommentThreadReplies(page: Page, rootId: string): Promise<Locator> {
  const turn = page.locator(`[data-turn="comment:${rootId}"]`);
  const expand = turn.getByRole("button", { exact: true, name: "Expand thread" }).first();
  const replies = turn.getByRole("list", { exact: true, name: "Replies" }).first();
  await expect(async () => {
    if (await expand.isVisible()) {
      await expand.click();
    }
    await expect(replies).toBeVisible({ timeout: 1_000 });
  }).toPass({ timeout: 30_000 });
  return replies;
}
