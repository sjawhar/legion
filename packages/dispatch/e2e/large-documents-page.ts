// The measurement set-up for a large document on a phone, as dispatch://INFRA-341/spec words it:
// the browser suite's `iphone` project with 4x CPU throttling, reading five numbers from the
// page itself. The first is the time from the start of navigation until the editor's first
// visible block - a block of the editor's own root while that root carries
// `data-write-path="live"`, which is the first block a reader can type in and have the edit
// reach the Yjs document. The second is the time until the editor is open, which is its last
// batch in and its Yjs binding live. The third is the longest main-thread task, through a
// `PerformanceObserver` for `longtask` entries, from the start of navigation until the editor
// has opened and the page has then scrolled to the end of the document one viewport height
// every 100 ms.
//
// The last two are the draws a reader has in front of all that, neither of them read against a
// target: the time until any block of the document was on the screen, whoever drew it, which
// is the server's own blocks where it drew them into the page it sent (`first painted`, about
// a second into the load on this pod); and the time until the page's own code had a block of
// the document on the screen, in the preview it renders inside the document surface, which is
// about seven seconds later. The second of those is what this rig read as the editor's block
// until INFRA-341: its selector fell back to the `Document editor` section's name, and that
// section is the whole document surface, the preview included. Neither draw takes an edit.
//
// Neither draw can be the first number now: the spec's Design says a server or preview block is
// not the block R11 means, and `firstBlockSelector` below rules both out element by element,
// rather than taking whichever block is on the screen first. What the measurement timed says so
// itself - the row reads back where that block stood - so a rig that drifted back onto the
// preview fails rather than printing its draw as the editor's.
//
// The window waits for the editor because the targets are about the whole build. The editor
// shows its first batch's blocks within a second of the sync and builds the rest behind them, a
// batch at a time (`packages/proof-editor/src/progressive-build.ts`), and the document's own
// draw gives up each of those blocks as they land (`web/src/features/doc/DocumentPreview.tsx`),
// so the page holds the whole document throughout and the scroll at the end of the window walks
// the whole of it. A window that closed at the editor's first block would close with most of the
// build still to come (dispatch://INFRA-341, R11).
//
// Every number is the page's own `performance.now()`, which counts from the navigation that
// created the document, so no Node-side round trip is in any of them. `large-documents.e2e.ts`
// prints all five and asserts two of them against R11's targets - the editor's first visible
// block and the longest main-thread task - and the line it prints names which two those are.
import { expect, type Page, test } from "@playwright/test";

declare global {
  interface Window {
    /** What the editor's first visible block was timed as: `performance.now()` when a block of
     *  an editor root carrying `data-write-path="live"` was first inside the viewport, which is
     *  the milliseconds from the start of navigation, and what that block itself said about
     *  where it was drawn. Undefined until there was one. */
    __largeDocumentFirstBlock?: {
      at: number;
      editable: boolean;
      inLiveEditor: boolean;
      inPreview: boolean;
    };
    /** `performance.now()` when the editor was first open: its last batch in and its Yjs
     *  binding live; undefined until it was. */
    __largeDocumentEditorAt?: number;
    /** The longest `longtask` entry's duration, in milliseconds, since the page began. */
    __largeDocumentLongestTask?: number;
    /** Every `longtask` entry since the page began, by when it started and how long it ran.
     *  The longest number above says how bad the worst freeze is; this says how many there
     *  were and where each fell, which is what a report of the tasks still over the target
     *  reads (dispatch://INFRA-341, R11). */
    __largeDocumentLongTasks?: { at: number; ms: number }[];
    /** `performance.now()` when the reader first had any block of the document on the screen -
     *  the server's draw, or the page's own - which is read against no target. */
    __largeDocumentPaintedAt?: number;
    /** `performance.now()` when the page's own code first had a block of the document on the
     *  screen: a block inside the document surface it renders, which is the preview's. Read
     *  against no target, and the number this rig used to read as the editor's. */
    __largeDocumentPageDrawnAt?: number;
  }
}

/** What the renderer's CPU is throttled by: the rate the set-up names
 *  (`Emulation.setCPUThrottlingRate`). */
export const cpuThrottlingRate = 4;

/** The longest main-thread task either document may cost a reader ("Targets for large
 *  documents", dispatch://INFRA-341/spec): a task over half a second is a freeze a person
 *  sees. The measuring rows read it as their target, and the line this module prints names
 *  every task past it. */
export const longTaskTargetMs = 500;

/** How long the scroll waits between steps; each step moves one viewport height. */
export const scrollStepMs = 100;

/** How often the page looks for the editor's box, which is the end of its build and of the
 *  window. A timer rather than a frame loop, for the reason `watchTheDocument` gives. */
export const editorPollMs = 200;

