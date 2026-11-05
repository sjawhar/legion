import { expect, test } from "bun:test";

import type { MeasureRecord } from "../measure-reporter";
import { type PartSummary, type Summary, summarise } from "../tools/measure-summary";

let nextLine = 0;

/** A measured row, with every part at 0 but the ones the case names. */
function row(record: Partial<MeasureRecord> & { workerIndex: number }): MeasureRecord {
  nextLine += 1;
  return {
    project: "chromium",
    file: "e2e/smoke.e2e.ts",
    line: nextLine,
    title: `row ${nextLine}`,
    resetSqlMs: 0,
    resetFakesMs: 0,
    signInMs: 0,
    firstPageLoadMs: 0,
    ...record,
  };
}

function partRow(summary: Summary, part: string): PartSummary | undefined {
  return summary.parts.find((candidate) => candidate.part === part);
}

// A10: "a test counts only the parts it runs". A row that opens no page pays no page load, and
// reading its 0 as a measurement would pull the median of the rows that do pay it down.
test("a part a row never runs is left out of that part's median, not counted as a zero", () => {
  const summary = summarise([
    row({ workerIndex: 0, firstPageLoadMs: 100, signInMs: 10 }),
    row({ workerIndex: 0, firstPageLoadMs: 200, signInMs: 20 }),
    row({ workerIndex: 0, firstPageLoadMs: 0, signInMs: 30 }),
    row({ workerIndex: 0, firstPageLoadMs: 300, signInMs: 40 }),
  ]);
  expect(partRow(summary, "first-page-load")).toEqual({
    part: "first-page-load",
    medianMs: 200,
    rows: 3,
  });
  // Four values, so the median is the two middle ones averaged.
  expect(partRow(summary, "sign-in")).toEqual({ part: "sign-in", medianMs: 25, rows: 4 });
  // A part no row pays at all is a part of no rows, not a median of zeros.
  expect(partRow(summary, "reset-fakes")).toEqual({ part: "reset-fakes", medianMs: 0, rows: 0 });
});

// A10: "a part done once per worker is divided over that worker's tests". Each worker pays its
// own once, so each worker's rows carry their own share, not the suite's average.
test("a part one row of a worker pays is divided evenly over all of that worker's rows", () => {
  const records = [
    row({ workerIndex: 0, resetSqlMs: 1000 }),
    ...Array.from({ length: 19 }, () => row({ workerIndex: 0 })),
    row({ workerIndex: 1, resetSqlMs: 2000 }),
    ...Array.from({ length: 19 }, () => row({ workerIndex: 1 })),
  ];
  // 20 shares of 1000/20 and 20 of 2000/20, so the median falls between 50 and 100.
  expect(partRow(summarise(records), "reset-sql")).toEqual({
    part: "reset-sql",
    medianMs: 75,
    rows: 40,
  });
});

// The threshold separates the two rules, so the row at it must read as per-test: two of twenty
// rows paying a part is a part those two ran, not one the worker paid once.
test("a part at the worker-scoped threshold counts only the rows that pay it", () => {
  const records = [
    row({ workerIndex: 0, signInMs: 300 }),
    row({ workerIndex: 0, signInMs: 500 }),
    ...Array.from({ length: 18 }, () => row({ workerIndex: 0 })),
  ];
  expect(partRow(summarise(records), "sign-in")).toEqual({
    part: "sign-in",
    medianMs: 400,
    rows: 2,
  });
});

test("the sum row is the median of each row's own parts, not the sum of the medians", () => {
  const summary = summarise([
    row({ workerIndex: 0, resetSqlMs: 100, resetFakesMs: 10, signInMs: 50, firstPageLoadMs: 300 }),
    row({ workerIndex: 0, resetSqlMs: 200, resetFakesMs: 20, signInMs: 0, firstPageLoadMs: 100 }),
    row({ workerIndex: 0, resetSqlMs: 300, resetFakesMs: 30, signInMs: 150, firstPageLoadMs: 200 }),
  ]);
  const medians = summary.parts.map((part) => part.medianMs);
  expect(medians).toEqual([200, 20, 100, 200]);
  // The rows total 460, 320 and 680; their median is 460, while the medians above sum to 520.
  expect(summary.sum).toEqual({ part: "sum", medianMs: 460, rows: 3 });
});

test("a worker's shared part lands in every one of its rows' own sums", () => {
  const records = [
    row({ workerIndex: 0, resetSqlMs: 400, signInMs: 10 }),
    ...Array.from({ length: 19 }, () => row({ workerIndex: 0, signInMs: 10 })),
  ];
  const summary = summarise(records);
  // Each row carries 400/20 of the shared reset and its own 10 ms sign-in.
  expect(partRow(summary, "reset-sql")).toEqual({ part: "reset-sql", medianMs: 20, rows: 20 });
  expect(summary.sum).toEqual({ part: "sum", medianMs: 30, rows: 20 });
});
