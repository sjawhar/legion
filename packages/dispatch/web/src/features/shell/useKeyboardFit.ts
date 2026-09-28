import { type RefObject, useLayoutEffect } from "react";

/**
 * While `enabled` and a textarea inside `ref`'s element has focus, caps the element's inline
 * `max-height` so its bottom edge meets the visual viewport's bottom edge. The shell passes
 * `<main>` on a route that fills the viewport, so the page's composer, the textarea at its
 * bottom, keeps `<main>`'s bottom gutter above the on-screen keyboard. The cap is written
 * synchronously in the event handler, so the frame after a resize is already capped; keep it
 * there (no `requestAnimationFrame`, no state-keyed effect). The effect also fits once as it
 * runs, before the first paint: a textarea already focused when the route mounts (focus restored
 * with the keyboard up) fires no event the listeners could hear. A disabled element is never
 * capped.
 *
 * Chromium on Android does not need this - the page's `interactive-widget=resizes-content`
 * viewport shrinks the layout viewport, and the dynamic-viewport shell with it. iOS Safari
 * ignores that key and shrinks only the visual viewport, which it may also pan down the layout
 * viewport (`offsetTop`), so the visual viewport's bottom in layout coordinates is
 * `offsetTop + height`; element rectangles are measured in the same coordinates. Where the
 * layout viewport did shrink, that bottom is at or below the element's own and the cap is inert.
 */
export function useKeyboardFit(ref: RefObject<HTMLElement | null>, enabled: boolean): void {
  useLayoutEffect(() => {
    const element = ref.current;
    // `== null`: happy-dom leaves `visualViewport` undefined where the DOM types say `null`.
    const viewport = window.visualViewport;
    if (!enabled || element === null || viewport == null) return;
    const fit = () => {
      const focused = document.activeElement;
      if (focused instanceof HTMLTextAreaElement && element.contains(focused)) {
        const bottom = viewport.offsetTop + viewport.height;
        element.style.maxHeight = `${Math.max(0, bottom - element.getBoundingClientRect().top)}px`;
      } else {
        element.style.removeProperty("max-height");
      }
    };
    viewport.addEventListener("resize", fit);
    viewport.addEventListener("scroll", fit);
    element.addEventListener("focusin", fit);
    element.addEventListener("focusout", fit);
    fit();
    return () => {
      viewport.removeEventListener("resize", fit);
      viewport.removeEventListener("scroll", fit);
      element.removeEventListener("focusin", fit);
      element.removeEventListener("focusout", fit);
      element.style.removeProperty("max-height");
    };
  }, [ref, enabled]);
}
