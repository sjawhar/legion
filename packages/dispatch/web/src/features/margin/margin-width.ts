/** How wide the margin column may be. Its own module because the shell reads the default while
 *  `features/margin/Margin.tsx` is still loading: the margin's code is fetched after the first
 *  paint, and the width the shell restores from the reader's preference must not wait for it. */

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
