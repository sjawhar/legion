import { expect, test } from "bun:test";
import { type MeasuredHeights, OVERSCAN_ROWS, rowWindow, withMeasuredHeights } from "./rowWindow";

const ESTIMATE = 40;

/** A list of `count` rows, keyed as the Agents page keys its own: by session id. */
function keys(count: number): string[] {
  return Array.from({ length: count }, (_, index) => `session-${index}`);
}

const windowOver = (
  count: number,
  scrollTop: number,
  viewportHeight: number,
  options: { kept?: ReadonlySet<string>; measured?: MeasuredHeights } = {}
) =>
  rowWindow({
    estimate: ESTIMATE,
    kept: options.kept,
    keys: keys(count),
    measured: options.measured ?? new Map(),
    scrollTop,
    viewportHeight,
  });

test("the window draws the rows the screen holds and twelve either side", () => {
  // Scrolled 40 rows down a 600-row list, with 10 rows' worth of screen under it.
  const list = windowOver(600, 40 * ESTIMATE, 10 * ESTIMATE);

  expect(OVERSCAN_ROWS).toBe(12);
  expect({ first: list.first, last: list.last }).toEqual({ first: 28, last: 62 });
  // 34 rows for a screen that holds 10: the saving R7 measures is that the other 566 are absent.
  expect(list.drawn).toHaveLength(34);
  expect(list.drawn[0]).toBe(28);
  expect(list.drawn.at(-1)).toBe(61);
});

test("the window at the top of the list has no overscan above it and clamps at the end", () => {
  const top = windowOver(20, 0, 10 * ESTIMATE);
  expect({ first: top.first, last: top.last }).toEqual({ first: 0, last: 20 });

  const end = windowOver(600, 599 * ESTIMATE, 10 * ESTIMATE);
  expect({ first: end.first, last: end.last }).toEqual({ first: 587, last: 600 });
  expect(end.drawn.at(-1)).toBe(599);
});

test("a measured row is cached, and the rows below it move by what it measured", () => {
  const measured = withMeasuredHeights(new Map(), [["session-1", 300]]);
  expect(measured.get("session-1")).toBe(300);

  const list = windowOver(5, 0, 10 * ESTIMATE, { measured });
  // Five rows and the list's own height last: the one measured row moves every row below it.
  expect(list.tops).toEqual([0, 40, 340, 380, 420, 460]);

  // Measuring again at the same height is the map it was given, so a page that measures on every
  // render renders once; a new height is a new map.
  expect(withMeasuredHeights(measured, [["session-1", 300]])).toBe(measured);
  expect(withMeasuredHeights(measured, [["session-1", 301]])).not.toBe(measured);
  // A hidden row, and every row in a DOM with no layout, measures zero, which is no measurement:
  // caching it would collapse the row to nothing and move every row below it up.
  expect(withMeasuredHeights(measured, [["session-1", 0]])).toBe(measured);
});

test("a row taller than the rest pushes the window's own range down", () => {
  // The second row is open: a conversation 20 rows tall sits inside it.
  const measured = withMeasuredHeights(new Map(), [["session-1", 20 * ESTIMATE]]);
  const list = windowOver(600, 0, 10 * ESTIMATE, { measured });

  // The screen now holds the first row and part of the open one, so the range ends 12 rows past
  // the open row rather than 12 rows past the tenth.
  expect({ first: list.first, last: list.last }).toEqual({ first: 0, last: 14 });
});

test("the scrollbar measures the whole list, not the rows drawn", () => {
  const measured = withMeasuredHeights(new Map(), [["session-3", 500]]);
  const list = windowOver(600, 0, 10 * ESTIMATE, { measured });

  // 599 rows at the estimate and the measured one: a height read off the drawn rows alone would
  // be a scrollbar that cannot reach the end of the list.
  expect(list.height).toBe(599 * ESTIMATE + 500);
  expect(list.tops.at(-1)).toBe(list.height);
  expect(list.tops[600]).toBe(list.height);
});

test("a row the person has opened stays drawn when it scrolls out of the window", () => {
  const kept = new Set(["session-2"]);
  const list = windowOver(600, 300 * ESTIMATE, 10 * ESTIMATE, { kept });

  expect(list.first).toBe(288);
  expect(list.drawn).toContain(2);
  expect(list.drawn[0]).toBe(2);
  expect(list.drawn[1]).toBe(288);
  // The kept row is the only row drawn outside the range: it costs one row, not a list.
  expect(list.drawn).toHaveLength(34 + 1);
});

test("the range after a scroll to a row far down holds that row", () => {
  const list = windowOver(600, 0, 10 * ESTIMATE);
  expect(list.drawn).not.toContain(450);

  const scrolled = windowOver(600, list.tops[450] ?? 0, 10 * ESTIMATE);
  expect(scrolled.first).toBe(438);
  expect(scrolled.last).toBe(472);
  expect(scrolled.drawn).toContain(450);
});

test("an empty list draws nothing and measures nothing", () => {
  const list = windowOver(0, 0, 800);

  expect(list.drawn).toEqual([]);
  expect({ first: list.first, last: list.last }).toEqual({ first: 0, last: 0 });
  expect(list.height).toBe(0);
});
