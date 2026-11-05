import { expect, test } from "bun:test";

import { judge, markersIn, type Row } from "../tools/check-slow-rows";

/** A row of a report, slow unless the case says otherwise. */
function row(file: string, line: number, record: Partial<Row> = {}): Row {
  return {
    file,
    line,
    ms: 2_500,
    project: "chromium",
    title: `row at ${file}:${line}`,
    ...record,
  };
}

const describeSource = `import { expect, test } from "@playwright/test";

// SLOW(>=2s): two contexts and two editors, in both layouts.
test.describe("margin threads", () => {
  test("the first row", async ({ page }) => {
    await page.goto("/");
  });

  test("the second row", async ({ page }) => {
    await page.goto("/");
  });
});

test("a row outside the block", async ({ page }) => {
  await page.goto("/");
});
`;

const loopSource = `import { expect, test } from "@playwright/test";

for (const scheme of ["light", "dark"]) {
  // The scheme is the only difference between the two rows.
  // SLOW(>=2s): a full-page screenshot per scheme.
  test(\`the card reads in \${scheme}\`, async ({ page }) => {
    await page.goto("/");
  });
}
`;

// A5: a marker above a `test.describe(` is the block's, so every row of that block carries it -
// including the second one, which the marker does not sit directly above.
test("a marker above a describe marks every row of that block, and nothing after it", () => {
  const markers = markersIn("margin.e2e.ts", describeSource);
  expect(markers).toEqual([
    {
      file: "margin.e2e.ts",
      kind: "describe",
      line: 3,
      reason: "two contexts and two editors, in both layouts.",
      target: 4,
      targetEnd: 12,
    },
  ]);

  const verdict = judge(
    [row("margin.e2e.ts", 5), row("margin.e2e.ts", 9), row("margin.e2e.ts", 14)],
    markers,
    2_000
  );
  expect(verdict.unmarked.map((slow) => slow.line)).toEqual([14]);
  expect(verdict.stale).toEqual([]);
  expect(verdict.markersUsed).toBe(1);
});

// Every row a loop's one `test(` call generates reports that call's line, so one marker above it
// covers them all - and the comment lines between the marker and the call do not break the
// attribution.
test("a marker above a loop's test( marks every row that call generates", () => {
  const markers = markersIn("task-items.e2e.ts", loopSource);
  expect(markers).toMatchObject([{ kind: "test", line: 5, target: 6, targetEnd: 6 }]);
  // The call builds each row's title, so these rows are found by their line alone.
  expect(markers[0].title).toBeUndefined();

  const verdict = judge(
    [
      row("task-items.e2e.ts", 6, { title: "the card reads in light" }),
      row("task-items.e2e.ts", 6, { title: "the card reads in dark" }),
    ],
    markers,
    2_000
  );
  expect(verdict.unmarked).toEqual([]);
  expect(verdict.rowsSlow).toBe(2);
  expect(verdict.markersUsed).toBe(1);
});

// A report is a snapshot of a tree that has moved on: writing a marker above a row moves that row
// down by the marker's own length, so the line the report holds is no longer the row's. The title
// the call names is the row's either way.
test("a marker finds the row it sits above by title once the line has moved", () => {
  const source = `import { expect, test } from "@playwright/test";

// SLOW(>=2s): a real finger, step by step.
test("a finger drives the board", async ({ page }) => {
  await page.goto("/");
});
`;
  const markers = markersIn("board.e2e.ts", source);
  expect(markers).toMatchObject([{ target: 4, title: "a finger drives the board" }]);

  // The report measured this row at line 3, before the marker above it was written.
  const moved = row("board.e2e.ts", 3, { title: "a finger drives the board" });
  const verdict = judge([moved], markers, 2_000);
  expect(verdict.unmarked).toEqual([]);
  expect(verdict.stale).toEqual([]);
  expect(verdict.markersUsed).toBe(1);

  // A row of another title at that line is another row, and carries no marker of this one's.
  const other = judge([row("board.e2e.ts", 3, { title: "a mouse drags a card" })], markers, 2_000);
  expect(other.unmarked.map((slow) => slow.title)).toEqual(["a mouse drags a card"]);
  expect(other.stale).toEqual([...markers]);
});

