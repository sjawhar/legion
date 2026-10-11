import { expect, type Locator, type Page } from "@playwright/test";

/**
 * Nothing between `locator`'s own deepest content and the document clips it: no line-clamp or
 * ellipsis anywhere from its first text node up through every ancestor, and no ancestor actually
 * set to clip its own overflow (`overflow` other than `visible`) has grown past the box it sits
 * in. A page taller than its viewport scrolling normally (`overflowY: visible` on `html`) is not
 * a clip, so the walk never flags that; an ancestor set to clip, with content that no longer fits
 * it, is. The walk starts at the deepest descendant rather than `locator` itself because a clamp
 * can live on an inner span the locator wraps, not on the element the locator resolves to
 * (LEGION-559's issue title: a `line-clamp-2` span nested inside the `h1` the heading role
 * resolves to); descending first and climbing from there visits that span, then the heading,
 * then its ancestors, in one pass that also covers the ordinary case where `locator` already is
 * the clamped element (approval.e2e.ts's long-question `<p>`, whose only child is its text node).
 */
export async function assertWhole(locator: Locator): Promise<void> {
  const clips = await locator.evaluate((element) => {
    const found: string[] = [];
    let deepest: Node = element;
    while (deepest.firstChild !== null) {
      deepest = deepest.firstChild;
    }
    for (
      let node: Element | null = deepest.parentElement;
      node !== null;
      node = node.parentElement
    ) {
      const style = getComputedStyle(node);
      if (style.webkitLineClamp !== "none" || style.textOverflow === "ellipsis") {
        found.push(`${node.tagName}: line clamp ${style.webkitLineClamp}, ${style.textOverflow}`);
      }
      if (
        (style.overflowY !== "visible" && node.scrollHeight > node.clientHeight + 1) ||
        (style.overflowX !== "visible" && node.scrollWidth > node.clientWidth + 1)
      ) {
        found.push(
          `${node.tagName}: ${node.scrollWidth}x${node.scrollHeight} in ${node.clientWidth}x${node.clientHeight}`
        );
      }
    }
    return found;
  });
  expect(clips, clips.join(", ")).toEqual([]);
}

/**
 * Nothing on the page is wider than the viewport the browser was given. A phone zooms out to fit
 * a page wider than the device, and which measure that zoom moves depends on the page: the
 * visual viewport can grow, so `window.innerWidth` exceeds the device width while `clientWidth`
 * stays 390 (an unbroken document title in a flex row), or the layout viewport can widen with
 * it, so `scrollWidth` alone, compared against the zoomed-out `clientWidth`, passes (a 1,000-
 * character search chip). So both are checked: `innerWidth` is still the viewport's width, and
 * nothing is wider than `clientWidth`. Both are whole pixels, so the comparisons are exact.
 */
export async function assertPageFits(page: Page): Promise<void> {
  const layout = await page.evaluate(() => ({
    clientWidth: document.documentElement.clientWidth,
    innerWidth: window.innerWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(
    layout.innerWidth,
    `the page zoomed out to ${layout.innerWidth}px to fit content wider than the viewport`
  ).toBe(page.viewportSize()?.width);
  expect(
    layout.scrollWidth,
    `page ${layout.scrollWidth}px wide in a ${layout.clientWidth}px viewport`
  ).toBeLessThanOrEqual(layout.clientWidth);
}
