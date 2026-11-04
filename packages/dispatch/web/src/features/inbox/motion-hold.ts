import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { refPreviewCardContains, refPreviewOpenedIn } from "../refs/ref-preview-card";

/**
 * Holds list data while a pointer is travelling across the list. Adoption happens once the
 * pointer has been still for `settleMs`, leaves the list, or presses inside it.
 */
export function useMotionHold<T>(data: T, root: RefObject<HTMLElement | null>, settleMs = 400): T {
  const [adopted, setAdopted] = useState(data);
  const [moving, setMoving] = useState(false);
  const movingNow = useRef(false);
  const timer = useRef<number | undefined>(undefined);
  const attached = useRef<HTMLElement | null>(null);

  const stopMoving = useCallback(() => {
    window.clearTimeout(timer.current);
    timer.current = undefined;
    if (!movingNow.current) return;
    movingNow.current = false;
    setMoving(false);
  }, []);
  const move = useCallback(() => {
    window.clearTimeout(timer.current);
    if (!movingNow.current) {
      movingNow.current = true;
      setMoving(true);
    }
    timer.current = window.setTimeout(stopMoving, settleMs);
  }, [settleMs, stopMoving]);
  /**
   * The pointer crossed the list's edge. Moving onto the hover card the list's own content
   * opened is not leaving it: a reference in a row opens a card that is `fixed` over the page
   * and outside the list (`features/refs/RefPreview.tsx`), so a pointer at rest has the card
   * appear under it and its next move lands inside the card, which the list hears as
   * `pointerleave`. Dropping the hold there moves the row the reader is reading at the next
   * refetch, and the reader left nothing. Nothing more is heard from a pointer over the card -
   * the card is no child of this root - so the settle timer is what ends the hold then, exactly
   * as it does for a pointer that simply stopped; a pointer leaving for anywhere else still
   * ends it here and now.
   */
  const leave = useCallback(
    (event: PointerEvent) => {
      const node = event.currentTarget;
      if (
        node instanceof Element &&
        refPreviewCardContains(event.relatedTarget) &&
        refPreviewOpenedIn(node)
      ) {
        return;
      }
      stopMoving();
    },
    [stopMoving]
  );

  useEffect(() => {
    const node = root.current;
    const previous = attached.current;
    if (node === previous) return;
    previous?.removeEventListener("pointermove", move);
    previous?.removeEventListener("pointerleave", leave);
    previous?.removeEventListener("pointerdown", stopMoving);
    node?.addEventListener("pointermove", move);
    node?.addEventListener("pointerleave", leave);
    node?.addEventListener("pointerdown", stopMoving);
    attached.current = node;
  });

  useEffect(
    () => () => {
      window.clearTimeout(timer.current);
      attached.current?.removeEventListener("pointermove", move);
      attached.current?.removeEventListener("pointerleave", leave);
      attached.current?.removeEventListener("pointerdown", stopMoving);
      attached.current = null;
    },
    [leave, move, stopMoving]
  );

  useLayoutEffect(() => {
    if (!moving && !Object.is(adopted, data)) setAdopted(data);
  }, [adopted, data, moving]);

  return moving ? adopted : data;
}
