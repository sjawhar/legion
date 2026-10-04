import { type RefObject, useLayoutEffect } from "react";

/** How far up from the screen's bottom edge the highest composer fixed at the foot of the screen
 *  reaches, on the document element, so the shell's floating status sits above it. Unset while no
 *  composer is fixed there. */
export const FOOT_INSET_PROPERTY = "--foot-composer-inset";

/** Each element measured, and how far up from the screen's bottom edge it reaches: 0 while it is
 *  laid out in the page, hidden, or not on screen. */
const insets = new Map<Element, number>();

function publish(): void {
  const highest = Math.max(0, ...insets.values());
  const style = document.documentElement.style;
  if (highest === 0) style.removeProperty(FOOT_INSET_PROPERTY);
  else style.setProperty(FOOT_INSET_PROPERTY, `${highest}px`);
}

/**
 * While `active`, measures `ref`'s element whenever it or the screen changes size - a refusal's
 * row grows a composer upward, a widening moves it into the page - and adds how far up from the
 * screen's bottom edge it reaches to `FOOT_INSET_PROPERTY`, which holds the largest of them. It
 * counts only while the element is fixed (`position: fixed`) and has a box.
 */
export function useFootInset(ref: RefObject<HTMLElement | null>, active: boolean): void {
  useLayoutEffect(() => {
    const element = ref.current;
    if (!active || element === null) return;
    const measure = () => {
      const box = element.getBoundingClientRect();
      const fixed = box.height > 0 && getComputedStyle(element).position === "fixed";
      insets.set(element, fixed ? Math.max(0, window.innerHeight - box.top) : 0);
      publish();
    };
    const sizes = new ResizeObserver(measure);
    sizes.observe(element);
    window.addEventListener("resize", measure);
    measure();
    return () => {
      sizes.disconnect();
      window.removeEventListener("resize", measure);
      insets.delete(element);
      publish();
    };
  }, [active, ref]);
}
