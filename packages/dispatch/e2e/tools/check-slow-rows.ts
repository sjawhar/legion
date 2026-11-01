/** `bun e2e/tools/check-slow-rows.ts <report.json> [--slow-ms 2000] [--dir e2e]`: reads INFRA-317's
 *  acceptance A5 - "every row of the merge-base inventory is under 2 s at the head unless a
 *  comment in the row names why" - against one run's own Playwright JSON report.
 *
 *  A row that must still take `--slow-ms` or more carries a comment of exactly the form
 *
 *    // SLOW(>=2s): <reason>
 *
 *  directly above its `test(` or `test.describe(` call: the next line that is neither blank nor
 *  another comment. A marker above a `test.describe(` marks every row of that block; a marker
 *  above a `test(` inside a loop marks every row that call generates, since every one of them
 *  reports that call's line.
 *
 *  Two things fail the check, each printed under its own heading:
 *
 *  - a row at or above the threshold with no marker, because A5's claim does not hold for it;
 *  - a marker no row at or above the threshold used, because a comment that calls a row slow
 *    when it is not is a lie the next reader believes.
 *
 *  A marker in a file the report never ran is neither: no row of that file could have used it.
 *  Those are counted apart, so a run over some of the suite judges only what it ran.
 *
 *  Usage, with a report the run writes itself:
 *
 *    PLAYWRIGHT_JSON_OUTPUT_NAME=/tmp/report.json bun e2e/run.ts -- bunx playwright test \
 *      --config e2e/playwright.config.ts --reporter=list,json
 *    bun e2e/tools/check-slow-rows.ts /tmp/report.json
 */
