import type { Locator, Page } from "@playwright/test";

export interface Point {
  x: number;
  y: number;
}

/** One finger on the screen: move it in steps, then lift it. */
export interface Finger {
  moveTo(point: Point, steps?: number): Promise<void>;
  lift(): Promise<void>;
}

/**
 * Real touch gestures for the `iphone` project. `page.mouse` on a touch context still emits
 * mouse events, which never reach dnd-kit's TouchSensor (touchstart, hold, touchmove), so the
 * board's phone behaviour - long-press lifts, swipe scrolls, tap on the badge opens the picker -
 * is driven through Chromium's `Input.dispatchTouchEvent` instead. Coordinates are CSS pixels
 * in the viewport, the same as `boundingBox()`.
 */
export async function pressFinger(page: Page, at: Point): Promise<Finger> {
  const session = await page.context().newCDPSession(page);
  let position = at;
  const dispatch = (type: "touchStart" | "touchMove" | "touchEnd", point?: Point) =>
    session.send("Input.dispatchTouchEvent", {
      touchPoints: point === undefined ? [] : [{ id: 1, x: point.x, y: point.y }],
      type,
    });
  await dispatch("touchStart", at);
  return {
    async moveTo(point, steps = 12) {
      const from = position;
      for (let step = 1; step <= steps; step += 1) {
        position = {
          x: from.x + ((point.x - from.x) * step) / steps,
          y: from.y + ((point.y - from.y) * step) / steps,
        };
        await dispatch("touchMove", position);
        await page.waitForTimeout(16);
      }
    },
    async lift() {
      try {
        await dispatch("touchEnd");
      } finally {
        await session.detach();
      }
    },
  };
}

/** Holds the finger still for `holdMs` as the page sees it. `Input.dispatchTouchEvent` resolves
 *  once the renderer has handled the touchstart, and a timer armed in the page after that fires
 *  no earlier than dnd-kit's own delay timer (armed on that touchstart, equal or shorter), so a
 *  starved main thread cannot compress the hold below the sensor's activation delay - a Node-side
 *  wait can, when the queued touchstart is handled late and the touchend right behind it. */
async function holdFinger(page: Page, holdMs: number): Promise<void> {
  await page.evaluate((ms) => {
    const held = Promise.withResolvers<void>();
    setTimeout(held.resolve, ms);
    return held.promise;
  }, holdMs);
}

/** Finger down at `from`, held `holdMs`, then moved to `to` in `steps`, then lifted. */
export async function touchDrag(
  page: Page,
  from: Point,
  to: Point,
  holdMs: number,
  steps = 12
): Promise<void> {
  const finger = await pressFinger(page, from);
  if (holdMs > 0) {
    await holdFinger(page, holdMs);
  }
  await finger.moveTo(to, steps);
  await finger.lift();
}

/** Finger down at `point`, held `holdMs` without moving, then lifted (0 = a plain tap). */
export async function touchHold(page: Page, point: Point, holdMs: number): Promise<void> {
  const finger = await pressFinger(page, point);
  if (holdMs > 0) {
    await holdFinger(page, holdMs);
  }
  await finger.lift();
}

export async function centerOf(locator: Locator): Promise<Point> {
  await locator.scrollIntoViewIfNeeded();
  const box = await locator.boundingBox();
  if (box === null) {
    throw new Error("element is not visible for a touch gesture");
  }
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}
