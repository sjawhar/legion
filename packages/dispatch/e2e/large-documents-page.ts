// The measurement set-up for a large document on a phone, as dispatch://INFRA-341/spec words it:
// the browser suite's `iphone` project with 4x CPU throttling, reading three numbers from the
// page itself. The first is the time from the start of navigation until the document's first
// block is in the viewport. The second is the time until the editor is on the screen, which is
// the end of its build. The third is the longest main-thread task, through a
// `PerformanceObserver` for `longtask` entries, from the start of navigation until the editor
// has opened and the page has then scrolled to the end of the document one viewport height
// every 100 ms.
//
// The window waits for the editor because the page is not the document until the editor is in
// it. The reader meets the document as its own draw (`web/src/features/doc/DocumentPreview.tsx`)
// while the editor is built behind it, and that draw is the blocks the document opens with and
// no more: its second pass, the whole document, is a parse the worker is given and - for the
// larger of the two documents, answered 16 s after it was posted against the queue's 10 s
// deadline - never lands, and from the moment the build begins the draw stops painting anyway,
// since every batch of the build yields the thread and a paint into those gaps is work for a
// draw the editor is about to replace. So the page is a screenful high for most of the build,
// and a window that closed where that page ends closed four scroll steps in, before the build
// it is there to measure (dispatch://INFRA-341, R11).
//
// Every number is the page's own `performance.now()`, which counts from the navigation that
// created the document, so no Node-side round trip is in any of them. `large-documents.e2e.ts`
// prints what this records; it asserts no target, because the targets are read from these runs
// (INFRA-341, stage 10a).
import { expect, type Page, test } from "@playwright/test";

declare global {
  interface Window {
    /** `performance.now()` when the document's first block was first inside the viewport, which
     *  is the milliseconds from the start of navigation; undefined until it was. */
    __largeDocumentFirstBlockAt?: number;
    /** `performance.now()` when the editor was first on the screen, which is the end of its
     *  build and of the swap that put it in front of the reader; undefined until it was. */
    __largeDocumentEditorAt?: number;
    /** The longest `longtask` entry's duration, in milliseconds, since the page began. */
    __largeDocumentLongestTask?: number;
  }
}

/** What the renderer's CPU is throttled by: the rate the set-up names
 *  (`Emulation.setCPUThrottlingRate`). */
export const cpuThrottlingRate = 4;

/** How long the scroll waits between steps; each step moves one viewport height. */
export const scrollStepMs = 100;

/** How often the page looks for the editor's box, which is the end of its build and of the
 *  window. A timer rather than a frame loop, for the reason `watchTheDocument` gives. */
export const editorPollMs = 200;

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

/** The editor itself, whose box is the end of its build: it has none until the swap shows its
 *  root (`web/src/features/doc/ProofDocument.tsx`). The textbox, never the `Document editor`
 *  section around it: that section is the whole document surface, the draw the reader reads
 *  first included, and it has a box from the page's first commit. */
const editorSelector = '[role="textbox"][aria-label="Document editor"]';

