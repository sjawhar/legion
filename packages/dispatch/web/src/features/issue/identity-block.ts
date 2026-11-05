import { type RefObject, useEffect, useLayoutEffect, useState } from "react";

/**
 * How tall the identity block is right now, on the document element, so anything that must not
 * sit under it reads one number rather than guessing: the tree's focused row scrolls clear of it
 * (dispatch://INFRA-1196 R9), and a keyboard target is "on screen" only below this line
 * (`e2e/issue-page.ts`'s `onScreen`). Unset while no identity block is drawn.
 */
export const IDENTITY_HEIGHT_PROPERTY = "--issue-identity-height";

/** How far the page scrolls before the block counts as stuck, in CSS pixels: a fractional
 *  layout leaves a sticky element's top a hair below zero without the page having scrolled. */
const STUCK_SLACK = 1;

/**
 * Publishes `ref`'s height as `IDENTITY_HEIGHT_PROPERTY` and answers whether the block is stuck
 * to the top of the screen, which is when it draws its condensed form: the sticky block gives
 * up its padding and the title's larger type, so a reader scrolling the tree keeps the issue's
 * identity and every one of its controls in a shorter band rather than losing a third of the
 * screen to it. Nothing is hidden by condensing, because a property a reader cannot see is a
 * property the page does not have (R11).
 */
export function useIdentityBlock(ref: RefObject<HTMLElement | null>): { condensed: boolean } {
  const [condensed, setCondensed] = useState(false);

  // The height is measured in a layout effect, so the first frame a reader sees already carries
  // it: a row placed against it on that frame would otherwise be placed against nothing.
  useLayoutEffect(() => {
    const element = ref.current;
    if (element === null) return;
    const publish = () => {
      document.documentElement.style.setProperty(
        IDENTITY_HEIGHT_PROPERTY,
        `${Math.round(element.getBoundingClientRect().height)}px`
      );
    };
    const sizes = new ResizeObserver(publish);
    sizes.observe(element);
    publish();
    return () => {
      sizes.disconnect();
      document.documentElement.style.removeProperty(IDENTITY_HEIGHT_PROPERTY);
    };
  }, [ref]);

  useEffect(() => {
    const element = ref.current;
    if (element === null) return;
    const measure = () => {
      setCondensed(element.getBoundingClientRect().top <= STUCK_SLACK);
    };
    window.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", measure);
    measure();
    return () => {
      window.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
    };
  }, [ref]);

  return { condensed };
}
