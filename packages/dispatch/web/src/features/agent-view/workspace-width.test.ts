import { expect, test } from "bun:test";

import {
  COLUMN_WIDTH_STEP,
  clampColumnWidth,
  clampPaneShare,
  DEFAULT_COLUMN_WIDTH,
  fitsTwoPanes,
  MIN_COLUMN_WIDTH,
  MIN_PANE_WIDTH,
  PANE_GAP,
  paneWidths,
} from "./workspace-width";

test("a stored column width is held inside the range the drag allows", () => {
  expect(clampColumnWidth(120)).toBe(200);
  expect(clampColumnWidth(999)).toBe(480);
  expect(clampColumnWidth(240)).toBe(240);
  expect(clampColumnWidth(200)).toBe(200);
  expect(clampColumnWidth(480)).toBe(480);
});

test("a column width that is not a number answers the default", () => {
  // The preference is read from text, so `Number("")` and `Number("wide")` both reach here.
  expect(clampColumnWidth(Number.NaN)).toBe(240);
});

test("stepping the column by one gutter never leaves the range", () => {
  let widening = DEFAULT_COLUMN_WIDTH;
  let narrowing = DEFAULT_COLUMN_WIDTH;
  for (let press = 0; press < 20; press += 1) {
    widening = clampColumnWidth(widening + COLUMN_WIDTH_STEP);
    narrowing = clampColumnWidth(narrowing - COLUMN_WIDTH_STEP);
    expect(widening).toBeGreaterThanOrEqual(200);
    expect(widening).toBeLessThanOrEqual(480);
    expect(narrowing).toBeGreaterThanOrEqual(200);
    expect(narrowing).toBeLessThanOrEqual(480);
  }
  expect(widening).toBe(480);
  expect(narrowing).toBe(200);
  expect(clampColumnWidth(MIN_COLUMN_WIDTH - COLUMN_WIDTH_STEP)).toBe(200);
});

test("two panes need both floors and the gap between them", () => {
  expect(fitsTwoPanes(807)).toBe(true);
  expect(fitsTwoPanes(804)).toBe(true);
  expect(fitsTwoPanes(803)).toBe(false);
  expect(fitsTwoPanes(647)).toBe(false);
});

test("an even split fills the area exactly, both panes readable", () => {
  const { left, right } = paneWidths(807, 0.5);
  expect(left).toBeGreaterThanOrEqual(MIN_PANE_WIDTH);
  expect(right).toBeGreaterThanOrEqual(MIN_PANE_WIDTH);
  expect(left + right + PANE_GAP).toBe(807);
});

test("a share dragged past a pane's floor stops at it and still fills the area", () => {
  for (const share of [0.95, 0.05]) {
    const { left, right } = paneWidths(807, share);
    expect(left).toBeGreaterThanOrEqual(MIN_PANE_WIDTH);
    expect(right).toBeGreaterThanOrEqual(MIN_PANE_WIDTH);
    expect(left + right + PANE_GAP).toBe(807);
  }
});

test("a share that is not a number is read as an even split", () => {
  expect(paneWidths(807, Number.NaN)).toEqual(paneWidths(807, 0.5));
});

test("the splitter reports the share the panes were actually given", () => {
  // At 807 the two floors leave the share barely any room either side of a half, so the extreme
  // is clamped; a 1200 wide area has slack, and a share inside it is reported untouched.
  expect(clampPaneShare(0.95, 807)).toBe(paneWidths(807, 0.95).left / (807 - PANE_GAP));
  expect(clampPaneShare(0.95, 807)).toBeLessThan(0.95);
  expect(clampPaneShare(0.42, 1200)).toBe(paneWidths(1200, 0.42).left / (1200 - PANE_GAP));
  expect(clampPaneShare(0.42, 1200)).toBeCloseTo(0.42, 10);
});

test("at the narrowest width that holds two panes, both sit on their floor", () => {
  expect(paneWidths(804, 0.5)).toEqual({ left: 390, right: 390 });
});
