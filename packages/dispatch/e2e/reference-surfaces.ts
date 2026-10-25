import { expect, type Locator, type Page } from "@playwright/test";

import { getIssue } from "./api";
import { openedThreadCard, setSheet } from "./editor";

/** The agent session the reference specs write as, so a write is not the viewer's own. */
export const referenceAgent = {
  actor: { id: "e2e-references-agent", kind: "session" as const },
  as: "agent" as const,
};

/** A question block's ask once settlement has made it: the server indexes a block into its ask
 *  row after the version holding it is written, one block at a time. */
export async function settledBlockAsk(
  issueKey: string,
  blockId: string
): Promise<{ id: string; question: string }> {
  const find = async () =>
    (await getIssue(issueKey)).open_asks.find((candidate) => candidate.block_id === blockId);
  await expect.poll(async () => (await find()) !== undefined).toBe(true);
  const ask = await find();
  if (ask === undefined) {
    throw new Error(`the ${blockId} question block of ${issueKey} was not settled`);
  }
  return { id: ask.id, question: ask.question };
}

/** The `data-dispatch-ref` of every element in scope matching `selector`, in document order: the
 *  reference a rendered link (`a[data-dispatch-ref]`) or an inert preview's link-styled span
 *  names, normalised to its `dispatch://` form by the renderer. */
export function referencesIn(scope: Locator, selector = "a[data-dispatch-ref]"): Promise<string[]> {
  return scope
    .locator(selector)
    .evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("data-dispatch-ref") ?? "")
    );
}

/** The `data-dispatch-href` of every reference title the document editor draws in scope, in
 *  document order (`@legion/proof-editor`'s reference decorations, `a.dispatch-reference`). */
export function editorReferencesIn(scope: Locator): Promise<string[]> {
  return scope
    .locator("a.dispatch-reference")
    .evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("data-dispatch-href") ?? "")
    );
}

/** A margin thread's collapsed card: the toggle that holds its one-line preview. On the phone
 *  layout the margin is a bottom sheet, which this opens first. */
export async function collapsedMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await setSheet(page, project, true);
  const toggle = page
    .getByTestId("margin-sheet")
    .locator(`[data-margin-item="${rootId}"]`)
    .locator('button[aria-expanded="false"]');
  await expect(toggle).toBeVisible();
  return toggle;
}

/** Opens a margin thread from its collapsed card, as a reader's click does, and answers the
 *  opened card: beside the document, or in the Thread dialog on the phone layout. The click lands
 *  on the card's anchor quote, since its preview keeps its links live and the thread's root must
 *  be anchored. */
export async function openMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await (await collapsedMarginThread(page, project, rootId)).locator("blockquote").click();
  const card = openedThreadCard(page, project, rootId);
  await expect(card).toBeVisible();
  return card;
}
