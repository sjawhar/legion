import { afterEach, expect, mock, test } from "bun:test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

import {
  Chart,
  type ChartCompare,
  type ChartRange,
  type ChartSeries,
  precedingRange,
} from "./Chart";

afterEach(cleanup);

/** The 28 days the measures panel's fixture window covers, as the answer resolves them. */
const WINDOW: ChartRange = {
  kind: "custom",
  from: "2026-09-01T00:00:00.000Z",
  to: "2026-09-29T00:00:00.000Z",
};

/** Three days of a window, drawn as one run of bars; the last has nothing to measure. */
const deploys: ChartSeries = {
  kind: "spark",
  format: (v) => `${v} deploys`,
  label: "Deploys",
  max: 4,
  met: (v) => v >= 4,
  points: [
    { label: "2026-09-26", value: 5, partial: false },
    { label: "2026-09-27", value: 2, partial: false },
    { label: "2026-09-28", value: null, partial: true },
  ],
  target: 4,
};

/** The same three days as a median-and-p90 spread. */
const runTime: ChartSeries = {
  kind: "spread",
  days: [
    { day: "2026-09-26", n: 4, median_minutes: 30, p90_minutes: 40, partial: false },
    { day: "2026-09-27", n: 3, median_minutes: 50, p90_minutes: 80, partial: false },
    { day: "2026-09-28", n: 0, median_minutes: null, p90_minutes: null, partial: true },
  ],
  format: (v) => `${v}m`,
  label: "Run time",
  noun: "runs",
};

/** The three days of the range before, as the compare read answers them. */
const earlierDeploys: ChartSeries = {
  ...deploys,
  points: [
    { label: "2026-08-29", value: 3, partial: false },
    { label: "2026-08-30", value: 2, partial: false },
    { label: "2026-08-31", value: 1, partial: false },
  ],
} as ChartSeries;

/** Two issues of a window, as the Legion section's work-time card draws them: a row is an issue,
 *  not a day, so two ranges hold different rows in the same positions. */
const workTime: ChartSeries = {
  kind: "spark",
  format: (v) => `${v}h`,
  label: "Work time",
  max: 9,
  points: [
    { label: "INFRA-1", value: 5, partial: false },
    { label: "INFRA-2", value: 2, partial: false },
  ],
};

/** The compare range's own issues: one the window holds too, and one it does not. */
const earlierWorkTime: ChartSeries = {
  ...workTime,
  points: [
    { label: "INFRA-2", value: 4, partial: false },
    { label: "INFRA-9", value: 9, partial: false },
  ],
} as ChartSeries;

function rows(): string[][] {
  const table = screen.getByRole("table");
  return within(table)
    .getAllByRole("row")
    .map((row) => Array.from(row.querySelectorAll("th, td"), (cell) => cell.textContent ?? ""));
}

function showTable(label: string): void {
  fireEvent.click(
    within(screen.getByRole("group", { name: `${label} view` })).getByRole("button", {
      name: "Table",
    })
  );
}

test("the chart draws its series as bars and names both controls", () => {
  render(
    <Chart
      compare={{ onRange: mock(), range: null, series: undefined }}
      label="PR CI"
      range={WINDOW}
      series={[runTime, deploys]}
    />
  );

  expect(screen.getByRole("img", { name: "per-day median and p90" })).toBeDefined();
  expect(screen.getByRole("img", { name: "per-day trend" })).toBeDefined();
  expect(screen.queryByRole("table")).toBeNull();
  const view = screen.getByRole("group", { name: "PR CI view" });
  expect(within(view).getByRole("button", { name: "Graph" }).getAttribute("aria-pressed")).toBe(
    "true"
  );
  expect(screen.getByLabelText("PR CI comparison")).toBeDefined();
});

test("the table holds every number the graph draws, each day a row", () => {
  render(<Chart label="PR CI" range={WINDOW} series={[runTime, deploys]} />);
  showTable("PR CI");

  expect(rows()).toEqual([
    ["Day", "Run time median", "Run time p90", "Runs", "Deploys"],
    ["2026-09-26", "30m", "40m", "4", "5 deploys"],
    ["2026-09-27", "50m", "80m", "3", "2 deploys"],
    ["2026-09-28 (partial)", "\u2014", "\u2014", "0", "\u2014"],
  ]);
  // The target the graph draws as its dashed line is named too.
  expect(screen.getByRole("table").querySelector("caption")?.textContent).toBe(
    "PR CI by UTC day. Deploys target 4 deploys. \u2014 is a day with nothing measured."
  );
});

test("a day with nothing measured reads as nothing measured, never as zero", () => {
  render(<Chart label="Deploys a day" range={WINDOW} series={[deploys]} />);
  showTable("Deploys a day");

  const empty = rows()[3];
  expect(empty).toEqual(["2026-09-28 (partial)", "\u2014"]);
  expect(empty?.[1]).not.toBe("0 deploys");
});

test("the default compare range is the one immediately before the window, of the same length", () => {
  const onRange = mock();
  render(
    <Chart
      compare={{ onRange, range: null, series: undefined }}
      label="Deploys a day"
      range={WINDOW}
      series={[deploys]}
    />
  );
  const control = screen.getByLabelText("Deploys a day comparison");
  expect(Array.from(control.querySelectorAll("option"), (option) => option.textContent)).toEqual([
    "No comparison",
    "Previous 28 days",
    "Custom range…",
  ]);

  fireEvent.change(control, { target: { value: "preceding" } });

  // The 28 days ending where the window begins: 2026-08-04 to 2026-09-01.
  expect(onRange).toHaveBeenCalledWith({
    kind: "custom",
    from: "2026-08-04T00:00:00.000Z",
    to: "2026-09-01T00:00:00.000Z",
  });
  expect(precedingRange(WINDOW)).toEqual({
    kind: "custom",
    from: "2026-08-04T00:00:00.000Z",
    to: "2026-09-01T00:00:00.000Z",
  });
});

