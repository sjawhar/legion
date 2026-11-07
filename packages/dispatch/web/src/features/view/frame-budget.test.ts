/**
 * The one contract of the budget that crosses a chunk boundary: a frame slot is taken out of the
 * budget by the `VIEW_FRAME_RELEASE` event dispatched on it as well as by the call
 * `registerViewFrame` answers with. The editor's node view has only the event - it is drawn on
 * every document, while this module is behind `ViewPortals`' `import()` - so nothing links the
 * two sides at compile time and this is what holds them together (`features/doc/view-block.ts`).
 *
 * Each row asserts the freed place is taken, which is the whole point of releasing: a block that
 * was waiting runs. The budget tells no slot it has released that it stopped, since the block
 * that held it is unmounting or already gone.
 */
import { afterEach, beforeEach, expect, test } from "bun:test";

import {
  type IntersectionControl,
  installIntersectionObserver,
} from "../../__tests__/intersection-observer";
import { MAX_RUNNING_FRAMES, registerViewFrame } from "./frame-budget";
import { VIEW_FRAME_RELEASE } from "./view-slots";

/** happy-dom reports no resize, and the budget only re-ranks on what it reports, so the rows
 *  drive the ranking through the intersection stub alone. */
class ResizeObserverStub {
  disconnect(): void {}
  observe(): void {}
  unobserve(): void {}
}

let control: IntersectionControl;
let originalResize: typeof globalThis.ResizeObserver;
/** Every slot of the row, and the call that takes it out of the budget: the row releases what it
 *  has not released itself, since the budget's registry outlives one test. */
let registered: { element: HTMLElement; release: () => void; running: boolean[] }[] = [];

beforeEach(() => {
  control = installIntersectionObserver();
  originalResize = globalThis.ResizeObserver;
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof globalThis.ResizeObserver;
  registered = [];
});

afterEach(() => {
  for (const slot of registered) {
    slot.release();
  }
  document.body.replaceChildren();
  control.restore();
  globalThis.ResizeObserver = originalResize;
});

/** `count` frame slots in the document, each registered with the budget and each on screen, so
 *  the nearest `MAX_RUNNING_FRAMES` of them run and the rest wait. */
function slotsOnScreen(count: number): HTMLElement[] {
  const elements = Array.from({ length: count }, () => document.createElement("div"));
  document.body.append(...elements);
  for (const element of elements) {
    const running: boolean[] = [];
    const release = registerViewFrame(element, (next) => running.push(next));
    registered.push({ element, release, running });
  }
  for (const element of elements) {
    control.show(element);
  }
  return elements;
}

/** Which of the row's slots the budget says are running now. */
function runningSlots(): number[] {
  return registered.flatMap(({ running }, index) =>
    running[running.length - 1] === true ? [index] : []
  );
}

test("a slot is released by the event its node view dispatches, and a waiting block takes its place", () => {
  const elements = slotsOnScreen(MAX_RUNNING_FRAMES + 1);
  expect(runningSlots()).toEqual([0, 1, 2, 3]);

  elements[0].dispatchEvent(new Event(VIEW_FRAME_RELEASE));

  expect(runningSlots()).toEqual([0, 1, 2, 3, 4]);
  expect(registered[4].running).toEqual([true]);
  // The released slot is out of the budget: no observer watches it any more.
  expect(control.observed()).not.toContain(elements[0]);
});

test("a slot released twice, by either route and in either order, is released once", () => {
  const elements = slotsOnScreen(MAX_RUNNING_FRAMES + 2);
  expect(runningSlots()).toEqual([0, 1, 2, 3]);

  elements[0].dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  elements[0].dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  // The call the block holds, after the event has already taken the slot.
  registered[0].release();
  expect(runningSlots()).toEqual([0, 1, 2, 3, 4]);

  // And the other order: the call first, then the event the node view dispatches as it is
  // destroyed. One place is freed, so one waiting block starts and no second does.
  registered[1].release();
  elements[1].dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  expect(runningSlots()).toEqual([0, 1, 2, 3, 4, 5]);
});

test("a slot the budget never held hears the event and nothing happens", () => {
  const elements = slotsOnScreen(MAX_RUNNING_FRAMES);
  const unheld = document.createElement("div");
  document.body.append(unheld);

  unheld.dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  // A block whose renderer never mounted holds no place, so the four that do keep theirs.
  expect(runningSlots()).toEqual([0, 1, 2, 3]);
  expect(control.observed()).not.toContain(unheld);
  expect(elements).toHaveLength(MAX_RUNNING_FRAMES);
});

test("a slot registered again after its event is released by the next event alone", () => {
  const elements = slotsOnScreen(MAX_RUNNING_FRAMES + 1);
  elements[0].dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  expect(runningSlots()).toEqual([0, 1, 2, 3, 4]);

  // The block mounts again - a reader scrolls back, or the editor draws the node anew - and the
  // budget holds the same element a second time. It is first in the document, so it takes a
  // place back and the last block waits again.
  const again: boolean[] = [];
  const release = registerViewFrame(elements[0], (next) => again.push(next));
  registered[0] = { element: elements[0], release, running: again };
  control.show(elements[0]);
  expect(again).toEqual([true]);
  expect(runningSlots()).toEqual([0, 1, 2, 3]);

  // One listener answers for one registration: the second registration's event releases it, and
  // the place goes back to the block that was waiting.
  elements[0].dispatchEvent(new Event(VIEW_FRAME_RELEASE));
  expect(control.observed()).not.toContain(elements[0]);
  expect(runningSlots()).toEqual([0, 1, 2, 3, 4]);
});
