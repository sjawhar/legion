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

type EntryListener = (entry: IntersectionObserverEntry) => void;

/** The one IntersectionObserver every `useDwell` on the page shares, and who listens for each
 *  element it watches. It is dropped once it watches nothing, so the next watch builds a new one. */
let shared:
  | {
      readonly listeners: Map<Element, Set<EntryListener>>;
      readonly observer: IntersectionObserver;
    }
  | undefined;

/** Hands `listener` every entry the shared observer reports for `element`, and returns the call
 *  that stops it. A listener joining an element already watched re-observes it, since the browser
 *  reports an element's current state only to a fresh observe. An observer built from another
 *  `IntersectionObserver` than the current one (a unit test installs its own) is not reused; the
 *  stops of the elements it still watches close over it, so they still reach it. */
function watch(element: Element, listener: EntryListener): () => void {
  if (shared === undefined || !(shared.observer instanceof IntersectionObserver)) {
    const listeners = new Map<Element, Set<EntryListener>>();
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          for (const listen of [...(listeners.get(entry.target) ?? [])]) listen(entry);
        }
      },
      { threshold: OBSERVED_SHARES }
    );
    shared = { listeners, observer };
  }
  const { listeners, observer } = shared;
  const forElement = listeners.get(element) ?? new Set<EntryListener>();
  if (forElement.size > 0) observer.unobserve(element);
  forElement.add(listener);
  listeners.set(element, forElement);
  observer.observe(element);
  return () => {
    if (!forElement.delete(listener) || forElement.size > 0) return;
    listeners.delete(element);
    observer.unobserve(element);
    if (listeners.size > 0) return;
    observer.disconnect();
    if (shared?.observer === observer) shared = undefined;
  };
}

/**
 * The on-screen rule the Conversation's read mark and the Replies page's thread mark share: an
 * element at least 75% visible for 1,000 ms dwells, and an element taller than the screen dwells
 * once it fills 75% of the screen; dropping below that first cancels its timer.
 * One IntersectionObserver serves every element any `useDwell` on the page registers through its
 * returned ref callback, and an element whose ref is detached (it unmounted, or the ref moved to
 * another element) leaves the watch at once.
 */
export function useDwell({ enabled, resetKey, shouldDwell, onDwell }: DwellOptions): DwellRef {
  const elements = useRef(new Set<Element>());
  const attach = useRef<
    | { readonly start: (element: Element) => void; readonly stop: (element: Element) => void }
    | undefined
  >(undefined);
  const latest = useRef({ onDwell, shouldDwell });
  latest.current = { onDwell, shouldDwell };

  // biome-ignore lint/correctness/useExhaustiveDependencies: resetKey is a trigger the effect does not read.
  useEffect(() => {
    if (!enabled) {
      return;
    }
    const visible = new Set<Element>();
    const timers = new Map<Element, number>();
    const stops = new Map<Element, () => void>();
    let active = true;
    const cancel = (target: Element) => {
      const timer = timers.get(target);
      if (timer === undefined) return;
      window.clearTimeout(timer);
      timers.delete(target);
    };
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
    const onEntry = (entry: IntersectionObserverEntry) => {
      if (!readable(entry)) {
        visible.delete(entry.target);
        cancel(entry.target);
        return;
      }
      visible.add(entry.target);
      schedule(entry.target);
    };
    const start = (element: Element) => {
      if (!stops.has(element)) stops.set(element, watch(element, onEntry));
    };
    const stop = (element: Element) => {
      stops.get(element)?.();
      stops.delete(element);
      visible.delete(element);
      cancel(element);
    };
    attach.current = { start, stop };
    for (const element of elements.current) {
      if (!element.isConnected) {
        elements.current.delete(element);
        continue;
      }
      start(element);
    }
    return () => {
      active = false;
      attach.current = undefined;
      for (const unwatch of stops.values()) unwatch();
      stops.clear();
      for (const timer of timers.values()) window.clearTimeout(timer);
    };
  }, [enabled, resetKey]);
  return useCallback((element: Element | null) => {
    if (element === null) return undefined;
    elements.current.add(element);
    attach.current?.start(element);
    return () => {
      elements.current.delete(element);
      attach.current?.stop(element);
    };
  }, []);
}
