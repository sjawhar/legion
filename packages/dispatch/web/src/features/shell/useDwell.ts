import { useCallback, useEffect, useRef } from "react";

/** How much of an element must be on screen, and for how long, before it counts as read. */
const DWELL_THRESHOLD = 0.75;
const DWELL_MS = 1_000;

/** The visible shares at which the observer reports, every 5% (0.75 among them), so an element's
 *  share is read again as it scrolls rather than only as it crosses one line. */
const OBSERVED_SHARES = Array.from({ length: 21 }, (_, step) => step / 20);

/** Whether an entry's element is on screen enough to read: three-quarters of it visible, or, for
 *  an element taller than the screen, which can never show three-quarters of itself, three-quarters
 *  of the screen filled by it. */
function readable(entry: IntersectionObserverEntry): boolean {
  if (!entry.isIntersecting) return false;
  const screen = entry.rootBounds?.height ?? window.innerHeight;
  const needed = DWELL_THRESHOLD * Math.min(entry.boundingClientRect.height, screen);
  return entry.intersectionRect.height >= needed;
}

export interface DwellOptions {
  /** Whether anything is watched at all; off, no observer exists. */
  readonly enabled: boolean;
  /** A change starts the watch over: every registered element is observed afresh, so one whose
   *  content changed while it stayed on screen dwells again. */
  readonly resetKey: string;
  /** Whether a visible element still needs a dwell; an element that does not starts no timer. */
  readonly shouldDwell?: (element: Element) => boolean;
  /** Called once an element has stayed at least three-quarters on screen for a second. A promise
   *  it returns that rejects re-arms the timer while the element stays on screen, so a write the
   *  server refused is sent again rather than leaving the element unread. */
  readonly onDwell: (element: Element) => unknown;
}

/** The ref callback `useDwell` returns: it registers an element for the watch and returns the
 *  cleanup that removes it, which React 19 calls when the element detaches. */
export type DwellRef = (element: Element | null) => (() => void) | undefined;

/**
 * The on-screen rule the Conversation's read mark and the Replies page's thread mark share: an
 * element at least 75% visible for 1,000 ms dwells, and an element taller than the screen dwells
 * once it fills 75% of the screen; dropping below that first cancels its timer.
 * One IntersectionObserver serves every element registered through the returned ref callback, and
 * an element whose ref is detached (it unmounted, or the ref moved to another element) leaves the
 * watch at once.
 */
export function useDwell({ enabled, resetKey, shouldDwell, onDwell }: DwellOptions): DwellRef {
  const elements = useRef(new Set<Element>());
  const observer = useRef<IntersectionObserver | undefined>(undefined);
  const latest = useRef({ onDwell, shouldDwell });
  latest.current = { onDwell, shouldDwell };

  // biome-ignore lint/correctness/useExhaustiveDependencies: resetKey is a trigger the effect does not read.
  useEffect(() => {
    if (!enabled) {
      return;
    }
    const visible = new Set<Element>();
    const timers = new Map<Element, number>();
    let active = true;
    const schedule = (target: Element) => {
      if (!active || !visible.has(target) || timers.has(target)) return;
      if (!elements.current.has(target)) return;
      if (latest.current.shouldDwell?.(target) === false) return;
      const timer = window.setTimeout(() => {
        timers.delete(target);
        if (!active || !visible.has(target) || !elements.current.has(target)) return;
        if (latest.current.shouldDwell?.(target) === false) return;
        void Promise.resolve()
          .then(() => latest.current.onDwell(target))
          .catch(() => schedule(target));
      }, DWELL_MS);
      timers.set(target, timer);
    };
    const watcher = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!readable(entry)) {
            visible.delete(entry.target);
            const timer = timers.get(entry.target);
            if (timer !== undefined) {
              window.clearTimeout(timer);
              timers.delete(entry.target);
            }
            continue;
          }
          visible.add(entry.target);
          schedule(entry.target);
        }
      },
      { threshold: OBSERVED_SHARES }
    );
    observer.current = watcher;
    for (const element of elements.current) {
      if (!element.isConnected) {
        elements.current.delete(element);
        continue;
      }
      watcher.observe(element);
    }
    return () => {
      active = false;
      observer.current = undefined;
      watcher.disconnect();
      for (const timer of timers.values()) window.clearTimeout(timer);
    };
  }, [enabled, resetKey]);
  return useCallback((element: Element | null) => {
    if (element === null) return undefined;
    elements.current.add(element);
    observer.current?.observe(element);
    return () => {
      elements.current.delete(element);
      observer.current?.unobserve(element);
    };
  }, []);
}