test("a compare table puts the two ranges side by side with the change between them", () => {
  const compare: ChartCompare = {
    onRange: mock(),
    range: precedingRange(WINDOW),
    series: [earlierDeploys],
  };
  render(<Chart compare={compare} label="Deploys a day" range={WINDOW} series={[deploys]} />);
  showTable("Deploys a day");

  expect(rows()).toEqual([
    ["Day", "Compare day", "Deploys", "Deploys compare", "Deploys change"],
    ["2026-09-26", "2026-08-29", "5 deploys", "3 deploys", "+2 deploys"],
    ["2026-09-27", "2026-08-30", "2 deploys", "2 deploys", "0 deploys"],
    ["2026-09-28 (partial)", "2026-08-31", "\u2014", "1 deploys", "\u2014"],
  ]);
  expect(screen.getByRole("table").querySelector("caption")?.textContent).toContain(
    "compared with 2026-08-04 to 2026-08-31"
  );
});

test("rows a chart names, rather than days, pair with the compare range by that name", () => {
  const compare: ChartCompare = {
    onRange: mock(),
    range: precedingRange(WINDOW),
    series: [earlierWorkTime],
  };
  render(
    <Chart
      compare={compare}
      label="Work time to merge"
      range={WINDOW}
      rows={{ header: "Issue", over: "issue", pair: "name" }}
      series={[workTime]}
    />
  );
  showTable("Work time to merge");

  // Row 1 of each range is a different issue, so the window's rows pair with the compare range's
  // by key: the issue both ranges hold shows its change, and an issue only one of them holds
  // has nothing on the other side rather than another issue's number.
  expect(rows()).toEqual([
    ["Issue", "Compare issue", "Work time", "Work time compare", "Work time change"],
    ["INFRA-1", "\u2014", "5h", "\u2014", "\u2014"],
    ["INFRA-2", "INFRA-2", "2h", "4h", "-2h"],
    ["\u2014", "INFRA-9", "\u2014", "9h", "\u2014"],
  ]);
});

test("a rolling window, whose end is the present, is named to the day its last bar draws", () => {
  // The 28-day preset the server resolved at 20:00: its last day is the partial one on screen.
  const rolling: ChartRange = {
    kind: "custom",
    from: "2026-08-31T20:00:00Z",
    to: "2026-09-28T20:00:00Z",
  };
  render(
    <Chart
      compare={{ onRange: mock(), range: precedingRange(rolling), series: undefined }}
      label="Deploys a day"
      range={rolling}
      series={[deploys]}
    />
  );

  expect(
    document.querySelector('[data-chart-range="window"]')?.firstElementChild?.textContent
  ).toBe("2026-08-31 to 2026-09-28");
  expect(
    document.querySelector('[data-chart-range="compare"]')?.firstElementChild?.textContent
  ).toBe("Compare 2026-08-03 to 2026-08-31");
});

test("a compare draws both ranges, each under the days it covers", () => {
  const compare: ChartCompare = {
    onRange: mock(),
    range: precedingRange(WINDOW),
    series: [earlierDeploys],
  };
  render(<Chart compare={compare} label="Deploys a day" range={WINDOW} series={[deploys]} />);

  const drawn = Array.from(
    document.querySelectorAll("[data-chart-range]"),
    (one) => `${one.getAttribute("data-chart-range")}: ${one.firstElementChild?.textContent}`
  );
  expect(drawn).toEqual([
    "window: 2026-09-01 to 2026-09-28",
    "compare: Compare 2026-08-04 to 2026-08-31",
  ]);
  expect(screen.getAllByRole("img", { name: "per-day trend" })).toHaveLength(2);
});

test("while the compare range's read is out its numbers are pending, not nothing measured", () => {
  const compare: ChartCompare = {
    onRange: mock(),
    range: precedingRange(WINDOW),
    series: undefined,
  };
  render(<Chart compare={compare} label="Deploys a day" range={WINDOW} series={[deploys]} />);
  showTable("Deploys a day");

  expect(rows()[1]).toEqual(["2026-09-26", "\u2026", "5 deploys", "\u2026", "\u2026"]);
});

test("the reader names a compare range of their own, and an overlong one is refused here", () => {
  const onRange = mock();
  render(
    <Chart
      compare={{ onRange, range: null, series: undefined }}
      label="Deploys a day"
      range={WINDOW}
      series={[deploys]}
    />
  );
  fireEvent.change(screen.getByLabelText("Deploys a day comparison"), {
    target: { value: "custom" },
  });
  fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-01-01" } });
  fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-06-30" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));

  expect(screen.getByRole("alert").textContent).toBe(
    "The window can span at most 84 days; this range spans 181."
  );
  expect(onRange).not.toHaveBeenCalled();

  fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-01-28" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));

  expect(onRange).toHaveBeenCalledWith({
    kind: "custom",
    from: "2026-01-01T00:00:00.000Z",
    to: "2026-01-29T00:00:00.000Z",
  });
});
