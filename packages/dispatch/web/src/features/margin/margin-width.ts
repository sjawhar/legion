/** How wide the margin column may be.
 *
 *  Its own module because the shell reads the default for the stored preference it hands
 *  `Margin` (`app.tsx`), while `Margin` itself is a chunk the shell loads only once it is up
 *  (`features/shell/deferred-shell.ts`): a handful of numbers must not pull the margin's whole
 *  column, its sheet and its composer into the entry chunk. */

export const DEFAULT_MARGIN_WIDTH = 384;
export const MIN_MARGIN_WIDTH = 280;
export const MARGIN_WIDTH_STEP = 24;

/** The widest the margin may be dragged: 60 % of the viewport, never below the minimum. */
export function maxMarginWidth(): number {
  return Math.max(MIN_MARGIN_WIDTH, Math.floor(window.innerWidth * 0.6));
}

export function clampMarginWidth(width: number): number {
  return Math.min(Math.max(width, MIN_MARGIN_WIDTH), maxMarginWidth());
}
