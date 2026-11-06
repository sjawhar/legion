import { mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

/** Calls `movedOn` the first time the reader presses, scrolls, touches or types, so a
 *  correction of where the page sits never pulls back a reader who has moved on. Answers the
 *  dispose. */
function whenTheReaderMovesOn(movedOn: () => void): () => void {
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, movedOn, { capture: true, passive: true });
  }
  return () => {
    for (const type of READER_ACTIONS) window.removeEventListener(type, movedOn, true);
  };
}

/** How long the page has to hold still - no source waiting for the diagram renderer, and a
 *  document of the same height - before the watch reads it as settled. */
const SETTLE_MS = 300;

/** How long the watch runs at most. A document whose height never holds still stops being
 *  corrected rather than being corrected for ever. */
const WATCH_MS = 30_000;

/**
 * Runs `refocus` (every focus in the live document, through `ProofDocument`'s `focusInEditor`,
 * and a past version's highlighted mark in `VersionView`) again each time the page settles under
 * it, so the focus lands on its target rather than where the target stood when it first ran.
 *
 * Two things move a target after a focus. A document's diagrams draw in Dispatch's Mermaid
 * worker (`features/refs/mermaid-diagrams.ts`), each one above the target pushing it down by the
 * diagram's height, and a block the browser skipped (`content-visibility: auto`, `styles.css`)
 * stands at an estimated height until it paints at its own. Neither is settled when this is
 * called: a block asks for its diagram only once an `IntersectionObserver` reports it within one
 * viewport of the reader, and the focus's own scroll is what brings the blocks around the target
 * that near.
 *
 * So the watch reads the page every frame, in an animation frame, which the browser runs before
 * it lays the page out: a frame whose height has changed since the last one puts the reader back
 * on their target there and then, so the frame that first shows the grown page already shows
 * the target where the link left it. Waiting for the page to settle instead is what failed: the
 * diagram directly above the decision block at the end of the larger document grew from 297 to
 * 1,587 px, and the 300 ms before the correction were 300 ms with the target 1,290 px below
 * where it belonged - long enough for a reader to see it, and for a page watching the scroll
 * offset to read the page as settled (`e2e/large-documents.e2e.ts`, "a link to the decision
 * block at the end of the larger document lands on that block").
 *
 * The settle is still read, because a block the browser skipped can paint at its own height
 * without the document's total changing: `SETTLE_MS` with no source waiting for the renderer
 * and no change in height runs the focus once more where anything moved since the last one.
 * The watch runs until `WATCH_MS` pass rather than stopping at the first quiet settle, because
 * a document of this size goes quiet between its diagrams and the next one moves the target
 * just as far. A page that never moves runs the focus no second time and the target is pulsed
 * once. The focus itself does not animate (`FOCUS_SCROLL`, `@legion/proof-editor`): over a
 * document of 400,000 characters an animated scroll was cut short and restarted by every
 * correction, and the link never landed.
 *
 * It runs only while the reader has not pressed, scrolled, touched or typed since this was
 * called, so a reader who moved on is never pulled back. Returns the dispose, which removes the
 * listeners and drops the re-run; the listeners also go once the watch is over.
 */
export function refocusAfterDiagrams(refocus: () => void): () => void {
  let done = false;
  let stopListening: (() => void) | undefined;
  const stop = () => {
    if (done) return;
    done = true;
    stopListening?.();
  };
  stopListening = whenTheReaderMovesOn(stop);
  const giveUpAt = performance.now() + WATCH_MS;
  let height = document.scrollingElement?.scrollHeight ?? 0;
  let stillSince = performance.now();
  let movedSinceFocus = false;
  const watch = () => {
    if (done) return;
    const now = performance.now();
    const nextHeight = document.scrollingElement?.scrollHeight ?? 0;
    if (nextHeight !== height) {
      height = nextHeight;
      stillSince = now;
      // The page is a different height than the frame before, so the target is somewhere else
      // than the focus left it: put the reader back on it in this frame, before the browser
      // lays the page out and paints it.
      refocus();
      movedSinceFocus = false;
    } else if (mermaidDrawsWaiting()) {
      stillSince = now;
      movedSinceFocus = true;
    }
    if (now >= giveUpAt) {
      stop();
      return;
    }
    if (now - stillSince >= SETTLE_MS && movedSinceFocus) {
      movedSinceFocus = false;
      stillSince = now;
      refocus();
    }
    requestAnimationFrame(watch);
  };
  requestAnimationFrame(watch);
  return stop;
}