export interface DocumentMeasurement {
  /** Milliseconds from the start of navigation until the first block was in the viewport. */
  readonly firstBlockMs: number;
  /** Milliseconds from the start of navigation until the editor was on the screen: the end of
   *  its build. `undefined` where it never opened inside the wait the caller gave it. */
  readonly editorOpenedMs: number | undefined;
  /** The longest main-thread task, in milliseconds, over the whole measurement. */
  readonly longestTaskMs: number;
  /** How many viewport heights the scroll moved before it reached the end of the document or its
   *  cap, which says the page scrolled at all. */
  readonly scrollSteps: number;
  /** Milliseconds from the start of navigation until the window closed, which is what the
   *  longest task is the longest of. */
  readonly windowClosedMs: number;
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
 * Installs the recorders before the page's own scripts: the `longtask` observer, which takes
 * the entries buffered before it registered as well, a frame loop that records
 * `performance.now()` the first time the document's first block has a box inside the viewport,
 * and a timer that records when the editor first has a box at all. Call it before the
 * navigation it measures.
 *
 * The editor's box is what says the build is over and the swap has run: its root is hidden for
 * the whole build and shown in the task that takes the document's own draw off the page
 * (`@legion/proof-editor`'s `reveal`, `web/src/features/doc/ProofDocument.tsx`), so an editor
 * with a box is an editor a reader is reading.
 *
 * The first block is read every frame, because it is one of the two numbers the targets are
 * read against and a frame is as often as the page can change. The editor is read on a timer
 * instead: it is read for most of a build that can last ten seconds, each read is a layout the
 * page did not ask for, and a frame loop running through the build would both pay that layout
 * sixty times a second and keep the page rendering while the batches want the thread. What it
 * costs is a window that closes up to `editorPollMs` late, which is no target.
 */
export async function watchTheDocument(page: Page): Promise<void> {
  await page.addInitScript(
    ({
      editor: editorSelector,
      first: firstSelector,
      pollMs,
    }: {
      editor: string;
      first: string;
      pollMs: number;
    }) => {
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
        const box = document.querySelector(firstSelector)?.getBoundingClientRect();
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
      const lookForTheEditor = () => {
        const box = document.querySelector(editorSelector)?.getBoundingClientRect();
        if (box !== undefined && box.width > 0 && box.height > 0) {
          window.__largeDocumentEditorAt = performance.now();
          return;
        }
        setTimeout(lookForTheEditor, pollMs);
      };
      setTimeout(lookForTheEditor, pollMs);
    },
    { editor: editorSelector, first: firstBlockSelector, pollMs: editorPollMs }
  );
}

/** What a caller gives the measurement beyond the page and the path: how long each of its two
 *  waits may last. Each is a wait for something that never came, so a failure reads as the miss
 *  it is rather than as a hang; a build that draws at all is read against the target. */
export interface MeasurementWaits {
  /** How long to wait for the editor to open, which is the end of its build. */
  readonly buildTimeoutMs: number;
  /** How long to wait for the document's first block to come into the viewport. */
  readonly firstBlockTimeoutMs: number;
}

/**
 * Opens `path` and measures it: the navigation starts the page's clock, the first block ends the
 * first number, the editor opening ends the second, and the scroll to the end of the document
 * then closes the window the third is read over. The navigation waits for the response alone
 * (`commit`), so a document whose `load` lands long after its first block does not hold the
 * measurement up.
 *
 * The scroll waits for the editor rather than racing it, because until the editor is in the
 * page there is no document to scroll: the draw in front of the reader is a screenful of blocks
 * and the page ends a few steps down (the module comment above). So the window covers the whole
 * build and then a scroll through the whole document.
 *
 * Each wait is `page.evaluate` polled from here, never `waitForFunction`, whose predicate runs
 * in an isolated world where an init script's `window` property does not exist. What it polls is
 * only when this reads the number; the numbers themselves are the ones the page's own frame loop
 * recorded.
 */
export async function measureDocument(
  page: Page,
  path: string,
  { buildTimeoutMs, firstBlockTimeoutMs }: MeasurementWaits
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

  await expect
    .poll(() => page.evaluate(() => window.__largeDocumentEditorAt), {
      intervals: [500],
      message:
        "the editor never opened, so there was no build to measure and no document to scroll: " +
        "the page is still the document's own draw of its first blocks",
      timeout: buildTimeoutMs,
    })
    .not.toBeUndefined();
  const editorOpenedMs = await page.evaluate(() => window.__largeDocumentEditorAt);

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
  const windowClosedMs = await page.evaluate(() => performance.now());
  if (firstBlockMs === undefined || longestTaskMs === undefined) {
    throw new Error("the page lost its recorders: an init script never ran");
  }
  return { editorOpenedMs, firstBlockMs, longestTaskMs, scrollSteps, windowClosedMs };
}

/**
 * Puts a measurement where the run's reader finds it: on the row's annotations, which the report
 * keeps with the result, and on stdout, which `--reporter line` prints as the row runs.
 */
export function printMeasurement(what: string, measured: DocumentMeasurement): void {
  const line =
    `${what}: first block ${Math.round(measured.firstBlockMs)} ms, ` +
    `editor open ${measured.editorOpenedMs === undefined ? "never" : `${Math.round(measured.editorOpenedMs)} ms`}, ` +
    `longest main-thread task ${Math.round(measured.longestTaskMs)} ms, ` +
    `over ${measured.scrollSteps} scroll steps in a ${Math.round(measured.windowClosedMs)} ms window`;
  test.info().annotations.push({ description: line, type: "R11 measurement" });
  console.log(`R11 measurement · ${line}`);
}
