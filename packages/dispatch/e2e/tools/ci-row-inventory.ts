/** `bun e2e/tools/ci-row-inventory.ts <job.log> [--slow-ms 2000]`: the per-row inventory of one
 *  `dispatch` CI job, from the log of that job.
 *
 *  Playwright's `list` reporter writes one line per row that ran:
 *
 *    ✓  1 [chromium] › e2e/activity.e2e.ts:32:1 › the Activity tab lists … (1.4s)
 *    -  2 [webkit] › e2e/doc.e2e.ts:20:1 › … (the dash is a skipped row, which has no time)
 *
 *  GitHub prefixes every line with its timestamp, so the parser anchors on the reporter's own
 *  index and project rather than on the line's start. The output is a TSV of
 *  `index project file line col title ms status`, then a reconciliation on stderr: the parsed
 *  row count against the log's own `Running N tests using M worker(s)` line and its
 *  `N passed (Tm)` / `N skipped` summary, and the parsed sum of row times against the summary's
 *  wall time. A mismatch in the counts exits 1, because every later figure of INFRA-317 is read
 *  against this inventory and a parser that silently drops rows would move them all.
 *
 *  `--slow-ms` sets the threshold the `>= slow` figures use (the spec's own is 2000 ms).
 *  Usage, with the log fetched as the deployment instructions say:
 *
 *    gh api repos/sjawhar/legion/actions/jobs/<job>/logs --allow-escape-sequences \
 *      | sed 's/\x1b\[[0-9;]*m//g' > base.log
 *    bun e2e/tools/ci-row-inventory.ts base.log > rows.tsv
 */
import { parseArgs } from "node:util";

const usage =
  "usage: bun e2e/tools/ci-row-inventory.ts <job.log> [--slow-ms <n>]\n" +
  "  <job.log>  the dispatch job's log, with its terminal escape sequences stripped";

export interface Row {
  readonly index: number;
  readonly project: string;
  readonly file: string;
  readonly line: number;
  readonly column: number;
  readonly title: string;
  /** The row's own time; 0 for a skipped row, which the reporter gives no time. */
  readonly ms: number;
  readonly status: "passed" | "skipped";
}

/** `(1.4s)`, `(919ms)`, `(1.2m)` as the list reporter writes them, in milliseconds. */
export function durationMs(text: string): number | undefined {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m)$/.exec(text);
  if (match === null) return undefined;
  const value = Number(match[1]);
  return match[2] === "ms" ? value : match[2] === "s" ? value * 1000 : value * 60_000;
}

const rowPattern =
  /(?<mark>✓|✘|-|×|±)\s+(?<index>\d+)\s+\[(?<project>[^\]]+)\]\s+›\s+(?<path>[^\s›]+):(?<line>\d+):(?<column>\d+)\s+›\s+(?<rest>.*)$/u;

/** One row of the reporter's output, or undefined for a line that is not one. */
export function parseRow(text: string): Row | undefined {
  const match = rowPattern.exec(text);
  if (match?.groups === undefined) return undefined;
  const { mark, index, project, path, line, column, rest } = match.groups;
  // The title runs to the end of the line, less the trailing `(1.4s)` a row that ran carries.
  const timed = /^(?<title>.*?)\s+\((?<duration>\d+(?:\.\d+)?(?:ms|s|m))\)\s*$/u.exec(rest);
  const title = (timed?.groups?.title ?? rest).trim();
  const ms = timed?.groups?.duration === undefined ? 0 : (durationMs(timed.groups.duration) ?? 0);
  if (title === "") return undefined;
  return {
    column: Number(column),
    file: path,
    index: Number(index),
    line: Number(line),
    ms,
    project,
    status: mark === "-" ? "skipped" : "passed",
    title,
  };
}

export interface Summary {
  /** `Running N tests using M workers`. */
  readonly listed?: number;
  readonly workers?: number;
  readonly passed?: number;
  readonly skipped?: number;
  readonly failed?: number;
  /** The wall time of `N passed (59.2m)`, in milliseconds. */
  readonly wallMs?: number;
}

