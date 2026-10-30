// The parse worker's threshold, in a module that imports nothing, so the Playwright specs read
// the value the page runs on (`ref-preview-timing.ts` carries the hover card's timing the same
// way); `markdown-engine.ts` carries everything a page needs a browser for.

/**
 * The longest body parsed on the page's own thread. After this change's linear-time fixes the
 * worst shape costs about 46 µs per character in Chromium on a three-core pod, so a body of this
 * length holds the thread for about 0.2 s - five times under the second a reader may wait - and
 * every longer one is parsed in the worker (sjawhar/legion#2403).
 */
export const markdownWorkerThreshold = 4_000;
