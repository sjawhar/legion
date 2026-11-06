/** Every number the agent workspace's layout rests on: the width of its session column and the
 *  widths of the two panes beside it. Its own module, and a pure one - no React, no DOM read -
 *  because the arithmetic has to hold at widths no test can render: the column's width is restored
 *  from stored text before the workspace mounts, and the pane split is recomputed on every resize,
 *  so both are worth checking as plain functions rather than through a layout. */

/** The session column's resting width. It is what leaves room for two panes at 1440 with the
 *  sidebar shown: the navigation aside takes 320 and one border (`w-80 border-r` in
 *  `packages/dispatch/web/src/app.tsx`) and `<main>` takes 24 of padding on each side (`p-6` in the
 *  same file), which leaves 1071; 1071 - 240 - 24 is 807, and 807 holds two panes of at least 390
 *  with 24 between them. */
export const DEFAULT_COLUMN_WIDTH = 240;

/** The ends of the drag, and the arrow-key step, in the shape the margin column uses
 *  (`packages/dispatch/web/src/features/margin/margin-width.ts`): a fixed floor rather than a
 *  fraction of the viewport, and keys that move the edge by one page gutter. The ceiling is twice
 *  the default and still leaves 567 of the 1071 above to the view at 1440, well over one pane's
 *  390. */
export const MIN_COLUMN_WIDTH = 200;
export const MAX_COLUMN_WIDTH = 480;
export const COLUMN_WIDTH_STEP = 24;

/** The stored preference is read from text, so a width that is not a number at all answers the
 *  default rather than propagating a NaN into a CSS length. */
export function clampColumnWidth(width: number): number {
  if (!Number.isFinite(width)) return DEFAULT_COLUMN_WIDTH;
  return Math.min(Math.max(width, MIN_COLUMN_WIDTH), MAX_COLUMN_WIDTH);
}

/** The narrowest a pane stays readable at: the width the browser suite already drives the live
 *  view at, a 390 by 844 phone viewport in `packages/dispatch/e2e/agent-view.e2e.ts`. A pane
 *  narrower than that is a layout no test covers. */
export const MIN_PANE_WIDTH = 390;

/** The gap this application puts between two columns: `gap-6` between the Inbox's two columns in
 *  `packages/dispatch/web/src/features/inbox/Inbox.tsx`. */
export const PANE_GAP = 24;

/** Two panes on their floor with the gap between them. */
export const MIN_TWO_PANE_WIDTH = MIN_PANE_WIDTH * 2 + PANE_GAP;

export const DEFAULT_PANE_SHARE = 0.5;

export function fitsTwoPanes(areaWidth: number): boolean {
  return areaWidth >= MIN_TWO_PANE_WIDTH;
}

/** The left pane's width, clamped so that neither pane falls below its floor. Not rounded to whole
 *  pixels: the caller sets these as CSS lengths, and the browser owns the rounding - rounding here
 *  would cost the area the fraction the two widths and the gap must add back up to. */
function leftPaneWidth(areaWidth: number, share: number): number {
  const room = areaWidth - PANE_GAP;
  const wanted = (Number.isFinite(share) ? share : DEFAULT_PANE_SHARE) * room;
  // Below `MIN_TWO_PANE_WIDTH` no split keeps both panes on their floor, so the floors cancel and
  // the room is halved. A caller that asked `fitsTwoPanes` first never reaches this.
  if (room < MIN_PANE_WIDTH * 2) return room / 2;
  return Math.min(Math.max(wanted, MIN_PANE_WIDTH), room - MIN_PANE_WIDTH);
}

/** The share `paneWidths` reads a requested `share` as, for the splitter to report through
 *  `aria-valuenow`: the pointer may be dragged past a pane's floor, and the handle must not claim
 *  a position the panes did not take. */
export function clampPaneShare(share: number, areaWidth: number): number {
  return leftPaneWidth(areaWidth, share) / (areaWidth - PANE_GAP);
}

/** The two pane widths for a share of the room the panes have, `areaWidth - PANE_GAP`. The right
 *  pane takes what the left one leaves, so `left + right + PANE_GAP` is the area exactly. */
export function paneWidths(areaWidth: number, share: number): { left: number; right: number } {
  const left = leftPaneWidth(areaWidth, share);
  return { left, right: areaWidth - PANE_GAP - left };
}
