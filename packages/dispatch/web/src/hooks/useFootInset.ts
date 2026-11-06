import { type RefObject, useLayoutEffect } from "react";

/** How far up from the screen's bottom edge the highest composer fixed at the foot of the screen
 *  reaches, on the document element, so the shell's floating status sits above it. Unset while no
 *  composer is fixed there. */
export const FOOT_COMPOSER_INSET_PROPERTY = "--foot-composer-inset";

/** How far up from the screen's bottom edge the shell's status corner - the pending-writes
 *  indicator and the connection pill (`app.tsx`) - reaches, on the document element, so a control
 *  floating at the foot of a page rides above the whole corner rather than under it. Unset while
 *  the corner shows nothing, which leaves a reader of this property the composer's alone. */
export const FOOT_STATUS_INSET_PROPERTY = "--foot-status-inset";

/** A property this hook publishes into. */
export type FootInsetProperty =
  | typeof FOOT_COMPOSER_INSET_PROPERTY
  | typeof FOOT_STATUS_INSET_PROPERTY;

/** Each property's own elements, and how far up from the screen's bottom edge each reaches: 0
 *  while it is laid out in the page, hidden, or not on screen. The measurements are kept per
 *  property, so a publisher of one never raises another's and never clears it. */
const insets = new Map<FootInsetProperty, Map<Element, number>>();

function measurementsOf(property: FootInsetProperty): Map<Element, number> {
  const measured = insets.get(property) ?? new Map<Element, number>();
  insets.set(property, measured);
  return measured;
}

function publish(property: FootInsetProperty): void {
  const highest = Math.max(0, ...measurementsOf(property).values());
  const style = document.documentElement.style;
  if (highest === 0) style.removeProperty(property);
  else style.setProperty(property, `${highest}px`);
}

/**
 * While `active`, measures `ref`'s element whenever it or the screen changes size - a refusal's
 * row grows a composer upward, a widening moves it into the page - and adds how far up from the
 * screen's bottom edge it reaches to `property`, which holds the largest of the elements
 * published into it; a composer's by default. It counts only while the element is fixed
 * (`position: fixed`) and has a box.
 */
export function useFootInset(
  ref: RefObject<HTMLElement | null>,
  active: boolean,
  property: FootInsetProperty = FOOT_COMPOSER_INSET_PROPERTY
): void {
  useLayoutEffect(() => {
    const element = ref.current;
    if (!active || element === null) return;
    const measured = measurementsOf(property);
    const measure = () => {
      const box = element.getBoundingClientRect();
      const fixed = box.height > 0 && getComputedStyle(element).position === "fixed";
      measured.set(element, fixed ? Math.max(0, window.innerHeight - box.top) : 0);
      publish(property);
    };
    const sizes = new ResizeObserver(measure);
    sizes.observe(element);
    window.addEventListener("resize", measure);
    measure();
    return () => {
      sizes.disconnect();
      window.removeEventListener("resize", measure);
      measured.delete(element);
      publish(property);
    };
  }, [active, property, ref]);
}
