import { useCallback, useLayoutEffect, useRef } from "react";

interface ReadingAnchor {
  seq: number;
  top: number;
}

const probeSteps = 5;
const probeStepPx = 8;

// An O(1) hit test for the turn nearest the center of the Conversation's own visible portion,
// rather than a getBoundingClientRect scan over every loaded turn (hundreds of forced layout
// reads per scroll tick on a long conversation). The probe point is derived from the Conversation
// section's own bounding rect — not window.innerWidth/innerHeight — because the conversation
// column is rarely centered in (or as wide as) the viewport: the sidebar and margin panel narrow
// it well before the window itself is narrow, and hit-testing the window's center can land in a
// neighboring column entirely, missing every probe. elementFromPoint at the exact center can still
// miss within the column itself — landing on a gap, divider, or composer below the list — in which
// case this probes downward first, then upward, in small steps: a content change lands between two
// turns more often than exactly on one, and probing downward first means the reader's answer for a
// point in the gap is consistently "whichever turn is below", matching reading order.
function measureAnchor(root: HTMLElement): ReadingAnchor | null {
  const rect = root.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const visibleTop = Math.max(rect.top, 0);
  const visibleBottom = Math.min(rect.bottom, window.innerHeight);
  const y = (visibleTop + visibleBottom) / 2;
  const hit = (offset: number): HTMLElement | null =>
    document.elementFromPoint(x, y + offset)?.closest<HTMLElement>("[data-event-seq]") ?? null;

  let found = hit(0);
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(step * probeStepPx);
  }
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(-step * probeStepPx);
  }
  if (found === null || !root.contains(found)) {
    return null;
  }
  return { seq: Number(found.dataset.eventSeq), top: found.getBoundingClientRect().top };
}

/**
 * The Conversation never moves the reader's attention: keeps the turn they are looking at pinned
 * to its viewport position across renders that shift content above it (an older page loading, a
 * pin reflow, the failed-operations banner appearing, or the unread divider moving as a turn is
 * marked read). Readers at the top are left alone so new items appear in place, unchanged.
 *
 * The reader's position is tracked by a scroll listener rather than sampled inside the
 * render-triggered effect below: a reader can scroll (or scroll back) without causing any React
 * render, and compensating against whatever was true as of the last render would apply a stale
 * anchor. The listener samples synchronously on its leading edge and again on a trailing animation
 * frame, coalescing a fast scroll gesture's flood of events into one settled measurement.
 *
 * Whether to compensate is decided at render time from both the live window position and the
 * caller's current reading mode. A Conversation following the latest turn must not compensate
 * after a prepend; a reader browsing older turns may keep their anchor.
 *
 * Returns a ref callback rather than accepting a useRef object: the Conversation section may not
 * mount on its first several renders, and a callback fires exactly when it attaches or detaches.
 */
export function usePreserveReaderPosition(
  shouldCompensate: () => boolean
): (node: HTMLElement | null) => void {
  const root = useRef<HTMLElement | null>(null);
  const anchor = useRef<ReadingAnchor | null>(null);
  const detach = useRef<(() => void) | null>(null);

  const attach = useCallback((node: HTMLElement | null) => {
    detach.current?.();
    detach.current = null;
    root.current = node;
    anchor.current = node === null ? null : measureAnchor(node);
    if (node === null) {
      return;
    }
    let frame: number | null = null;
    const sample = () => {
      anchor.current = measureAnchor(node);
    };
    const onScroll = () => {
      sample();
      if (frame === null) {
        frame = window.requestAnimationFrame(() => {
          frame = null;
          sample();
        });
      }
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    detach.current = () => {
      window.removeEventListener("scroll", onScroll);
      if (frame !== null) {
        window.cancelAnimationFrame(frame);
      }
    };
  }, []);

  useLayoutEffect(() => {
    const current = root.current;
    const previous = anchor.current;
    if (current === null || previous === null || window.scrollY <= 0 || !shouldCompensate()) {
      return;
    }
    const element = current.querySelector<HTMLElement>(`[data-event-seq="${previous.seq}"]`);
    if (element === null) {
      return;
    }
    const delta = element.getBoundingClientRect().top - previous.top;
    if (delta !== 0) {
      window.scrollBy(0, delta);
    }
  });

  return attach;
}
