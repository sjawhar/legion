import { expect, test } from "bun:test";

import { waitingSeries } from "./waiting";

const window = { start: "2024-06-01T00:00:00Z", end: "2024-06-02T00:00:00Z" };

test("a merge still waiting at the window's start starts the line at 1, and its deploy brings it down", () => {
  const points = waitingSeries(
    [{ merged_at: "2024-05-03T00:00:00Z", deployed_at: "2024-06-01T06:00:00Z" }],
    window
  );
  expect(points).toEqual([
    { at: window.start, count: 1 },
    { at: "2024-06-01T06:00:00Z", count: 0 },
    { at: window.end, count: 0 },
  ]);
});

test("a merge that shipped before the window's start never raises the line", () => {
  const points = waitingSeries(
    [{ merged_at: "2024-05-03T00:00:00Z", deployed_at: "2024-05-04T00:00:00Z" }],
    window
  );
  expect(points.map((point) => point.count)).toEqual([0, 0]);
});

test("a merge after the window's end is not counted inside it", () => {
  const points = waitingSeries([{ merged_at: "2024-06-03T00:00:00Z", deployed_at: null }], window);
  expect(points.map((point) => point.count)).toEqual([0, 0]);
});

test("the window's end is compared as an instant, so a differently spelled end adds no point", () => {
  const points = waitingSeries(
    [{ merged_at: "2024-06-01T12:00:00Z", deployed_at: "2024-06-02T00:00:00.000Z" }],
    { start: window.start, end: "2024-06-02T00:00:00Z" }
  );
  expect(points).toEqual([
    { at: window.start, count: 0 },
    { at: "2024-06-01T12:00:00Z", count: 1 },
    { at: "2024-06-02T00:00:00.000Z", count: 0 },
  ]);
});
