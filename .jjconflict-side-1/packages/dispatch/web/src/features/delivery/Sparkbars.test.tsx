import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";

import { chartBar } from "../../theme/classes";
import { type SparkbarPoint, Sparkbars } from "./Sparkbars";
import { type SpreadBarDay, SpreadBars } from "./SpreadBars";

afterEach(cleanup);

/** The drawn bars of a chart: every `<rect>` but the transparent hover target each day carries. */
function bars(svg: Element): Element[] {
  return Array.from(svg.querySelectorAll("rect")).filter(
    (rect) => rect.getAttribute("fill") !== "transparent"
  );
}

function titles(svg: Element): string[] {
  return Array.from(svg.querySelectorAll("title")).map((title) => title.textContent ?? "");
}

const atLeast20 = (value: number) => value >= 20;
const asNumber = (value: number) => `${value}/day`;

const points: SparkbarPoint[] = [
  { label: "2026-09-25", value: 25, partial: false },
  { label: "2026-09-26", value: 10, partial: false },
  { label: "2026-09-27", value: null, partial: true },
];

test("Sparkbars draws a bar per day and one dashed target line", () => {
  render(<Sparkbars format={asNumber} max={0} met={atLeast20} points={points} target={20} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });

  expect(svg.getAttribute("viewBox")).toBe("0 0 30 28");
  expect(svg.getAttribute("preserveAspectRatio")).toBe("none");
  expect(bars(svg)).toHaveLength(3);
  const lines = svg.querySelectorAll("line");
  expect(lines).toHaveLength(1);
  const line = lines[0];
  expect(line?.getAttribute("class")).toBe(chartBar.targetLine);
  expect(line?.getAttribute("stroke-dasharray")).toBe("3 2");
  // The scale's top is the largest of max, 1.25 x the target and the tallest bar: 25 here.
  expect(Number(line?.getAttribute("y1"))).toBeCloseTo(28 - (20 / 25) * 28);
});

test("Sparkbars colours a bar that meets the target met and one that misses it missed", () => {
  render(<Sparkbars format={asNumber} max={0} met={atLeast20} points={points} target={20} />);
  const [meets, misses] = bars(screen.getByRole("img", { name: "per-day trend" }));

  expect(meets?.getAttribute("class")).toBe(chartBar.met);
  expect(meets?.getAttribute("height")).toBe("28");
  expect(misses?.getAttribute("class")).toBe(chartBar.missed);
  expect(Number(misses?.getAttribute("height"))).toBeCloseTo((10 / 25) * 28);
});

test("Sparkbars draws a one-pixel stub on a day with nothing to measure", () => {
  render(<Sparkbars format={asNumber} max={0} met={atLeast20} points={points} target={20} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });
  const stub = bars(svg)[2];

  expect(stub?.getAttribute("class")).toBe(chartBar.empty);
  expect(stub?.getAttribute("height")).toBe("1");
  expect(stub?.getAttribute("y")).toBe("27");
  expect(titles(svg)).toEqual([
    "2026-09-25: 25/day",
    "2026-09-26: 10/day",
    "2026-09-27 (partial day): no runs",
  ]);
  expect(svg.querySelectorAll("g")[2]?.getAttribute("opacity")).toBe("0.4");
});

test("Sparkbars without a target draws every bar neutral and no line", () => {
  render(<Sparkbars format={asNumber} max={30} points={points.slice(0, 2)} />);
  const svg = screen.getByRole("img", { name: "per-day trend" });

  expect(bars(svg).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutral,
    chartBar.neutral,
  ]);
  expect(svg.querySelectorAll("line")).toHaveLength(0);
  // With no target the top of the scale is max (30), above the tallest bar (25).
  expect(Number(bars(svg)[0]?.getAttribute("height"))).toBeCloseTo((25 / 30) * 28);
});

const asMinutes = (minutes: number) => `${minutes}m`;

const days: SpreadBarDay[] = [
  { day: "2026-09-25", n: 4, median_minutes: 30, p90_minutes: 40, partial: false },
  { day: "2026-09-26", n: 3, median_minutes: 50, p90_minutes: 80, partial: false },
  { day: "2026-09-27", n: 0, median_minutes: null, p90_minutes: null, partial: true },
];

test("SpreadBars draws the p90 behind the median each day and one dashed target line", () => {
  render(<SpreadBars days={days} format={asMinutes} noun="runs" target={45} />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });

  expect(svg.getAttribute("viewBox")).toBe("0 0 36 36");
  expect(svg.getAttribute("preserveAspectRatio")).toBe("none");
  // Two bars on each measured day and the stub on the empty one.
  expect(bars(svg)).toHaveLength(5);
  const [p90, median] = bars(svg);
  // Drawn later is drawn in front: the p90 first, the median over it.
  expect(Number(p90?.getAttribute("height"))).toBeCloseTo((40 / 80) * 36);
  expect(Number(median?.getAttribute("height"))).toBeCloseTo((30 / 80) * 36);
  const lines = svg.querySelectorAll("line");
  expect(lines).toHaveLength(1);
  expect(lines[0]?.getAttribute("class")).toBe(chartBar.targetLine);
  expect(Number(lines[0]?.getAttribute("y1"))).toBeCloseTo(36 - (45 / 80) * 36);
});

test("SpreadBars colours each bar met under the target and missed at or over it", () => {
  render(<SpreadBars days={days} format={asMinutes} noun="runs" target={45} />);
  const [p90Under, medianUnder, p90Over, medianOver] = bars(
    screen.getByRole("img", { name: "per-day median and p90" })
  );

  expect(p90Under?.getAttribute("class")).toBe(chartBar.metFaint);
  expect(medianUnder?.getAttribute("class")).toBe(chartBar.met);
  expect(p90Over?.getAttribute("class")).toBe(chartBar.missedFaint);
  expect(medianOver?.getAttribute("class")).toBe(chartBar.missed);
});

test("SpreadBars draws a stub on a day with nothing to measure, naming the noun and today", () => {
  render(<SpreadBars days={days} format={asMinutes} noun="runs" target={45} />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });
  const stub = bars(svg)[4];

  expect(stub?.getAttribute("class")).toBe(chartBar.empty);
  expect(stub?.getAttribute("height")).toBe("1");
  expect(stub?.getAttribute("y")).toBe("35");
  expect(titles(svg)).toEqual([
    "2026-09-25: median 30m, p90 40m, 4 runs",
    "2026-09-26: median 50m, p90 80m, 3 runs",
    "2026-09-27 (today so far): no runs",
  ]);
  expect(svg.querySelectorAll("g")[2]?.getAttribute("opacity")).toBe("0.55");
});

test("SpreadBars without a target draws the neutral pair and no line", () => {
  render(<SpreadBars days={days.slice(0, 1)} format={asMinutes} noun="merges" />);
  const svg = screen.getByRole("img", { name: "per-day median and p90" });

  expect(bars(svg).map((bar) => bar.getAttribute("class"))).toEqual([
    chartBar.neutralFaint,
    chartBar.neutral,
  ]);
  expect(svg.querySelectorAll("line")).toHaveLength(0);
  expect(titles(svg)).toEqual(["2026-09-25: median 30m, p90 40m, 4 merges"]);
});