// A5's first half: a row that takes the threshold or more and names no reason fails the check,
// whichever project ran it.
test("a slow row with no marker is a failure, and a fast one is not", () => {
  const markers = markersIn("nav.e2e.ts", 'test("a row", async () => {});\n');
  const verdict = judge(
    [row("nav.e2e.ts", 1, { ms: 1_999 }), row("nav.e2e.ts", 1, { ms: 2_000, project: "iphone" })],
    markers,
    2_000
  );
  expect(markers).toEqual([]);
  expect(verdict.unmarked).toEqual([
    { file: "nav.e2e.ts", line: 1, ms: 2_000, project: "iphone", title: "row at nav.e2e.ts:1" },
  ]);
  expect(verdict.rowsRun).toBe(2);
  expect(verdict.rowsSlow).toBe(1);
});

// A5's second half: a comment that calls a row slow when the run shows it fast is a claim the
// next reader believes, so it fails too - but only where that row ran.
test("a marker no slow row used is stale, unless its file never ran", () => {
  const markers = markersIn("margin.e2e.ts", describeSource);
  const ranFast = judge([row("margin.e2e.ts", 5, { ms: 900 })], markers, 2_000);
  expect(ranFast.stale).toEqual([...markers]);
  expect(ranFast.notRun).toEqual([]);
  expect(ranFast.markersUsed).toBe(0);

  const elsewhere = judge([row("nav.e2e.ts", 1)], markers, 2_000);
  expect(elsewhere.stale).toEqual([]);
  expect(elsewhere.notRun).toEqual([...markers]);
  expect(elsewhere.unmarked.map((slow) => slow.file)).toEqual(["nav.e2e.ts"]);
});

// The band under the bar: a row this pod runs at 1.9 s one time and 2.1 s the next is one row,
// and its marker is earned either way. The bar itself does not move - a row inside the band
// still owes no marker - so the band only ever keeps a marker, never demands one.
test("a marker a row earned inside the band is not stale, and the band demands no new markers", () => {
  const markers = markersIn("margin.e2e.ts", describeSource);
  const inBand = judge([row("margin.e2e.ts", 5, { ms: 1_600 })], markers, 2_000);
  expect(inBand.stale).toEqual([]);
  expect(inBand.markersUsed).toBe(1);
  expect(inBand.markersInBand).toBe(1);
  expect(inBand.rowsSlow).toBe(0);
  expect(inBand.unmarked).toEqual([]);

  // A row that took the bar earns its marker as it always did, and is not counted as banded.
  const overBar = judge([row("margin.e2e.ts", 5, { ms: 2_000 })], markers, 2_000);
  expect(overBar.markersUsed).toBe(1);
  expect(overBar.markersInBand).toBe(0);

  // A row well under the band is the lie the stale half exists for, and still fails.
  const wellUnder = judge([row("margin.e2e.ts", 5, { ms: 1_499 })], markers, 2_000);
  expect(wellUnder.stale).toEqual([...markers]);
  expect(wellUnder.markersUsed).toBe(0);
});

// The band is a share of the threshold, so `--slow-ms` moves both halves together: the same
// 1,600 ms row is inside the band of a 2 s bar and well under the band of a 4 s one.
test("the band moves with the threshold", () => {
  const markers = markersIn("margin.e2e.ts", describeSource);
  const rows = [row("margin.e2e.ts", 5, { ms: 1_600 })];
  expect(judge(rows, markers, 2_000).stale).toEqual([]);
  expect(judge(rows, markers, 4_000).stale).toEqual([...markers]);
});