export function parseSummary(lines: readonly string[]): Summary {
  const summary: {
    listed?: number;
    workers?: number;
    passed?: number;
    skipped?: number;
    failed?: number;
    wallMs?: number;
  } = {};
  for (const line of lines) {
    const running = /Running (\d+) tests? using (\d+) workers?/.exec(line);
    if (running !== null) {
      summary.listed = Number(running[1]);
      summary.workers = Number(running[2]);
    }
    const passed = /(\d+) passed \((\d+(?:\.\d+)?(?:ms|s|m))\)/.exec(line);
    if (passed !== null) {
      summary.passed = Number(passed[1]);
      summary.wallMs = durationMs(passed[2]);
    }
    const bare = line.replace(/^\S+Z\s/, "");
    const skipped = /^\s*(\d+) skipped\s*$/.exec(bare);
    if (skipped !== null) summary.skipped = Number(skipped[1]);
    const failed = /^\s*(\d+) failed\s*$/.exec(bare);
    if (failed !== null) summary.failed = Number(failed[1]);
  }
  return summary;
}

export function inventory(log: string): { rows: Row[]; summary: Summary } {
  const lines = log.split("\n");
  const seen = new Map<number, Row>();
  for (const line of lines) {
    const row = parseRow(line);
    // The reporter prints a row once; a retry or a repeated line keeps the first reading.
    if (row !== undefined && !seen.has(row.index)) seen.set(row.index, row);
  }
  const rows = [...seen.values()].sort((a, b) => a.index - b.index);
  return { rows, summary: parseSummary(lines) };
}

/** The command line, or the usage and exit 2. `parseArgs` refuses an unknown flag and a flag
 *  whose value is missing, which is the same bad command line as a value the checks here refuse,
 *  so both leave the same way. */
function readArguments(): { log: string; slowMs: number } {
  try {
    const { positionals, values } = parseArgs({
      allowPositionals: true,
      options: { "slow-ms": { type: "string" } },
      strict: true,
    });
    const slowMs = Number(values["slow-ms"] ?? 2000);
    if (positionals.length !== 1 || !Number.isFinite(slowMs)) throw new Error(usage);
    return { log: positionals[0], slowMs };
  } catch {
    console.error(usage);
    process.exit(2);
  }
}

if (import.meta.main) {
  const { log, slowMs } = readArguments();
  const { rows, summary } = inventory(await Bun.file(log).text());
  const out: string[] = ["index\tproject\tfile\tline\tcol\tms\tstatus\ttitle"];
  for (const row of rows) {
    out.push(
      [
        row.index,
        row.project,
        row.file,
        row.line,
        row.column,
        row.ms,
        row.status,
        row.title.replaceAll("\t", " "),
      ].join("\t")
    );
  }
  process.stdout.write(`${out.join("\n")}\n`);

  const ran = rows.filter((row) => row.status === "passed");
  const skipped = rows.filter((row) => row.status === "skipped");
  const total = ran.reduce((sum, row) => sum + row.ms, 0);
  const slow = ran.filter((row) => row.ms >= slowMs);
  const slowMsSum = slow.reduce((sum, row) => sum + row.ms, 0);
  const report = [
    `parsed rows           ${rows.length} (${ran.length} ran, ${skipped.length} skipped)`,
    `log says              listed ${summary.listed ?? "?"} using ${summary.workers ?? "?"} worker(s); ` +
      `${summary.passed ?? "?"} passed, ${summary.skipped ?? "?"} skipped, ${summary.failed ?? 0} failed`,
    `summed row time       ${total} ms = ${(total / 60_000).toFixed(2)} min ` +
      `(log wall ${summary.wallMs ?? "?"} ms)`,
    `rows >= ${slowMs} ms        ${slow.length} rows, ${slowMsSum} ms = ${(slowMsSum / 60_000).toFixed(2)} min`,
    `rows < ${slowMs} ms         ${ran.length - slow.length} rows, ${total - slowMsSum} ms = ` +
      `${((total - slowMsSum) / 60_000).toFixed(2)} min`,
  ];
  const residue: string[] = [];
  if (summary.listed !== undefined && summary.listed !== rows.length) {
    residue.push(`parsed ${rows.length} rows against the log's ${summary.listed} listed`);
  }
  if (summary.passed !== undefined && summary.passed !== ran.length) {
    residue.push(`parsed ${ran.length} rows that ran against the log's ${summary.passed} passed`);
  }
  if (summary.skipped !== undefined && summary.skipped !== skipped.length) {
    residue.push(`parsed ${skipped.length} skipped against the log's ${summary.skipped}`);
  }
  console.error(report.join("\n"));
  console.error(
    residue.length === 0 ? "reconciliation: zero residue" : `reconciliation: ${residue.join("; ")}`
  );
  if (residue.length > 0) process.exit(1);
}
