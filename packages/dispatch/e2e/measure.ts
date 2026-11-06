/** The timing fixture of INFRA-317: the four parts every row pays before its own work, measured
 *  per row so a before/after run can be compared part by part. It is off unless
 *  `E2E_MEASURE_FILE` names the file e2e/measure-reporter.ts writes the rows to, and off it costs
 *  a call nothing: `measured` returns the body's own promise and adds no step. The fourth part,
 *  the first page load, needs no wrapper here: the reporter reads it from Playwright's own
 *  navigation step. */
import { test } from "@playwright/test";

/** The file e2e/measure-reporter.ts appends to, or "" when this run measures nothing. */
export const measureFile = process.env.E2E_MEASURE_FILE ?? "";
export const measuring = measureFile !== "";

export type MeasuredPart = "reset-sql" | "reset-fakes" | "sign-in";

/** Runs `body` as the part `part`, timed by Playwright as one boxed step titled
 *  `measure:<part>`, which the reporter reads back off the result's step tree. */
export async function measured<T>(part: MeasuredPart, body: () => Promise<T>): Promise<T> {
  if (!measuring) return body();
  try {
    test.info();
  } catch {
    // `test.step` throws outside a running test, and the parts are not only paid inside one:
    // e2e/preflight.ts, Playwright's globalSetup, runs before any row. Only this probe is
    // guarded, never `body`, so a real failure inside the part still propagates.
    return body();
  }
  return test.step(`measure:${part}`, body, { box: true });
}
