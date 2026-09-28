import { type RefObject, useEffect } from "react";

/** The height, in px, that `useKeyboardFit` caps its element to while the keyboard is up. The
 *  element reads it through its `max-h-(--keyboard-fit-height)` class; unset, that declaration
 *  is invalid at computed-value time and the cap is `none`. */
const KEYBOARD_FIT_VARIABLE = "--keyboard-fit-height";

/**
 * While a text field inside `ref`'s element has focus, caps the element so its bottom edge meets
 * the visual viewport's bottom edge: the composer at the bottom of the live view then sits
 * directly above the on-screen keyboard.
 *
 * Chromium on Android does not need this - the page's `interactive-widget=resizes-content`
 * viewport shrinks the layout viewport, and the dynamic-viewport shell with it. iOS Safari
 * ignores that key and shrinks only the visual viewport, which it may also pan down the layout
 * viewport (`offsetTop`), so the visual viewport's bottom in layout coordinates is
 * `offsetTop + height`; element rectangles are measured in the same coordinates. Where the
 * layout viewport did shrink, that bottom is at or below the element's own and the cap is inert.
 */
export function useKeyboardFit(ref: RefObject<HTMLElement | null>): void {
  useEffect(() => {
    const element = ref.current;
    // `== null`: happy-dom leaves `visualViewport` undefined where the DOM types say `null`.
    const viewport = window.visualViewport;
    if (element === null || viewport == null) return;
    const fit = () => {
      const focused = document.activeElement;
      if (focused instanceof HTMLTextAreaElement && element.contains(focused)) {
        const bottom = viewport.offsetTop + viewport.height;
        const height = Math.max(0, bottom - element.getBoundingClientRect().top);
        element.style.setProperty(KEYBOARD_FIT_VARIABLE, `${height}px`);
      } else {
        element.style.removeProperty(KEYBOARD_FIT_VARIABLE);
      }
    };
    viewport.addEventListener("resize", fit);
    viewport.addEventListener("scroll", fit);
    element.addEventListener("focusin", fit);
    element.addEventListener("focusout", fit);
    return () => {
      viewport.removeEventListener("resize", fit);
      viewport.removeEventListener("scroll", fit);
      element.removeEventListener("focusin", fit);
      element.removeEventListener("focusout", fit);
      element.style.removeProperty(KEYBOARD_FIT_VARIABLE);
    };
  }, [ref]);
}
