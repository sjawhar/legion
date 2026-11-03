// The measurement set-up for a large document on a phone, as dispatch://INFRA-341/spec words it:
// the browser suite's `iphone` project with 4x CPU throttling, reading two numbers from the page
// itself. The first is the time from the start of navigation until the document's first block is
// in the viewport. The second is the longest main-thread task, through a `PerformanceObserver`
// for `longtask` entries, from the start of navigation until the page has scrolled to the end of
// the document one viewport height every 100 ms.
//
// Both numbers are the page's own `performance.now()`, which counts from the navigation that
// created the document, so no Node-side round trip is in either of them. `large-documents.e2e.ts`
// prints what this records; it asserts no target, because the targets are read from these runs
// (INFRA-341, stage 10a).
import { expect, type Page, test } from "@playwright/test";

declare global {
  interface Window {
    /** `performance.now()` when the document's first block was first inside the viewport, which
     *  is the milliseconds from the start of navigation; undefined until it was. */
    __largeDocumentFirstBlockAt?: number;
    /** The longest `longtask` entry's duration, in milliseconds, since the page began. */
    __largeDocumentLongestTask?: number;
  }
}

/** What the renderer's CPU is throttled by: the rate the set-up names
 *  (`Emulation.setCPUThrottlingRate`). */
export const cpuThrottlingRate = 4;

/** How long the scroll waits between steps; each step moves one viewport height. */
export const scrollStepMs = 100;

/** The scroll stops at the end of the document or after this many steps, whichever comes first,
 *  so a document that keeps growing under the scroll still ends the measurement. */
export const scrollStepsMax = 120;

/** The document's first block: the first `[data-block-id]` element
 *  (`BLOCK_ID_ATTRIBUTE` in `web/src/features/doc/scroll-block.ts`) inside the document itself,
 *  rather than anywhere in the page.
 *
 *  Two surfaces draw the document, and whichever of them is up is what the reader is reading,
 *  which is what this number is for: the editor (`Document editor`, the one that takes edits)
 *  and, before it exists, the document's own text drawn from the server's markdown
 *  (`Document text`, `web/src/features/doc/DocumentPreview.tsx`). Each stamps every block it
 *  draws with the same attribute, and the preview stands before the editor in the page, so
 *  `querySelector` answers with the first block of whichever is drawing. */
const firstBlockSelector =
  '[aria-label="Document editor"] [data-block-id], [aria-label="Document text"] [data-block-id]';

export interface DocumentMeasurement {
  /** Milliseconds from the start of navigation until the first block was in the viewport. */
  readonly firstBlockMs: number;
  /** The longest main-thread task, in milliseconds, over the whole measurement. */
  readonly longestTaskMs: number;
  /** How many viewport heights the scroll moved before it reached the end of the document or its
   *  cap, which says the page scrolled at all. */
  readonly scrollSteps: number;
}

/**
 * Throttles `page`'s renderer through a CDP session of its own
 * (`page.context().newCDPSession(page)`, as `e2e/touch.ts` opens one). The session stays attached
 * for the page's life: detaching it would drop the override the measurement is taken under.
 * Chromium only, which is the engine the `iphone` project runs.
 */
export async function throttleTheCpu(page: Page): Promise<void> {
  const session = await page.context().newCDPSession(page);
  await session.send("Emulation.setCPUThrottlingRate", { rate: cpuThrottlingRate });
}

/**
 * Installs both recorders before the page's own scripts: the `longtask` observer, which takes
 * the entries buffered before it registered as well, and a frame loop that records
 * `performance.now()` the first time the document's first block has a box inside the viewport.
 * Call it before the navigation it measures.
 */
export async function watchTheDocument(page: Page): Promise<void> {
  await page.addInitScript((selector: string) => {
    window.__largeDocumentLongestTask = 0;
    new PerformanceObserver((entries) => {
      for (const entry of entries.getEntries()) {
        window.__largeDocumentLongestTask = Math.max(
          window.__largeDocumentLongestTask ?? 0,
          entry.duration
        );
      }
    }).observe({ buffered: true, type: "longtask" });
    const look = () => {
      const box = document.querySelector(selector)?.getBoundingClientRect();
      if (
        box !== undefined &&
        box.width > 0 &&
        box.height > 0 &&
        box.top < window.innerHeight &&
        box.bottom > 0
      ) {
        window.__largeDocumentFirstBlockAt = performance.now();
        return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  }, firstBlockSelector);
}

/**
 * Opens `path` and measures it: the navigation starts the page's clock, the first block ends the
 * first number, and the scroll to the end of the document ends the second. The navigation waits
 * for the response alone (`commit`), so a document whose `load` lands long after its first block
 * does not hold the measurement up.
 *
 * The wait for the first block is `page.evaluate` polled from here, never `waitForFunction`,
 * whose predicate runs in an isolated world where an init script's `window` property does not
 * exist. What it polls is only when this reads the number; the number itself is the one the
 * page's own frame loop recorded.
 */
export async function measureDocument(
  page: Page,
  path: string,
  firstBlockTimeoutMs: number
): Promise<DocumentMeasurement> {
  await throttleTheCpu(page);
  await watchTheDocument(page);
  await page.goto(path, { waitUntil: "commit" });

  await expect
    .poll(() => page.evaluate(() => window.__largeDocumentFirstBlockAt), {
      intervals: [500],
      message:
        "the document's first block never came into the viewport: either the page never drew it, " +
        "or a taller page header has pushed it under the fold, where no scroll of this " +
        "measurement's reaches it",
      timeout: firstBlockTimeoutMs,
    })
    .not.toBeUndefined();
  const firstBlockMs = await page.evaluate(() => window.__largeDocumentFirstBlockAt);

  // The loop runs in the page, so its steps are the page's own 100 ms apart rather than a round
  // trip's, and a step the throttled main thread runs late delays the next step rather than
  // piling up behind it.
  const scrollSteps = await page.evaluate(
    ({ cap, stepMs }) => {
      const scrolled = Promise.withResolvers<number>();
      let steps = 0;
      const step = () => {
        const root = document.scrollingElement;
        if (root === null) {
          scrolled.resolve(steps);
          return;
        }
        if (steps >= cap || root.scrollTop + root.clientHeight >= root.scrollHeight - 1) {
          scrolled.resolve(steps);
          return;
        }
        steps += 1;
        root.scrollTop += root.clientHeight;
        setTimeout(step, stepMs);
      };
      setTimeout(step, stepMs);
      return scrolled.promise;
    },
    { cap: scrollStepsMax, stepMs: scrollStepMs }
  );

  const longestTaskMs = await page.evaluate(() => window.__largeDocumentLongestTask);
  if (firstBlockMs === undefined || longestTaskMs === undefined) {
    throw new Error("the page lost its recorders: an init script never ran");
  }
  return { firstBlockMs, longestTaskMs, scrollSteps };
}

/**
 * Puts a measurement where the run's reader finds it: on the row's annotations, which the report
 * keeps with the result, and on stdout, which `--reporter line` prints as the row runs.
 */
export function printMeasurement(what: string, measured: DocumentMeasurement): void {
  const line =
    `${what}: first block ${Math.round(measured.firstBlockMs)} ms, ` +
    `longest main-thread task ${Math.round(measured.longestTaskMs)} ms, ` +
    `over ${measured.scrollSteps} scroll steps`;
  test.info().annotations.push({ description: line, type: "R11 measurement" });
  console.log(`R11 measurement · ${line}`);
}
