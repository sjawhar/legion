import { mermaidDrawsSettled, mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

/** How long the drawn document must hold its height before the focus runs again: long enough
 *  that a run of cards landing one after another is waited out once rather than chased, short
 *  enough that a reader who followed a link sees the target without reading somewhere else
 *  first. */
const QUIET_MS = 200;

/** How long a re-run is waited for at all. A drawing that never settles - a card that re-measures
 *  itself forever - must not leave the listeners armed for the life of the page, and a target
 *  moved by something that late is better left where the reader has it. */
const LIMIT_MS = 3_000;

/**
 * Runs `refocus` (every focus in the live document, through `ProofDocument`'s `focusInEditor`,
 * and a past version's highlighted mark in `VersionView`) once more after the drawing around its
 * target has settled, so the focus lands on its target whether or not everything above it had
 * drawn when it first ran.
 *
 * Two things draw after the editor has opened and scrolled to its target, and each one above the
 * target pushes it down: the page's mermaid diagrams, drawn in Dispatch's Mermaid worker
 * (`features/refs/mermaid-diagrams.ts`), and the document's own blocks - a decision block's
 * hosted card, a picture, a font - which grow `drawn`, the editor's drawn root. The focus scrolls
 * smoothly, and a script scroll during a smooth scroll cuts it short, so it runs again once, when
 * the diagrams have settled and `drawn` has held its height for `QUIET_MS`, rather than being
 * corrected as each piece lands.
 *
 * With nothing that can move the target - no diagram waiting, and a `drawn` whose height never
 * changes - it does not run again and the target is pulsed once. A caller with no drawn root to
 * name (a reading column, whose scroll container keeps its own size whatever its content does)
 * passes none and waits on the diagrams alone.
 *
 * It runs only if the reader has not pressed, scrolled, touched or typed since this was called,
 * so a reader who moved on is never pulled back. Returns the dispose, which removes the
 * listeners and drops the re-run; they also go once the re-run has run or the limit has passed.
 */
export function refocusWhenSettled(refocus: () => void, drawn?: Element): () => void {
  const diagrams = mermaidDrawsWaiting();
  if (!diagrams && drawn === undefined) return () => {};
  let done = false;
  let moved = false;
  let quiet: number | undefined;
  let limit: number | undefined;
  let sizes: ResizeObserver | undefined;
  const stop = () => {
    if (done) return;
    done = true;
    window.clearTimeout(quiet);
    window.clearTimeout(limit);
    sizes?.disconnect();
    for (const type of READER_ACTIONS) window.removeEventListener(type, stop, true);
  };
  const run = () => {
    if (done) return;
    stop();
    refocus();
  };
  // Something that can have moved the target has just happened: the re-run waits for the quiet
  // after the last of them, and for the diagrams, whichever comes later.
  const moving = () => {
    if (done) return;
    moved = true;
    window.clearTimeout(quiet);
    quiet = window.setTimeout(() => {
      if (!mermaidDrawsWaiting()) run();
    }, QUIET_MS);
  };
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  limit = window.setTimeout(() => (moved ? run() : stop()), LIMIT_MS);
  if (drawn !== undefined) {
    let height = drawn.getBoundingClientRect().height;
    sizes = new ResizeObserver(() => {
      const drawnHeight = drawn.getBoundingClientRect().height;
      if (drawnHeight === height) return;
      height = drawnHeight;
      moving();
    });
    sizes.observe(drawn);
  }
  if (diagrams) void mermaidDrawsSettled().then(moving);
  return stop;
}
