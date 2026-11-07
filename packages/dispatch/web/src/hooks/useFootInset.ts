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

/** One element this hook holds: how far up from the screen's bottom edge it reaches - 0 while it
 *  is laid out in the page, hidden, or not on screen - and the measurement that reads that again
 *  where the element now stands. */
type FootControl = { measure: () => void; reach: number };

/** Each property's own elements. The measurements are kept per property, so a publisher of one
 *  never raises another's and never clears it. */
const insets = new Map<FootInsetProperty, Map<Element, FootControl>>();

function controlsOf(property: FootInsetProperty): Map<Element, FootControl> {
  const controls = insets.get(property) ?? new Map<Element, FootControl>();
  insets.set(property, controls);
  return controls;
}

/**
 * Publishes `property`'s highest reach, and, when that changes the value the document element
 * holds, measures every element the hook holds again, in either property.
 *
 * These properties place controls, so a publish moves controls that never change size: the
 * shell's status corner sits on `--foot-composer-inset`, so a composer that grows pushes the
 * corner up the screen with its box untouched. Neither a `ResizeObserver` on the corner nor a
 * `window` resize reports a move, so without this the corner would keep publishing the reach it
 * had where it stood before, and `Jump to latest`, placed by that reach, would land inside the
 * composer.
 *
 * The comparison is against the property as the document element holds it, so what decides a
 * cascade is the published value itself rather than a number kept beside it. That is also what
 * ends the recursion: a publish whose value is unchanged measures nothing again. The composer
 * grows and publishes, the corner is measured where it now stands and publishes its own reach,
 * that measures the composer again - which the status inset does not move, so its value is
 * unchanged - and the cascade stops.
 */
function publish(property: FootInsetProperty): void {
  const reaches = [...controlsOf(property).values()].map((control) => control.reach);
  const highest = Math.max(0, ...reaches);
  const style = document.documentElement.style;
  const value = highest === 0 ? "" : `${highest}px`;
  if (style.getPropertyValue(property) === value) return;
  if (value === "") style.removeProperty(property);
  else style.setProperty(property, value);
  for (const controls of insets.values()) {
    for (const control of controls.values()) control.measure();
  }
}

/**
 * While `active`, measures `ref`'s element whenever it or the screen changes size - a refusal's
 * row grows a composer upward, a widening moves it into the page - or whenever a published inset
 * moves it, and adds how far up from the screen's bottom edge it reaches to `property`, which
 * holds the largest of the elements published into it; a composer's by default. It counts only
 * while the element is fixed (`position: fixed`) and has a box.
 */
export function useFootInset(
  ref: RefObject<HTMLElement | null>,
  active: boolean,
  property: FootInsetProperty = FOOT_COMPOSER_INSET_PROPERTY
): void {
  useLayoutEffect(() => {
    const element = ref.current;
    if (!active || element === null) return;
    const controls = controlsOf(property);
    // Reading the box flushes the style and layout a cascading publish has just changed, so the
    // element is measured where the new insets put it, not where it was painted last.
    const control: FootControl = {
      measure: () => {
        const box = element.getBoundingClientRect();
        const fixed = box.height > 0 && getComputedStyle(element).position === "fixed";
        control.reach = fixed ? Math.max(0, window.innerHeight - box.top) : 0;
        publish(property);
      },
      reach: 0,
    };
    controls.set(element, control);
    const sizes = new ResizeObserver(control.measure);
    sizes.observe(element);
    window.addEventListener("resize", control.measure);
    control.measure();
    return () => {
      sizes.disconnect();
      window.removeEventListener("resize", control.measure);
      controls.delete(element);
      publish(property);
    };
  }, [active, property, ref]);
}
