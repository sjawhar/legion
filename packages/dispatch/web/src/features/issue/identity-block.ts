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
 *
 * `restoringTo` is the place the page is about to put the reader back at, in pixels down the
 * document, where it has one: a tab the reader is returning to carries the position they left
 * it at (`IssuePage`'s `panelScroll`). The block answers for *that* place rather than for the
 * page's current one, because its two forms are 26 px apart in flow height: measured after the
 * scroll, the block would condense once the page was already restored, the content above the
 * viewport would lose those pixels, and the browser's scroll anchoring would take them off the
 * reader's position - leaving a reader who left the Conversation at 500 px at 474. Answering
 * for the place first makes the block's height final in the same commit, before the restore is
 * written, so nothing moves under the reader and nothing is anchored away.
 */
export function useIdentityBlock(
  ref: RefObject<HTMLElement | null>,
  restoringTo?: number
): { condensed: boolean } {
  const [stuck, setStuck] = useState(false);

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
      setStuck(element.getBoundingClientRect().top <= STUCK_SLACK);
    };
    window.addEventListener("scroll", measure, { passive: true });
    window.addEventListener("resize", measure);
    measure();
    return () => {
      window.removeEventListener("scroll", measure);
      window.removeEventListener("resize", measure);
    };
  }, [ref]);

  // The block sits at the top of the page's own column, so a page restored past `STUCK_SLACK`
  // is a page the block is stuck on. The measurement above takes over as soon as the reader
  // scrolls again, which is why it keeps running while a restore is pending.
  return { condensed: restoringTo === undefined ? stuck : restoringTo > STUCK_SLACK };
}
