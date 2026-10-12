/**
 * An IntersectionObserver a test drives: every element any observer watches is recorded, and
 * `show` / `hide` report an element entering or leaving the viewport to every observer watching
 * it, as the browser would once it crossed one of the observer's thresholds. happy-dom implements
 * no intersection, so a component's on-screen rule only runs where a test says what is on screen.
 */

/** How an element sits on a screen, in pixels; by default a 100 px element wholly on an 800 px
 *  screen. */
export interface Placement {
  readonly height?: number;
  readonly screen?: number;
  readonly visible?: number;
}

export interface IntersectionControl {
  /** Every element an observer currently watches. */
  readonly observed: () => readonly Element[];
  readonly show: (element: Element, placement?: Placement) => void;
  readonly hide: (element: Element) => void;
  readonly restore: () => void;
}

export function installIntersectionObserver(): IntersectionControl {
  const original = globalThis.IntersectionObserver;
  const live = new Set<ObserverStub>();

  class ObserverStub {
    readonly targets = new Set<Element>();
    readonly callback: IntersectionObserverCallback;

    constructor(callback: IntersectionObserverCallback) {
      this.callback = callback;
      live.add(this);
    }

    disconnect(): void {
      this.targets.clear();
      live.delete(this);
    }

    observe(target: Element): void {
      this.targets.add(target);
    }

    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }

    unobserve(target: Element): void {
      this.targets.delete(target);
    }
  }

  const report = (element: Element, placement: Placement | undefined) => {
    const height = placement?.height ?? 100;
    const screen = placement?.screen ?? 800;
    const visible = placement === undefined ? height : (placement.visible ?? height);
    for (const observer of [...live]) {
      if (!observer.targets.has(element)) continue;
      // The rule reads the heights alone, so the stub fills the vertical geometry.
      const entry = {
        boundingClientRect: { height },
        intersectionRatio: height === 0 ? 0 : visible / height,
        intersectionRect: { height: visible },
        isIntersecting: visible > 0,
        rootBounds: { height: screen },
        target: element,
      } as unknown as IntersectionObserverEntry;
      observer.callback([entry], observer as unknown as IntersectionObserver);
    }
  };

  globalThis.IntersectionObserver = ObserverStub as unknown as typeof IntersectionObserver;
  return {
    hide: (element) => report(element, { visible: 0 }),
    observed: () => [...live].flatMap((observer) => [...observer.targets]),
    restore: () => {
      globalThis.IntersectionObserver = original;
    },
    show: (element, placement) => report(element, placement),
  };
}
