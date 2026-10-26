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

/** The reference each element in scope matching `selector` names, each once, in order of first
 *  appearance: a Dispatch picture's `data-dispatch-picture`, any other's `data-dispatch-ref`. A
 *  rendered body's links and pictures, or an inert preview's link-styled spans. */
export async function referenceTargetsIn(scope: Locator, selector: string): Promise<string[]> {
  const references = await scope
    .locator(selector)
    .evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("data-dispatch-picture") ??
          element.getAttribute("data-dispatch-ref") ??
          ""
      )
    );
  return [...new Set(references)];
}

/** The `data-dispatch-href` of every reference title the document editor draws in scope, each
 *  once, in order of first appearance (`@legion/proof-editor`'s reference decorations,
 *  `a.dispatch-reference`). */
export async function editorReferencesIn(scope: Locator): Promise<string[]> {
  const references = await scope
    .locator("a.dispatch-reference")
    .evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("data-dispatch-href") ?? "")
    );
  return [...new Set(references)];
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
 *  be anchored. The opened card holds no collapsed preview, so what it renders is the comment. */
export async function openMarginThread(
  page: Page,
  project: string,
  rootId: string
): Promise<Locator> {
  await (await collapsedMarginThread(page, project, rootId)).locator("blockquote").click();
  const card = openedThreadCard(page, project, rootId);
  await expect(card).toBeVisible();
  await expect(card.locator('button[aria-expanded="false"]')).toHaveCount(0);
  return card;
}
