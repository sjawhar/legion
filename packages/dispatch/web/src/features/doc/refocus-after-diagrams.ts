import { mermaidDrawsSettled, mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

/**
 * Runs `refocus` (every focus in the live document, through `ProofDocument`'s `focusInEditor`,
 * and a past version's highlighted mark in `VersionView`) once more after the page's mermaid
 * diagrams have drawn, so the focus lands on its target whether or not a diagram above it had
 * drawn when it first ran. A document's diagrams draw in Dispatch's Mermaid
 * worker (`features/refs/mermaid-diagrams.ts`), after the editor opened and scrolled to its
 * target, and each one that draws above the target pushes it down by the diagram's height. The
 * focus scrolls smoothly, and a script scroll during a smooth scroll cuts it short, so the focus
 * runs again once, when no source is waiting (`mermaidDrawsSettled`), rather than being corrected
 * as each diagram lands. With none waiting when it is called (no diagram, or every one drawn
 * from the cache already) nothing can move the target, so it does not run again and the target
 * is pulsed once.
 *
 * It runs only if the reader has not pressed, scrolled, touched or typed since this was called,
 * so a reader who moved on is never pulled back. Returns the dispose, which removes the listeners
 * and drops the re-run; the listeners also go once the diagrams settle.
 */
export function refocusAfterDiagrams(refocus: () => void): () => void {
  if (!mermaidDrawsWaiting()) return () => {};
  let done = false;
  const stop = () => {
    if (done) return;
    done = true;
    for (const type of READER_ACTIONS) window.removeEventListener(type, stop, true);
  };
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  void mermaidDrawsSettled().then(() => {
    if (done) return;
    stop();
    refocus();
  });
  return stop;
}
