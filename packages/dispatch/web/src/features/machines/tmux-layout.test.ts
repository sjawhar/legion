import { expect, test } from "bun:test";
import { TMUX_LAYOUT_MAX } from "@legion/contracts";

import { parseTmuxLayout, type TmuxLayoutPane } from "./tmux-layout";

// Recorded from tmux 3.5a, each the window's own `window_layout`.
/** One pane filling an 80x24 window. */
const ONE_PANE = "b262,80x24,0,0,5";
/** `%0` over `%1` beside `%2`, in an 80x24 window. */
const THREE_PANES_SMALL = "b05b,80x24,0,0[80x12,0,0,0,80x11,0,13{40x11,0,13,1,39x11,41,13,2}]";
/** The same three panes in a 200x50 window. */
const THREE_PANES_LARGE = "81f0,200x50,0,0[200x25,0,0,0,200x24,0,26{100x24,0,26,1,99x24,101,26,2}]";
/** What that window reports while `%1` is zoomed: the one pane that is on the screen. */
const ZOOMED = "ac9e,200x50,0,0,1";

interface Proportion {
  readonly paneId: string;
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

/** Each pane's box as a fraction of the window, which is what a drawing scales to its own box. */
function proportions(layout: string): Proportion[] {
  const parsed = parseTmuxLayout(layout);
  if (parsed === undefined) {
    throw new Error(`layout did not parse: ${layout}`);
  }
  const { root } = parsed;
  return parsed.panes.map((pane) => ({
    height: pane.height / root.height,
    paneId: pane.paneId,
    width: pane.width / root.width,
    x: pane.x / root.width,
    y: pane.y / root.height,
  }));
}

/**
 * Two windows of different sizes cannot hold one shape exactly: the divider is one cell whatever
 * the window's size, so it is 1/24 of the small window's height and 1/50 of the large one's, and
 * every difference between the two layouts below is exactly that share — 1/24 − 1/50 = 0.0217 of
 * the height, 1/80 − 1/200 = 0.0075 of the width. The check is therefore one percent of the
 * window on top of the divider's own share.
 */
const SAME_SHAPE = 0.01 + (1 / 24 - 1 / 50);

test("a window of one pane reads as that pane filling the window", () => {
  const pane: TmuxLayoutPane = { height: 24, kind: "pane", paneId: "%5", width: 80, x: 0, y: 0 };

  expect(parseTmuxLayout(ONE_PANE)).toEqual({ panes: [pane], root: pane });
});

test("a split window reads as its tree of boxes, each pane keeping tmux's `%` on its id", () => {
  const layout = parseTmuxLayout(THREE_PANES_SMALL);

  expect(layout?.root).toEqual({
    children: [
      { height: 12, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
      {
        children: [
          { height: 11, kind: "pane", paneId: "%1", width: 40, x: 0, y: 13 },
          { height: 11, kind: "pane", paneId: "%2", width: 39, x: 41, y: 13 },
        ],
        height: 11,
        into: "columns",
        kind: "split",
        width: 80,
        x: 0,
        y: 13,
      },
    ],
    height: 24,
    into: "rows",
    kind: "split",
    width: 80,
    x: 0,
    y: 0,
  });
  expect(layout?.panes.map((pane) => pane.paneId)).toEqual(["%0", "%1", "%2"]);
});

test("the divider tmux counts between two panes is left as tmux counted it", () => {
  const panes = parseTmuxLayout(THREE_PANES_SMALL)?.panes ?? [];
  const [top, left, right] = panes;

  // 40 + 1 + 39 is the window's 80, and 12 + 1 + 11 its 24: the missing cell is the divider, and
  // the parser neither adds it to a pane nor reports a gap of its own.
  expect(left.width + 1 + right.width).toBe(80);
  expect(top.height + 1 + left.height).toBe(24);
  expect(right.x).toBe(left.x + left.width + 1);
});

test("the same window at another size reads as the same shape", () => {
  const small = proportions(THREE_PANES_SMALL);
  const large = proportions(THREE_PANES_LARGE);

  expect(large.map((pane) => pane.paneId)).toEqual(small.map((pane) => pane.paneId));
  for (const [index, pane] of small.entries()) {
    const other = large[index];
    expect(Math.abs(pane.x - other.x)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.y - other.y)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.width - other.width)).toBeLessThanOrEqual(SAME_SHAPE);
    expect(Math.abs(pane.height - other.height)).toBeLessThanOrEqual(SAME_SHAPE);
  }
  // Where the window is split is the same in both, to the cell: half the height, half the width.
  expect([small[0].height, large[0].height]).toEqual([0.5, 0.5]);
  expect([small[1].width, large[1].width]).toEqual([0.5, 0.5]);
});

test("a zoomed window reads as the one pane that is on the screen", () => {
  expect(parseTmuxLayout(ZOOMED)?.panes).toEqual([
    { height: 50, kind: "pane", paneId: "%1", width: 200, x: 0, y: 0 },
  ]);
});

test("an unbalanced bracket is no layout", () => {
  expect(parseTmuxLayout("b05b,80x24,0,0[80x12,0,0,0,80x11,0,13")).toBeUndefined();
  expect(parseTmuxLayout("b05b,80x24,0,0[80x12,0,0,0,80x11,0,13}")).toBeUndefined();
});

test("a cell that is neither a pane nor a split is no layout", () => {
  expect(parseTmuxLayout("b262,80x24,0,0")).toBeUndefined();
  expect(parseTmuxLayout("b262,80x24,0,0,")).toBeUndefined();
});

test("a size that is not a number is no layout", () => {
  expect(parseTmuxLayout("b262,80xtall,0,0,5")).toBeUndefined();
  expect(parseTmuxLayout("b262,eightyx24,0,0,5")).toBeUndefined();
});

test("the empty string is no layout", () => {
  expect(parseTmuxLayout("")).toBeUndefined();
});

test("a string without tmux's checksum, or with anything after the root cell, is no layout", () => {
  expect(parseTmuxLayout("80x24,0,0,5")).toBeUndefined();
  expect(parseTmuxLayout("b262,80x24,0,0,5]")).toBeUndefined();
});

/** A layout of `levels` splits, each holding the next, with one pane at the bottom: eleven
 *  characters a level, which is how cheaply a sender can buy a level of recursion in the
 *  parser. */
function nested(levels: number): string {
  return `b262,${"80x24,0,0[".repeat(levels)}80x24,0,0,0${"]".repeat(levels)}`;
}

test("a layout past the cap the sender's own client holds to is no layout, however it nests", () => {
  const inside = nested(180);
  const past = nested(181);
  expect(inside.length).toBeLessThanOrEqual(TMUX_LAYOUT_MAX);
  expect(past.length).toBeGreaterThan(TMUX_LAYOUT_MAX);

  // The two strings differ by one split, so what refuses the longer one is its length and not
  // its shape: a page draws a window from whatever a session registered, and nothing between
  // the sender and the page applies this cap on the parser's behalf.
  expect(parseTmuxLayout(inside)?.panes).toEqual([
    { height: 24, kind: "pane", paneId: "%0", width: 80, x: 0, y: 0 },
  ]);
  expect(parseTmuxLayout(past)).toBeUndefined();
});
