/** The reporter half of the timing fixture (e2e/measure.ts): one JSON line per test row, in the
 *  file `E2E_MEASURE_FILE` names, read back by e2e/tools/measure-summary.ts. The config adds it
 *  beside the list reporter only when that variable is set, so an ordinary run writes nothing.
 *
 *    E2E_MEASURE_FILE=/tmp/measure/head.jsonl bun e2e/run.ts -- bunx playwright test \
 *      --config e2e/playwright.config.ts --project=chromium e2e/smoke.e2e.ts
 */
import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, relative } from "node:path";
import { fileURLToPath } from "node:url";
import type { Reporter, TestCase, TestResult, TestStep } from "@playwright/test/reporter";

import { type MeasuredPart, measureFile } from "./measure";

/** One row's bill, as a line of the measurement file. A part the row never runs is 0. */
export interface MeasureRecord {
  workerIndex: number;
  project: string;
  file: string;
  line: number;
  title: string;
  resetSqlMs: number;
  resetFakesMs: number;
  signInMs: number;
  firstPageLoadMs: number;
}

type MeasuredField = "resetSqlMs" | "resetFakesMs" | "signInMs";

const measuredFields: Record<MeasuredPart, MeasuredField> = {
  "reset-sql": "resetSqlMs",
  "reset-fakes": "resetFakesMs",
  "sign-in": "signInMs",
};

/** The `measure:<part>` step titles e2e/measure.ts writes, to the field each sums into. */
const fieldsByStepTitle: Record<string, MeasuredField | undefined> = Object.fromEntries(
  Object.entries(measuredFields).map(([part, field]) => [`measure:${part}`, field])
);

// Paths are reported as the list reporter prints them, relative to the package rather than to
// the run's cwd or to Playwright's rootDir (which is this directory): `e2e/smoke.e2e.ts`.
const packageRoot = fileURLToPath(new URL("..", import.meta.url));

// Playwright 1.64 titles a page's navigation step `Navigate` (read off a run's own step tree);
// older versions wrote `Navigate to "<url>"`, so the prefix is what is matched, as a whole word.
const navigationStep = /^Navigate\b/;

function walkSteps(
  steps: readonly TestStep[],
  visit: (step: TestStep, measuredAbove: boolean) => void,
  measuredAbove = false
): void {
  for (const step of steps) {
    visit(step, measuredAbove);
    walkSteps(step.steps, visit, measuredAbove || fieldsByStepTitle[step.title] !== undefined);
  }
}

export default class MeasureReporter implements Reporter {
  onBegin(): void {
    // A re-run of one path must not read as one run of two, so the file is emptied once here
    // rather than between rows: every worker appends to it while the run lasts.
    mkdirSync(dirname(measureFile), { recursive: true });
    writeFileSync(measureFile, "");
  }

  onTestEnd(test: TestCase, result: TestResult): void {
    const record: MeasureRecord = {
      workerIndex: result.workerIndex,
      project: test.parent.project()?.name ?? "",
      file: relative(packageRoot, test.location.file),
      line: test.location.line,
      title: test.title,
      resetSqlMs: 0,
      resetFakesMs: 0,
      signInMs: 0,
      firstPageLoadMs: 0,
    };
    let firstNavigation: TestStep | undefined;
    walkSteps(result.steps, (step, measuredAbove) => {
      const field = fieldsByStepTitle[step.title];
      // A part can be paid more than once in a row (a reset per scenario step, two sign-ins),
      // and the row's bill is what it paid altogether.
      if (field !== undefined) record[field] += step.duration;
      if (step.category !== "pw:api" || !navigationStep.test(step.title)) return;
      // A navigation inside a part is that part's own, not the row's first page load: the
      // plain-HTTP sign-in loads the dev sign-in route in a page (e2e/users.ts), and counting it
      // in both would bill those rows twice.
      if (measuredAbove) return;
      if (firstNavigation === undefined || step.startTime < firstNavigation.startTime) {
        firstNavigation = step;
      }
    });
    record.firstPageLoadMs = firstNavigation?.duration ?? 0;
    // One `appendFileSync` of one whole line: a write this small is atomic on Linux, so the
    // lines of several workers interleave but never tear. `JSON.stringify` escapes a newline in
    // a title, so a record is always exactly one line.
    appendFileSync(measureFile, `${JSON.stringify(record)}\n`);
  }
}
