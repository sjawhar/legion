import type { Page } from "@playwright/test";

/** Waits two rendering frames in `page`. Whatever the browser has just been given - a key press,
 *  a click, a viewport change - its own handler and the render React queued for it are both done
 *  within them, so a row that reads the page after this reads what that event produced rather
 *  than what was on screen before it. */
export function nextFrames(page: Page): Promise<void> {
  return page.evaluate(() => {
    const frames = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
    return frames.promise;
  });
}
