/**
 * Windowing for a list whose rows are not all the same height: the Agents page's rows, each of
 * which expands into a conversation (`features/agents/AgentsPage.tsx`). Its neighbour
 * `prList.ts` windows the delivery table, where every row is 36 px inside a `<table>` and the
 * rows on screen are arithmetic with nothing measured; a row that grows a conversation inside it
 * cannot be counted that way. Here a row is as tall as the page last measured it, and a row
 * nothing has measured yet is taken to be `estimate` tall, so the list has a length - and a
 * scrollbar - before a single row is drawn. Pure: no React. The caller draws the rows this
 * answers with, measures them, and hands the heights back (`withMeasuredHeights`).
 *
 * Two rules are INFRA-812's D1. The list draws the rows the screen holds and `OVERSCAN_ROWS`
 * either side, and it draws every row the person has opened wherever that row has scrolled to:
 * an open row holds its reply target, its unread set and its composer's draft, which a scroll
 * must no more throw away than a collapse does. The height this answers is the whole list's,
 * measured rows and estimates together, so the scrollbar measures the list rather than the part
 * of it that is drawn.
 */

/** Rows drawn past each edge of the window, as the uniform list draws (`prList.ts`). */
export const OVERSCAN_ROWS = 12;

/** What the list measured each row to be, by the key the list names that row with. */
export type MeasuredHeights = ReadonlyMap<string, number>;

export interface RowWindow {
  /** Where each row starts, in pixels from the list's own top, with the list's whole height as
   *  a last entry: one more than there are rows. */
  readonly tops: readonly number[];
  /** The whole list's height, drawn rows and undrawn alike: what its scrollbar measures. */
  readonly height: number;
  /** The rows the screen holds and the overscan either side, as `[first, last)`. */
  readonly first: number;
  readonly last: number;
  /** Every row the list draws, ascending: that range, and each kept row outside it. */
  readonly drawn: readonly number[];
}

/** The row `offset` falls in: the last one starting at or before it, and row 0 above the list. */
function rowCovering(tops: readonly number[], rowCount: number, offset: number): number {
  let low = 0;
  let high = rowCount - 1;
  while (low < high) {
    const middle = Math.ceil((low + high) / 2);
    if ((tops[middle] ?? 0) <= offset) low = middle;
    else high = middle - 1;
  }
  return Math.max(0, low);
}

/** The first row starting at or after `offset`, and `rowCount` when the list ends above it. */
function rowFrom(tops: readonly number[], rowCount: number, offset: number): number {
  let low = 0;
  let high = rowCount;
  while (low < high) {
    const middle = (low + high) >> 1;
    if ((tops[middle] ?? 0) < offset) low = middle + 1;
    else high = middle;
  }
  return low;
}

/**
 * The window over `keys` - the list's rows in the order it draws them, each named by a key its
 * height is cached under - for a list scrolled to `scrollTop` under a viewport `viewportHeight`
 * tall. `kept` names the rows drawn wherever they have scrolled to, which on the Agents page is
 * every row the person has opened (D1); `overscan` is how many rows are drawn past each edge.
 */
export function rowWindow({
  estimate,
  kept,
  keys,
  measured,
  overscan = OVERSCAN_ROWS,
  scrollTop,
  viewportHeight,
}: {
  keys: readonly string[];
  measured: MeasuredHeights;
  /** How tall a row nothing has measured is taken to be. */
  estimate: number;
  /** How far the list has scrolled under the viewport's top edge, never below zero. */
  scrollTop: number;
  viewportHeight: number;
  kept?: ReadonlySet<string>;
  overscan?: number;
}): RowWindow {
  const rowCount = keys.length;
  const tops: number[] = new Array<number>(rowCount + 1);
  let top = 0;
  for (let index = 0; index < rowCount; index += 1) {
    tops[index] = top;
    top += measured.get(keys[index] ?? "") ?? estimate;
  }
  tops[rowCount] = top;
  const first = Math.max(0, rowCovering(tops, rowCount, scrollTop) - overscan);
  const last = Math.min(rowCount, rowFrom(tops, rowCount, scrollTop + viewportHeight) + overscan);
  const drawn: number[] = [];
  for (let index = 0; index < rowCount; index += 1) {
    const inRange = index >= first && index < last;
    if (inRange || kept?.has(keys[index] ?? "") === true) drawn.push(index);
  }
  return { drawn, first, height: top, last, tops };
}

/**
 * The heights after measuring the rows `heights` names, or the map handed in when none of them
 * changed, so a round of measuring that confirms the cache costs no render.
 *
 * A height of zero is no measurement, and keeps whatever the row last measured: a row the list
 * keeps mounted and hidden measures nothing, and so does every row in a unit test, where the DOM
 * has no layout at all and each row stands at `estimate`.
 */
export function withMeasuredHeights(
  measured: MeasuredHeights,
  heights: Iterable<readonly [string, number]>
): MeasuredHeights {
  let next: Map<string, number> | null = null;
  for (const [key, height] of heights) {
    if (height <= 0 || measured.get(key) === height) continue;
    next ??= new Map(measured);
    next.set(key, height);
  }
  return next ?? measured;
}
