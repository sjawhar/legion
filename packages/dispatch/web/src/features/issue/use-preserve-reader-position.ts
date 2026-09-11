import { useCallback, useLayoutEffect, useRef } from "react";

interface ReadingAnchor {
  seq: number;
  top: number;
}

const probeSteps = 5;
const probeStepPx = 8;

// An O(1) hit test for the article nearest the center of the log column's own visible portion,
// rather than a getBoundingClientRect scan over every loaded article (hundreds of forced layout
// reads per scroll tick on a long log). The probe point is derived from the log section's own
// bounding rect — not window.innerWidth/innerHeight — because the log column is rarely centered
// in (or as wide as) the viewport: the sidebar and the margin panel narrow it well before the
// window itself is narrow, and hit-testing the window's center can land in a neighboring column
// entirely, missing every probe. elementFromPoint at the exact center can still miss within the
// column itself — landing on the inter-card gap, the "New" divider, or the composer below the
// list — in which case this probes downward first, then upward, in small steps: a content change
// lands between two cards more often than exactly on one, and probing downward first means the
// reader's answer for a point in the gap is consistently "whichever card is below", matching the
// reader's actual reading order.
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
 * Dispatch never moves the reader's attention: keeps whichever log item the reader is currently
 * looking at pinned to the same viewport position across renders that shift content above it (a
 * new event prepended via SSE, a dismiss/pin reflow, the failed-ops banner appearing, the "New"
 * divider landing as more of the log gets marked read). Readers at the top of the page are left
 * alone so new items appear in place, unchanged.
 *
 * The reader's position is tracked by a scroll listener rather than sampled inside the
 * render-triggered effect below: a reader can scroll (or scroll back) without causing any React
 * render, and compensating against whatever was true as of the *last render* would apply a stale
 * anchor. The listener samples synchronously on its leading edge — a render that lands within the
 * same frame as a scroll must never see a position from before that scroll — and again on a
 * trailing animation frame, coalescing a fast scroll gesture's flood of events into one settled
 * measurement.
 *
 * Whether to compensate at all is decided fresh at render time from the live `window.scrollY`,
 * never from the tracked anchor: even a synchronous listener only runs once the browser gets
 * around to dispatching the scroll event, which it is not guaranteed to do before an unrelated
 * render lands. Reading the live value means a reader who is actually at the top right now is
 * never pulled away from it by a compensation decided from a sample that hasn't caught up yet.
 *
 * Returns a ref callback rather than accepting a `useRef` object: the log section may not mount
 * on the first several renders (it's swapped in once loading/error states resolve), and a plain
 * `useRef`'s identity never changes to signal that. A callback ref fires exactly when the section
 * actually attaches or detaches, however many renders that takes.
 */
export function usePreserveReaderPosition(): {
  attach(node: HTMLElement | null): void;
  compensate(): void;
} {
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

  const compensate = useCallback(() => {
    const current = root.current;
    const previous = anchor.current;
    if (current === null || previous === null || window.scrollY <= 0) {
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
  }, []);

  useLayoutEffect(() => {
    compensate();
  });

  return { attach, compensate };
}
