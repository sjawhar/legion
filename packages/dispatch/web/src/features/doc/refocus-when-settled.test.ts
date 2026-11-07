import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { waitFor } from "@testing-library/react";

import { realClock, tick } from "../../__tests__/fake-clock";
import { refocusWhenSettled } from "./refocus-when-settled";

/** The observers the rows below fire by hand: happy-dom lays nothing out and watches no size, so
 *  a drawn root grows only when its test says it did. */
let observers: ResizeObserverCallback[] = [];
let originalObserver: typeof ResizeObserver;

beforeEach(() => {
  observers = [];
  originalObserver = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    observe() {}
    disconnect() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
  jest.useFakeTimers();
});

afterEach(() => {
  realClock();
  globalThis.ResizeObserver = originalObserver;
});

/** A drawn document root of a stated height, with the growth a late card gives it. */
function drawnRoot(): { element: HTMLElement; grow(by: number): void } {
  let height = 100;
  const element = document.createElement("div");
  element.getBoundingClientRect = () => ({ height }) as DOMRect;
  return {
    element,
    grow(by: number) {
      height += by;
      for (const observer of observers) {
        observer([], undefined as unknown as ResizeObserver);
      }
    },
  };
}

/** Longer than the quiet the re-run waits out, so a row proving no re-run came waits past the
 *  moment one would have; a row proving one came waits for the run itself. */
const AFTER_THE_QUIET = 300;

test("a document that grows under the target focuses it again, once", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    drawn.grow(300);
    drawn.grow(500);
    await waitFor(() => expect(focused).toBe(1));
    await tick(AFTER_THE_QUIET);
    expect(focused).toBe(1);
  } finally {
    stop();
  }
});

test("a document that keeps its height leaves the target where the focus put it", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // A resize that reports the same height - the observer's own first call - moves nothing.
    drawn.grow(0);
    await tick(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("a reader who moved on after the document grew is not pulled back", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    drawn.grow(300);
    window.dispatchEvent(new Event("wheel"));
    await tick(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("a dropped re-run never runs, however the document grows after it", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  refocusWhenSettled(() => (focused += 1), drawn.element)();

  drawn.grow(300);
  await tick(AFTER_THE_QUIET);
  expect(focused).toBe(0);
});

test("a caller with no drawn root and no diagram waiting arms nothing", async () => {
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1));

  try {
    expect(observers).toHaveLength(0);
    await tick(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});
