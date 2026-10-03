// Captures a set of declared screenshots against a freshly seeded harness. A set is a section's
// `shots.config.ts` (Dispatch's is `docs/site/media/shots.config.ts`; another section's lives in
// `docs/site/media/<section>/shots.config.ts`), and `shots.ts` runs them all.
import { mkdirSync } from "node:fs";
import { dirname, join, relative } from "node:path";

import type { Browser, Locator, Page } from "@playwright/test";

import {
  assertScreenClean,
  type Harness,
  newViewerContext,
  pageErrors,
  REPO,
  type Theme,
  type Viewport,
  WORK,
  waitForSettled,
  withinStepTimeout,
} from "./harness";

/** Every set writes `<SHOTS_ROOT>/<set>/<id>.png`, which the site serves at
 *  `/legion/media/<set>/<id>.png`. Gitignored: CI takes them before each build. */
export const SHOTS_ROOT = join(REPO, "docs/site/public/media");

export interface Shot<Seeded> {
  readonly id: string;
  /** The alt text a page embedding this shot gives it. */
  readonly alt: string;
  readonly route: string | ((seeded: Seeded) => string);
  readonly viewport: Viewport;
  /** Server-side writes that bring the seeded data to the state this shot shows, before the page
   *  opens. Shots run in the order declared, so a set can follow one record through a journey. */
  readonly prepare?: (seeded: Seeded) => Promise<void>;
  readonly theme: Theme;
  /** Waits for the page's own readiness: the content the shot is of, on screen. */
  readonly ready: (page: Page, seeded: Seeded) => Promise<void>;
  /** Brings the ready page to the state the shot shows, and waits for that state. */
  readonly steps?: (page: Page, seeded: Seeded) => Promise<void>;
  /** Captures this one element rather than the viewport. */
  readonly element?: (page: Page, seeded: Seeded) => Locator;
  /** Empty states, by `aria-label`, this shot may show; any other one fails it. */
  readonly allowEmpty?: readonly string[];
}

export interface ShotSet<Seeded> {
  /** The directory under `SHOTS_ROOT`, e.g. `dispatch`. */
  readonly set: string;
  /** Seeds the freshly reset database; what it returns reaches every shot. */
  readonly seed: () => Promise<Seeded>;
  readonly shots: readonly Shot<Seeded>[];
  /** Empty states, by `aria-label`, every shot in the set may show. */
  readonly allowEmpty?: readonly string[];
}

/** Resets and seeds the harness, takes every shot in `set` (or those `only` names) in `browser`,
 *  and returns one line per shot that failed. A failed shot leaves what the page showed in
 *  `.work/`. Every shot's `prepare` runs, in order, whether or not the shot is taken. The reset
 *  and seed, and each shot, get STEP_TIMEOUT_MS; one that runs past it ends the run with a
 *  HarnessHang naming it and what it was doing. */
export async function runShotSet<Seeded>(
  harness: Harness,
  browser: Browser,
  set: ShotSet<Seeded>,
  only?: ReadonlySet<string>
): Promise<string[]> {
  if (!set.shots.some((shot) => only === undefined || only.has(shot.id))) return [];
  const seeded = await withinStepTimeout(
    () => `${set.set}: the reset and seed`,
    harness.reset().then(() => set.seed())
  );
  const failures: string[] = [];
  for (const shot of set.shots) {
    let doing = "its prepare";
    const capture = async () => {
      await shot.prepare?.(seeded);
      if (only !== undefined && !only.has(shot.id)) return;
      const out = join(SHOTS_ROOT, set.set, `${shot.id}.png`);
      mkdirSync(dirname(out), { recursive: true });
      doing = "opening its page";
      const context = await newViewerContext(browser, harness.baseURL, shot);
      const page = await context.newPage();
      const errors = pageErrors(page);
      try {
        await page.goto(typeof shot.route === "string" ? shot.route : shot.route(seeded));
        doing = "its ready";
        await shot.ready(page, seeded);
        doing = "its steps";
        await shot.steps?.(page, seeded);
        doing = "waiting for the page to settle";
        await waitForSettled(page);
        doing = "the screen check and the capture";
        await assertScreenClean(page, errors, [
          ...(set.allowEmpty ?? []),
          ...(shot.allowEmpty ?? []),
        ]);
        const options = { animations: "disabled", caret: "hide", path: out } as const;
        await (shot.element?.(page, seeded) ?? page).screenshot(options);
        console.log(`${set.set}/${shot.id}: ${relative(REPO, out)}`);
      } catch (error) {
        doing = "keeping what its page showed";
        const failed = join(WORK, "failed-shots", `${set.set}-${shot.id}.png`);
        await page.screenshot({ path: failed }).catch(() => undefined);
        failures.push(
          `${set.set}/${shot.id}: ${error instanceof Error ? error.message : String(error)} ` +
            `(the page as it was: ${relative(REPO, failed)})`
        );
      } finally {
        doing = "closing its page";
        await context.close();
      }
    };
    await withinStepTimeout(() => `${set.set}/${shot.id}, in ${doing}`, capture());
  }
  return failures;
}
