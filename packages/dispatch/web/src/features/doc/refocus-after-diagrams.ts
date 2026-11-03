import { mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

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
 * So the watch reads the page every frame and calls it settled once `SETTLE_MS` pass with no
 * source waiting for the renderer and no change in the document's height. A page that moved
 * since the last focus gets another one, and the watch starts again - that focus paints more
 * blocks, which may move the target again - until the page settles without having moved, or
 * `WATCH_MS` pass. A page that never moves runs the focus no second time and the target is
 * pulsed once. The focus itself does not animate (`FOCUS_SCROLL`, `@legion/proof-editor`): over
 * a document of 400,000 characters an animated scroll was cut short and restarted by every
 * correction, and the link never landed.
 *
 * It runs only while the reader has not pressed, scrolled, touched or typed since this was
 * called, so a reader who moved on is never pulled back. Returns the dispose, which removes the
 * listeners and drops the re-run; the listeners also go once the page has settled.
 */
export function refocusAfterDiagrams(refocus: () => void): () => void {
  let done = false;
  const stop = () => {
    if (done) return;
    done = true;
    for (const type of READER_ACTIONS) window.removeEventListener(type, stop, true);
  };
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  const giveUpAt = performance.now() + WATCH_MS;
  let height = document.scrollingElement?.scrollHeight ?? 0;
  let stillSince = performance.now();
  let movedSinceFocus = false;
  const watch = () => {
    if (done) return;
    const now = performance.now();
    const nextHeight = document.scrollingElement?.scrollHeight ?? 0;
    if (nextHeight !== height || mermaidDrawsWaiting()) {
      height = nextHeight;
      stillSince = now;
      movedSinceFocus = true;
    }
    if (now >= giveUpAt) {
      stop();
      return;
    }
    if (now - stillSince >= SETTLE_MS) {
      if (!movedSinceFocus) {
        stop();
        return;
      }
      movedSinceFocus = false;
      stillSince = now;
      refocus();
    }
    requestAnimationFrame(watch);
  };
  requestAnimationFrame(watch);
  return stop;
}
