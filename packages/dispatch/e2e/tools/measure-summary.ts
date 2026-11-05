/** `bun e2e/tools/measure-summary.ts <records.jsonl> [more.jsonl]`: the per-test cost of the
 *  four parts every row pays before its own work, from the rows one measured run wrote
 *  (`E2E_MEASURE_FILE`, e2e/measure-reporter.ts). One markdown table per input file, so a
 *  merge-base run and a head run read side by side (INFRA-317's A10).
 *
 *    E2E_MEASURE_FILE=/tmp/measure/head.jsonl bun e2e/run.ts -- bunx playwright test \
 *      --config e2e/playwright.config.ts --project=chromium e2e/smoke.e2e.ts
 *    bun e2e/tools/measure-summary.ts /tmp/measure/base.jsonl /tmp/measure/head.jsonl
 *
 *  A10 counts a part only for the rows that run it, and divides a part paid once per worker over
 *  that worker's rows; `summarise` is both rules and no I/O, so its arithmetic is unit-tested
 *  (e2e/__tests__/measure-summary.test.ts). A file it cannot read, or one with no records, is
 *  refused rather than summarised as zero: every figure of A10 is read off this table.
 */
import type { MeasureRecord } from "../measure-reporter";

const usage =
  "usage: bun e2e/tools/measure-summary.ts <records.jsonl> [more.jsonl ...]\n" +
  "  <records.jsonl>  the rows one measured run wrote (E2E_MEASURE_FILE)";

type MeasuredMs = "resetSqlMs" | "resetFakesMs" | "signInMs" | "firstPageLoadMs";

interface Part {
  readonly part: string;
  readonly field: MeasuredMs;
}

/** The table's rows, in the order A10 names the parts. */
const parts: readonly Part[] = [
  { part: "reset-sql", field: "resetSqlMs" },
  { part: "reset-fakes", field: "resetFakesMs" },
  { part: "sign-in", field: "signInMs" },
  { part: "first-page-load", field: "firstPageLoadMs" },
];

/** What separates a part every row of a worker pays from one that worker pays once. Under this
 *  share of the worker's rows, the part is read as paid once for the worker (a fixture a suite
 *  sets up in its first row and the rest inherit) and divided evenly over all of them, so each
 *  row carries the share it owes; at or above it, the part is paid per row and a row that never
 *  pays it counts nothing rather than a zero. */
const WORKER_SCOPED_SHARE = 0.1;

export interface PartSummary {
  part: string;
  medianMs: number;
  /** The rows that count this part: every row of a worker for a worker-scoped part. */
  rows: number;
}

export interface Summary {
  parts: PartSummary[];
  /** The median of each row's own summed parts, which is not the sum of the medians above. */
  sum: PartSummary;
}

function median(values: readonly number[]): number {
  if (values.length === 0) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2;
}

/** What each row counts of one part: its own time, the share of a worker-scoped part it owes, or
 *  undefined for a row that does not count the part at all. */
function shares(records: readonly MeasureRecord[], field: MeasuredMs): (number | undefined)[] {
  const byWorker = new Map<number, number[]>();
  records.forEach((record, index) => {
    const rows = byWorker.get(record.workerIndex);
    if (rows === undefined) byWorker.set(record.workerIndex, [index]);
    else rows.push(index);
  });
  const counted: (number | undefined)[] = records.map(() => undefined);
  for (const rows of byWorker.values()) {
    const paid = rows.filter((index) => records[index][field] > 0);
    if (paid.length === 0) continue;
    if (paid.length < rows.length * WORKER_SCOPED_SHARE) {
      const total = paid.reduce((sum, index) => sum + records[index][field], 0);
      const share = total / rows.length;
      for (const index of rows) counted[index] = share;
      continue;
    }
    for (const index of paid) counted[index] = records[index][field];
  }
  return counted;
}

export function summarise(records: readonly MeasureRecord[]): Summary {
  const rowTotals = records.map(() => 0);
  const rowCounts = records.map(() => false);
  const summaries = parts.map(({ part, field }) => {
    const values: number[] = [];
    shares(records, field).forEach((ms, index) => {
      if (ms === undefined) return;
      values.push(ms);
      rowTotals[index] += ms;
      rowCounts[index] = true;
    });
    return { part, medianMs: median(values), rows: values.length };
  });
  const totals = rowTotals.filter((_, index) => rowCounts[index]);
  return {
    parts: summaries,
    sum: { part: "sum", medianMs: median(totals), rows: totals.length },
  };
}

export function markdownTable(summary: Summary): string {
  const lines = ["| part | median ms | rows |", "| --- | ---: | ---: |"];
  for (const row of [...summary.parts, summary.sum]) {
    lines.push(`| ${row.part} | ${row.medianMs.toFixed(1)} | ${row.rows} |`);
  }
  lines.push(
    "",
    "The `sum` row is the median of each row's own summed parts, not the sum of the medians above."
  );
  return lines.join("\n");
}

function parseRecords(text: string, file: string): MeasureRecord[] {
  const records: MeasureRecord[] = [];
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    if (lines[i].trim() === "") continue;
    try {
      records.push(JSON.parse(lines[i]) as MeasureRecord);
    } catch (error) {
      console.error(
        `measure-summary: ${file}:${i + 1} is not a record: ` +
          (error instanceof Error ? error.message : String(error))
      );
      process.exit(1);
    }
  }
  return records;
}

if (import.meta.main) {
  const files = process.argv.slice(2);
  if (files.length === 0) {
    console.error(usage);
    process.exit(2);
  }
  const tables: string[] = [];
  for (const file of files) {
    let text: string;
    try {
      text = await Bun.file(file).text();
    } catch (error) {
      console.error(
        `measure-summary: cannot read ${file}: ` +
          (error instanceof Error ? error.message : String(error))
      );
      console.error(usage);
      process.exit(2);
    }
    const records = parseRecords(text, file);
    if (records.length === 0) {
      console.error(`measure-summary: ${file} holds no records, so there is nothing to summarise`);
      process.exit(1);
    }
    tables.push(`## ${file}\n\n${markdownTable(summarise(records))}`);
  }
  process.stdout.write(`${tables.join("\n\n")}\n`);
}
