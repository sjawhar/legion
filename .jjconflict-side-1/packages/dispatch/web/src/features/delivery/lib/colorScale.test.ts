import { expect, test } from "bun:test";

import { buildColorScale, COLOR_PALETTE, OTHER_COLOR, PLACEHOLDER_COLOR } from "./colorScale";
import { NO_SESSION } from "./facets";

/** `n` values, busiest first: v0 has `n` PRs, v1 `n - 1`, and so on. */
function counts(n: number): Record<string, number> {
  return Object.fromEntries(Array.from({ length: n }, (_, i) => [`v${i}`, n - i]));
}

test("with nothing chosen, up to ten values take ten different palette colours and none is Other", () => {
  for (let n = 1; n <= COLOR_PALETTE.length; n++) {
    const scale = buildColorScale(counts(n));
    const colours = [...scale.values()];
    expect(new Set(colours).size).toBe(n);
    expect(colours).not.toContain(OTHER_COLOR);
    expect(colours.every((colour) => COLOR_PALETTE.includes(colour))).toBe(true);
  }
});

test("with nothing chosen, an eleventh value repeats the palette as before", () => {
  const scale = buildColorScale(counts(12));
  expect(scale.get("v10")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("v11")).toBe(COLOR_PALETTE[1]);
});

test("chosen values take the palette in the chosen order; the rest are Other and a placeholder stays grey", () => {
  const scale = buildColorScale({ a: 5, b: 3, c: 2, [NO_SESSION]: 4 }, ["b", "a"]);
  expect(scale.get("b")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("c")).toBe(OTHER_COLOR);
  expect(scale.get(NO_SESSION)).toBe(PLACEHOLDER_COLOR);
});

test("a repeated chosen value consumes one palette slot", () => {
  const scale = buildColorScale({ a: 5, b: 3, c: 1 }, ["a", "a", "b"]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[0]);
  expect(scale.get("b")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("c")).toBe(OTHER_COLOR);
});

test("a chosen value the window does not hold takes no colour from the ones it does", () => {
  const scale = buildColorScale({ a: 5, b: 3 }, ["gone", "a"]);
  expect(scale.get("a")).toBe(COLOR_PALETTE[1]);
  expect(scale.get("b")).toBe(OTHER_COLOR);
});