/** The scroll stops at the end of the document or after this many steps, whichever comes first,
 *  so a document that keeps growing under the scroll still ends the measurement. */
export const scrollStepsMax = 120;

/** The editor's first visible block, which is the number the targets are read against: the
 *  first `[data-block-id]` element (`BLOCK_ID_ATTRIBUTE` in
 *  `web/src/features/doc/scroll-block.ts`) inside an editor root that says its blocks take
 *  edits reaching the Yjs document.
 *
 *  `data-write-path="live"` is written on the editor's root in the task its first batch lands
 *  in, which is the task that root is shown in - it is `display: none` before it - and the
 *  task from which an edit in those blocks is written through to the document
 *  (`packages/proof-editor/src/progressive-build.ts`). Nothing else on the page can wear it:
 *  `.proof-editor` is the class `createProofEditor` puts on the root it builds into, and that
 *  build is the only writer of the attribute, which it removes again when the editor is
 *  destroyed (`packages/proof-editor/src/lib.ts`). So neither the blocks the server drew into
 *  the page (`#first-screen`) nor the page's own draw of the document's text (`Document text`,
 *  a sibling of the editor's root inside the `Document editor` section,
 *  `web/src/features/doc/DocumentPreview.tsx`) can be read as this block. The fallback that
 *  once stood here - the `Document editor` section's own name - could: that section is the
 *  whole document surface, the draw the reader meets first included, so the measurement timed
 *  the preview's block and read it against R11's target.
 *
 *  A build whose editor never carries the attribute draws no block this times, and the wait
 *  for it fails as the miss it is rather than quietly timing another surface's draw. */
const liveEditorSelector = '.proof-editor[data-write-path="live"]';
const firstBlockSelector = `${liveEditorSelector} [data-block-id]`;

/** Where a block a reader has before the editor exists stands: the element the server put the
 *  document's opening blocks in (`web/src/features/doc/first-screen.ts`), and the page's own
 *  draw of the document's text, which adopts them and then draws the rest
 *  (`web/src/features/doc/DocumentPreview.tsx`). Neither takes an edit. The measurement reads
 *  this off the block it timed, so a run says which surface drew that block rather than leaving
 *  it to be read off the selector. */
const previewSelector = '#first-screen, [aria-label="Document text"]';

/** The page's own draw of the document: a block inside the `Document editor` section, which is
 *  the whole document surface the page renders - the preview it draws the text in, the editor's
 *  root, and the cards beside them (`web/src/features/doc/ProofDocument.tsx`). Until the editor
 *  builds, the only blocks in it are the preview's, so this is the preview's first block and
 *  the page's own first draw of the document in one number.
 *
 *  It is scoped by the section rather than by the preview's own `Document text` name because
 *  the server writes that same name into the page it sends, around the blocks the preview
 *  adopts (`internal/dispatch/pmdoc/first_screen.go`, `first-screen.test.tsx`): a selector on
 *  the name alone reads the server's draw, which `first painted` already times.
 *
 *  This is the selector the measurement read the editor's first block by until INFRA-341, which
 *  is the whole of that defect: a preview block, drawn before the editor exists and taking no
 *  edit, read against R11's target for the block a reader can type in. It is recorded here, and
 *  asserted nowhere. */
const pageDrawnSelector = '[aria-label="Document editor"] [data-block-id]';

/** The first block the reader had on the screen at all, whoever drew it: the blocks the server
 *  put in the page it sent, and the page's own draw of the document's text. Recorded as
 *  `first painted`, and read against no target. */
const firstPaintedSelector =
  '#first-screen [data-block-id], [aria-label="Document text"] [data-block-id]';

/** The editor, open: its last batch is in and its Yjs binding is live. The host names the
 *  textbox once `createProofEditor` has answered its handle, which is a task after the binding
 *  connected (`web/src/features/doc/editor.ts`'s `editorAttributes`), so an editor wearing this
 *  name is an editor whose build is over. Never the `Document editor` section around it: that
 *  section is the whole document surface, the draw the reader reads first included, and it has
 *  a box from the page's first commit. */
const editorSelector = '[role="textbox"][aria-label="Document editor"]';

