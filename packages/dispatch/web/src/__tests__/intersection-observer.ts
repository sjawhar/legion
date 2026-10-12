/**
 * An IntersectionObserver a test drives: every element any observer watches is recorded, and
 * `show` / `hide` report an element entering or leaving the viewport to every observer watching
 * it, as the browser would once it crossed the observer's threshold. happy-dom implements no
 * intersection, so a component's on-screen rule only runs where a test says what is on screen.
 */
export interface IntersectionControl {
  /** Every element an observer currently watches. */
  readonly observed: () => readonly Element[];
  readonly show: (element: Element) => void;
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

  const report = (element: Element, isIntersecting: boolean) => {
    for (const observer of [...live]) {
      if (!observer.targets.has(element)) continue;
      // The rule reads isIntersecting and target alone; the stub has no geometry to fill.
      const entry = {
        isIntersecting,
        intersectionRatio: isIntersecting ? 1 : 0,
        target: element,
      } as unknown as IntersectionObserverEntry;
      observer.callback([entry], observer as unknown as IntersectionObserver);
    }
  };

  globalThis.IntersectionObserver = ObserverStub as unknown as typeof IntersectionObserver;
  return {
    hide: (element) => report(element, false),
    observed: () => [...live].flatMap((observer) => [...observer.targets]),
    restore: () => {
      globalThis.IntersectionObserver = original;
    },
    show: (element) => report(element, true),
  };
}