import { readdirSync, readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { parseArgs } from "node:util";

const usage =
  "usage: bun e2e/tools/check-slow-rows.ts <report.json> [--slow-ms <n>] [--dir <e2e>]\n" +
  "  <report.json>  a Playwright JSON report (--reporter=json, PLAYWRIGHT_JSON_OUTPUT_NAME)\n" +
  "  --slow-ms      the threshold a row carries a marker for (default 2000)\n" +
  "  --dir          the directory whose *.e2e.ts hold the markers (default e2e)";

/** The marker's exact form. The reason is what the row's author wrote after the colon. */
const markerPattern = /^\s*\/\/\s*SLOW\(>=2s\):\s*(?<reason>.+?)\s*$/u;
const commentPattern = /^\s*\/\//u;
const describePattern = /^(?<indent>\s*)test\.describe[.(]/u;
const testPattern = /^\s*test[.(]/u;

export interface Row {
  readonly project: string;
  /** As the report names it, relative to Playwright's `testDir`. */
  readonly file: string;
  readonly line: number;
  readonly title: string;
  readonly ms: number;
}

export interface Marker {
  readonly file: string;
  /** The line the comment itself is on, counted from 1. */
  readonly line: number;
  readonly reason: string;
  /** The first line of the call the marker sits above. */
  readonly target: number;
  /** The last line that call covers: the `test(` call's own line, or a block's closing line. */
  readonly targetEnd: number;
  readonly kind: "test" | "describe";
}

export interface Verdict {
  readonly rowsRun: number;
  readonly rowsSlow: number;
  readonly markersFound: number;
  readonly markersUsed: number;
  /** Rows at or above the threshold that no marker covers. */
  readonly unmarked: readonly Row[];
  /** Markers in a file the report ran that no row at or above the threshold used. */
  readonly stale: readonly Marker[];
  /** Markers in a file the report never ran: judged by neither rule. */
  readonly notRun: readonly Marker[];
}

interface ReportSuite {
  readonly specs?: readonly ReportSpec[];
  readonly suites?: readonly ReportSuite[];
}

interface ReportSpec {
  readonly file?: string;
  readonly line?: number;
  readonly title?: string;
  readonly tests?: readonly ReportTest[];
}

interface ReportTest {
  readonly projectName?: string;
  readonly results?: readonly { readonly duration?: number; readonly status?: string }[];
}

/** Every row of a Playwright JSON report, one per (spec, project): the last attempt's time, and
 *  no row for one that never ran. */
export function reportRows(report: { readonly suites?: readonly ReportSuite[] }): readonly Row[] {
  const rows: Row[] = [];
  const walk = (suite: ReportSuite): void => {
    for (const spec of suite.specs ?? []) {
      for (const test of spec.tests ?? []) {
        const last = test.results?.at(-1);
        if (last === undefined || last.status === "skipped") continue;
        rows.push({
          file: spec.file ?? "",
          line: spec.line ?? 0,
          ms: last.duration ?? 0,
          project: test.projectName ?? "",
          title: spec.title ?? "",
        });
      }
    }
    for (const child of suite.suites ?? []) walk(child);
  };
  for (const suite of report.suites ?? []) walk(suite);
  return rows;
}

/** The last line of the block a `test.describe(` opens at `lines[start]`: the next line that
 *  closes a call at that call's own indentation. The repository is formatted by Biome, so a
 *  block's closer is the only `});` back at the opener's column. */
function blockEnd(lines: readonly string[], start: number, indent: string): number {
  for (let index = start + 1; index < lines.length; index += 1) {
    if (lines[index] === `${indent}});`) return index + 1;
  }
  return lines.length;
}

/** Every marker of one spec file's source, each attributed to the call it sits directly above:
 *  the next line that is neither blank nor another comment. A marker above anything else is left
 *  out - it marks no row, and the caller counts markers only where they can be used. */
export function markersIn(file: string, source: string): readonly Marker[] {
  const lines = source.split("\n");
  const markers: Marker[] = [];
  for (let index = 0; index < lines.length; index += 1) {
    const reason = markerPattern.exec(lines[index])?.groups?.reason;
    if (reason === undefined) continue;
    let next = index + 1;
    while (next < lines.length && (lines[next].trim() === "" || commentPattern.test(lines[next]))) {
      next += 1;
    }
    if (next >= lines.length) continue;
    const describe = describePattern.exec(lines[next]);
    if (describe?.groups !== undefined) {
      markers.push({
        file,
        kind: "describe",
        line: index + 1,
        reason,
        target: next + 1,
        targetEnd: blockEnd(lines, next, describe.groups.indent),
      });
      continue;
    }
    if (testPattern.test(lines[next])) {
      markers.push({
        file,
        kind: "test",
        line: index + 1,
        reason,
        target: next + 1,
        targetEnd: next + 1,
      });
    }
  }
  return markers;
}

/** A marker and a row meet when the row's `test(` call is the marked one, or sits inside the
 *  marked `describe` block. */
function covers(marker: Marker, row: Row): boolean {
  return (
    basename(marker.file) === basename(row.file) &&
    row.line >= marker.target &&
    row.line <= marker.targetEnd
  );
}

/** A5, judged: which slow rows carry no marker, and which markers no slow row used. */
export function judge(rows: readonly Row[], markers: readonly Marker[], slowMs: number): Verdict {
  const ran = new Set(rows.map((row) => basename(row.file)));
  const slow = rows.filter((row) => row.ms >= slowMs);
  const unmarked = slow.filter((row) => !markers.some((marker) => covers(marker, row)));
  const used = markers.filter((marker) => slow.some((row) => covers(marker, row)));
  const judged = markers.filter((marker) => ran.has(basename(marker.file)));
  return {
    markersFound: markers.length,
    markersUsed: used.length,
    notRun: markers.filter((marker) => !ran.has(basename(marker.file))),
    rowsRun: rows.length,
    rowsSlow: slow.length,
    stale: judged.filter((marker) => !used.includes(marker)),
    unmarked,
  };
}

/** The command line, or the usage and exit 2. `parseArgs` refuses an unknown flag and a flag
 *  whose value is missing, which is the same bad command line as a value the checks here refuse,
 *  so both leave the same way. */
function readArguments(): { report: string; slowMs: number; dir: string } {
  try {
    const { positionals, values } = parseArgs({
      allowPositionals: true,
      options: { dir: { type: "string" }, "slow-ms": { type: "string" } },
      strict: true,
    });
    const slowMs = Number(values["slow-ms"] ?? 2000);
    const dir = values.dir ?? "e2e";
    if (positionals.length !== 1 || !Number.isFinite(slowMs) || slowMs <= 0 || dir === "") {
      throw new Error(usage);
    }
    return { dir, report: positionals[0], slowMs };
  } catch {
    console.error(usage);
    process.exit(2);
  }
}

if (import.meta.main) {
  const { dir, report, slowMs } = readArguments();
  const rows = reportRows(JSON.parse(readFileSync(report, "utf8")));
  const markers = readdirSync(dir)
    .filter((name) => name.endsWith(".e2e.ts"))
    .sort()
    .flatMap((name) => markersIn(name, readFileSync(join(dir, name), "utf8")));
  const verdict = judge(rows, markers, slowMs);

  if (verdict.unmarked.length > 0) {
    console.error(`rows at or above ${slowMs} ms with no // SLOW(>=2s) comment:`);
    for (const row of [...verdict.unmarked].sort((a, b) => b.ms - a.ms)) {
      console.error(`  ${row.project} ${row.file}:${row.line} ${row.title} (${row.ms} ms)`);
    }
  }
  if (verdict.stale.length > 0) {
    console.error(`// SLOW(>=2s) comments no row at or above ${slowMs} ms used:`);
    for (const marker of verdict.stale) {
      console.error(`  ${marker.file}:${marker.line} ${marker.reason}`);
    }
  }
  console.error(
    `rows run ${verdict.rowsRun}, rows >= ${slowMs} ms ${verdict.rowsSlow}, ` +
      `markers found ${verdict.markersFound} (${verdict.notRun.length} in files this report did ` +
      `not run), markers used ${verdict.markersUsed}`
  );
  if (verdict.unmarked.length > 0 || verdict.stale.length > 0) process.exit(1);
}
