import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

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

  useEffect(() => {
    const node = root.current;
    const previous = attached.current;
    if (node === previous) return;
    previous?.removeEventListener("pointermove", move);
    previous?.removeEventListener("pointerleave", stopMoving);
    previous?.removeEventListener("pointerdown", stopMoving);
    node?.addEventListener("pointermove", move);
    node?.addEventListener("pointerleave", stopMoving);
    node?.addEventListener("pointerdown", stopMoving);
    attached.current = node;
  });

  useEffect(
    () => () => {
      window.clearTimeout(timer.current);
      attached.current?.removeEventListener("pointermove", move);
      attached.current?.removeEventListener("pointerleave", stopMoving);
      attached.current?.removeEventListener("pointerdown", stopMoving);
      attached.current = null;
    },
    [move, stopMoving]
  );

  useLayoutEffect(() => {
    if (!moving && !Object.is(adopted, data)) setAdopted(data);
  }, [adopted, data, moving]);

  return moving ? adopted : data;
}
