// The parse worker's threshold, in a module that imports nothing, so the Playwright specs read
// the value the page runs on (`ref-preview-timing.ts` carries the hover card's timing the same
// way); `markdown-engine.ts` carries everything a page needs a browser for.

/**
 * The longest body parsed on the page's own thread.
 *
 * Derived from the worst shape a page still parses itself - 49 list markers on one line, then
 * blank lines to one character under this value - in Firefox, the slowest of the three engines
 * on it, on a three-core issue pod (sjawhar/legion#2519). The Conversation parses such a body
 * twice, the card's two-line preview and the body its open thread shows, and
 * `e2e/heavy-bodies.e2e.ts` measures both parses as one main-thread gap against its 1,000 ms
 * bound. Three runs of that row at each candidate:
 *
 *     threshold   Chromium      WebKit         Firefox
 *     4,000       194-217 ms    594-1,082 ms   684-858 ms
 *     2,000       166-238 ms    338-350 ms     475-484 ms
 *     500         (not run)     199-258 ms     230-302 ms
 *
 * At 500 the body is too short to cost anything: what the row still measures there is the page's
 * own work around the draw, the floor every one of these rows carries. So the parse itself is
 * what 4,000 and 2,000 differ by - about 0.5 s of Firefox's thread against about 0.2 s - and
 * 2,000 is where the whole draw sits at half the bound in the slowest engine and a third of it
 * in the other two, where 4,000 left Firefox 126 ms of room and took WebKit over the bound
 * outright. Every longer body is parsed in the worker.
 */
export const markdownWorkerThreshold = 2_000;
