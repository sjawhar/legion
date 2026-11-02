import { expect, type Locator, type Page } from "@playwright/test";

/**
 * The Inbox's reading column (INFRA-575), for the specs that drive the Inbox at whatever width
 * their project gives them.
 *
 * From `READING_PANE_WIDTH` the Inbox page is two columns: short rows in `inbox-list`, and the
 * selected question's own surface - its document, or its card - in `inbox-pane`, whose head
 * carries that question's Snooze, its priority and its "Assign to me". A short row keeps every
 * `data-inbox-*` attribute it had, its checkbox, its owner link, a kind badge, the question's
 * first line and its age, and carries `data-inbox-selected="true"` while the column is showing
 * it. Below that width, and in the drawer at any width, the Inbox is the list of cards it has
 * always been: each row holds its own card and its own controls.
 *
 * Each helper reads which of the two this page is drawing off the viewport, so one spec row runs
 * under `chromium` (1280 px, the column on) and `iphone` (390 px, the column off).
 */

/** The width the Inbox draws its reading column from (`READING_PANE_QUERY`, 80rem). */
export const READING_PANE_WIDTH = 1280;

/** Whether this page is wide enough for the Inbox page's reading column. */
export function hasReadingPane(page: Page): boolean {
  return (page.viewportSize()?.width ?? 0) >= READING_PANE_WIDTH;
}

/** The list column, which is the whole Inbox where there is no reading column. */
export function inboxList(page: Page): Locator {
  return page.getByTestId("inbox-list");
}

/** The reading column; absent below `READING_PANE_WIDTH` and in the drawer. */
export function inboxPane(page: Page): Locator {
  return page.getByTestId("inbox-pane");
}

/** One question's row, at either width. */
export function inboxRow(page: Page, askId: string): Locator {
  return page.locator(`[data-inbox-row="${askId}"]`);
}

/** The row the reading column is showing. */
export function selectedRow(page: Page): Locator {
  return inboxList(page).locator('[data-inbox-selected="true"]');
}

/**
 * Selects a row the way the pointer does: one click both selects it and leaves the focus on it.
 * The click lands on the row's age, the one part of a short row that is neither the owner link
 * nor a control, so it reaches the row itself whatever the question's length leaves on the
 * line. Without the column there is no selection, so this is nothing to do - a click there
 * would land in the row's own card.
 */
export async function selectRow(page: Page, askId: string): Promise<void> {
  if (!hasReadingPane(page)) return;
  const row = inboxRow(page, askId);
  await row.locator("time").first().click();
  await expect(row).toHaveAttribute("data-inbox-selected", "true");
}

/** The question's own card, where this width draws it: in the reading column, given the row
 *  first, or in the row itself. */
export async function askCard(page: Page, askId: string): Promise<Locator> {
  await selectRow(page, askId);
  return hasReadingPane(page)
    ? inboxPane(page).getByTestId(`ask-${askId}`)
    : page.getByTestId(`ask-${askId}`);
}

/** The box holding a question's own controls - its priority, "Assign to me", its Snooze and the
 *  owner link beside them: the reading column's head, or the row itself. */
export async function askControls(page: Page, askId: string): Promise<Locator> {
  await selectRow(page, askId);
  return hasReadingPane(page) ? inboxPane(page).locator("header").first() : inboxRow(page, askId);
}

/** The box the Inbox's rows scroll in: the list column, which scrolls in itself beside the
 *  reading column, and the window where the list is the whole page. */
export function listScrollTop(page: Page): Promise<number> {
  return page.evaluate(() => {
    const list = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
    return list !== null && list.scrollHeight > list.clientHeight ? list.scrollTop : window.scrollY;
  });
}

/** Scrolls whichever box that is so the row's centre sits at the centre of what is visible. */
export async function centreRow(row: Locator): Promise<void> {
  await row.evaluate((node) => {
    const list = node.closest<HTMLElement>('[data-testid="inbox-list"]');
    const rect = node.getBoundingClientRect();
    if (list !== null && list.scrollHeight > list.clientHeight) {
      const box = list.getBoundingClientRect();
      list.scrollTop += rect.top + rect.height / 2 - (box.top + box.height / 2);
      return;
    }
    window.scrollBy(0, rect.top + rect.height / 2 - window.innerHeight / 2);
  });
}