export interface DocumentMeasurement {
  /** Milliseconds from the start of navigation until the editor's first visible block was in
   *  the viewport: a block of an editor root carrying `data-write-path="live"`, which is the
   *  first block a reader can type in. */
  readonly firstBlockMs: number;
  /** What the block `firstBlockMs` timed was, read off that element in the frame it was timed
   *  in: whether it stood inside an editor root carrying `data-write-path="live"`, whether it
   *  stood inside an editable host, and whether it stood in either draw the reader has before
   *  the editor exists. The measuring rows assert all three, so a rig that had timed the
   *  preview's block would fail rather than print that draw as the editor's. */
  readonly firstBlock: {
    readonly editable: boolean;
    readonly inLiveEditor: boolean;
    readonly inPreview: boolean;
  };
  /** Milliseconds from the start of navigation until the editor was open: its last batch in
   *  and its binding live. `undefined` where it never opened inside the wait the caller gave
   *  it. */
  readonly editorOpenedMs: number | undefined;
  /** Milliseconds from the start of navigation until the reader had any block of the document
   *  on the screen, whoever drew it. `undefined` where nothing was ever drawn. */
  readonly firstPaintedMs: number | undefined;
  /** Milliseconds from the start of navigation until the page's own code had a block of the
   *  document on the screen, in the preview inside the surface it renders. `undefined` where
   *  it never did. */
  readonly pageDrawnMs: number | undefined;
  /** The longest main-thread task, in milliseconds, over the whole measurement. */
  readonly longestTaskMs: number;
  /** Every main-thread task over `longTaskMs`, in the order they ran: when each started,
   *  counted from the start of navigation, and how long it held the thread. What the longest
   *  number alone cannot say is how many freezes a reader met and where. */
  readonly longTasks: readonly { readonly at: number; readonly ms: number }[];
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
 * `performance.now()` the first time the editor's first visible block has a box inside the
 * viewport - with what that block says about where it was drawn - the first time any draw of
 * the document has one and the first time the page's own draw of the text has one, and a timer
 * that records when the editor first has a box of its own under its name. Call it before the
 * navigation it measures.
 *
 * The three blocks are recorded independently: the two draws are almost always on the screen
 * first, and the loop goes on looking for the editor's own past both of them, which is the
 * number the targets are read against.
 *
 * The named editor's box is what says the build is over and the binding is live: the host names
 * the textbox only once `createProofEditor` has answered, a task after the binding connected
 * (`web/src/features/doc/editor.ts`), so an editor with that name and a box is an editor whose
 * whole document is in it.
 *
 * The blocks are read every frame, because one of them is the number the targets are read
 * against and a frame is as often as the page can change. The editor's own box is read on a
 * timer instead: it is read for most of a build that can last ten seconds, each read is a
 * layout the page did not ask for, and a frame loop running through the build would both pay
 * that layout sixty times a second and keep the page rendering while the batches want the
 * thread. What it costs is a window that closes up to `editorPollMs` late, which is no target.
 */
export async function watchTheDocument(page: Page): Promise<void> {
  await page.addInitScript(
    ({
      drawn: drawnSelector,
      editor: editorSelector,
      first: firstSelector,
      live: liveSelector,
      painted: paintedSelector,
      pollMs,
      preview: previewRoots,
    }: {
      drawn: string;
      editor: string;
      first: string;
      live: string;
      painted: string;
      pollMs: number;
      preview: string;
    }) => {
      window.__largeDocumentLongestTask = 0;
      window.__largeDocumentLongTasks = [];
      new PerformanceObserver((entries) => {
        for (const entry of entries.getEntries()) {
          window.__largeDocumentLongestTask = Math.max(
            window.__largeDocumentLongestTask ?? 0,
            entry.duration
          );
          window.__largeDocumentLongTasks?.push({ at: entry.startTime, ms: entry.duration });
        }
      }).observe({ buffered: true, type: "longtask" });
      const inTheViewport = (selector: string): Element | null => {
        const element = document.querySelector(selector);
        if (element === null) {
          return null;
        }
        const box = element.getBoundingClientRect();
        const seen =
          box.width > 0 && box.height > 0 && box.top < window.innerHeight && box.bottom > 0;
        return seen ? element : null;
      };
      const look = () => {
        if (
          window.__largeDocumentPaintedAt === undefined &&
          inTheViewport(paintedSelector) !== null
        ) {
          window.__largeDocumentPaintedAt = performance.now();
        }
        if (
          window.__largeDocumentPageDrawnAt === undefined &&
          inTheViewport(drawnSelector) !== null
        ) {
          window.__largeDocumentPageDrawnAt = performance.now();
        }
        const block = inTheViewport(firstSelector);
        if (block !== null) {
          // Read off the block itself rather than off the selector that found it, so the run
          // says which surface drew what it timed and the rows can hold it to the editor's.
          window.__largeDocumentFirstBlock = {
            at: performance.now(),
            editable: block.closest('[contenteditable="true"]') !== null,
            inLiveEditor: block.closest(liveSelector) !== null,
            inPreview: block.closest(previewRoots) !== null,
          };
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
    {
      drawn: pageDrawnSelector,
      editor: editorSelector,
      first: firstBlockSelector,
      live: liveEditorSelector,
      painted: firstPaintedSelector,
      pollMs: editorPollMs,
      preview: previewSelector,
    }
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
 * Opens `path` and measures it: the navigation starts the page's clock, the editor's first
 * block ends the first number, the editor opening ends the second, and the scroll to the end of
 * the document then closes the window the third is read over; the fourth, the reader's first
 * block from any draw, is read off the page at the end. The navigation waits for the response
 * alone (`commit`), so a document whose `load` lands long after its first block does not hold
 * the measurement up.
 *
 * The scroll waits for the editor rather than racing it, so the window covers the whole build
 * and then a scroll through the whole document (the module comment above).
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
    .poll(() => page.evaluate(() => window.__largeDocumentFirstBlock), {
      intervals: [500],
      message:
        "the editor's first visible block never came into the viewport: either no editor " +
        'root ever carried `data-write-path="live"` with a block in it, or a taller page ' +
        "header has pushed that block under the fold, where no scroll of this measurement " +
        "reaches it",
      timeout: firstBlockTimeoutMs,
    })
    .not.toBeUndefined();
  const firstBlock = await page.evaluate(() => window.__largeDocumentFirstBlock);

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
  const longTasks = await page.evaluate(() => window.__largeDocumentLongTasks);
  const windowClosedMs = await page.evaluate(() => performance.now());
  const firstPaintedMs = await page.evaluate(() => window.__largeDocumentPaintedAt);
  const pageDrawnMs = await page.evaluate(() => window.__largeDocumentPageDrawnAt);
  if (firstBlock === undefined || longestTaskMs === undefined || longTasks === undefined) {
    throw new Error("the page lost its recorders: an init script never ran");
  }
  return {
    editorOpenedMs,
    firstBlock: {
      editable: firstBlock.editable,
      inLiveEditor: firstBlock.inLiveEditor,
      inPreview: firstBlock.inPreview,
    },
    firstBlockMs: firstBlock.at,
    firstPaintedMs,
    pageDrawnMs,
    longestTaskMs,
    longTasks,
    scrollSteps,
    windowClosedMs,
  };
}

/** What the line below says about itself, for a reader who has not read this file: it is quoted
 *  in a pull request body and in a Dispatch check result, where the numbers have to tell
 *  themselves apart and say which of them a run is held to. */
const measurementLegend =
  "The first editable block is the editor's own first visible block, the first block a reader " +
  'can type in (its root carries `data-write-path="live"`); that number and the longest ' +
  "main-thread task are the two read against R11's targets, and no other number here is " +
  "asserted. First painted is the first block of the document on the screen from any draw, " +
  "which is the server's own where it drew the document's opening blocks into the page it " +
  "sent. The page's own draw is the first block the app itself put on the screen, in the " +
  "read-only preview inside the document surface it renders: that one is the number this rig " +
  "read as the editor's before INFRA-341 corrected it. Neither draw takes an edit.";

/**
 * Puts a measurement where the run's reader finds it: on the row's annotations, which the report
 * keeps with the result, and on stdout, which `--reporter line` prints as the row runs.
 * `firstBlockTargetMs` is the row's own target for the first editable block, printed beside the
 * number so the line says what each asserted number was held to.
 */
export function printMeasurement(
  what: string,
  measured: DocumentMeasurement,
  firstBlockTargetMs: number
): void {
  const over = measured.longTasks.filter(({ ms }) => ms > longTaskTargetMs);
  const line =
    `${what}: first editable block (asserted, target ${firstBlockTargetMs} ms) ` +
    `${Math.round(measured.firstBlockMs)} ms, ` +
    `longest main-thread task (asserted, target ${longTaskTargetMs} ms) ` +
    `${Math.round(measured.longestTaskMs)} ms, ` +
    `first painted (not asserted) ${measured.firstPaintedMs === undefined ? "never" : `${Math.round(measured.firstPaintedMs)} ms`}, ` +
    `page's own draw (not asserted) ${measured.pageDrawnMs === undefined ? "never" : `${Math.round(measured.pageDrawnMs)} ms`}, ` +
    `editor open (not asserted) ${measured.editorOpenedMs === undefined ? "never" : `${Math.round(measured.editorOpenedMs)} ms`}, ` +
    `${measured.longTasks.length} main-thread task(s) over 50 ms and ${over.length} over ` +
    `${longTaskTargetMs} ms` +
    (over.length === 0
      ? ""
      : ` (${over.map(({ at, ms }) => `${Math.round(ms)} ms at ${Math.round(at)} ms`).join("; ")})`) +
    `, over ${measured.scrollSteps} scroll steps in a ${Math.round(measured.windowClosedMs)} ms window. ` +
    measurementLegend;
  test.info().annotations.push({ description: line, type: "R11 measurement" });
  console.log(`R11 measurement · ${line}`);
}
