import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

const FOLLOW_POSITION_TOLERANCE_PX = 1;
const NEWEST_TURN_SELECTOR = "[data-event-seq]";

export function useFollowLatest({
  enabled,
  itemSeqs,
  ownSendCount,
}: {
  enabled: boolean;
  itemSeqs: readonly number[];
  ownSendCount: number;
}): {
  atTop: boolean;
  jumpToLatest: () => void;
  newItemCount: number;
  pinnedToTop: () => boolean;
} {
  const [atTop, setAtTop] = useState(true);
  const [newItemCount, setNewItemCount] = useState(0);
  const latestItemSeq = itemSeqs.at(0);
  const atTopRef = useRef(true);
  const previousItemSeq = useRef<number | undefined>(undefined);
  const previousSends = useRef(ownSendCount);
  const visibleThroughSeq = useRef<number | undefined>(undefined);
  const followScrollY = useRef<number | undefined>(undefined);
  const followsLatest = useCallback(
    () =>
      window.scrollY <= 0 ||
      (followScrollY.current !== undefined &&
        Math.abs(window.scrollY - followScrollY.current) <= FOLLOW_POSITION_TOLERANCE_PX),
    []
  );

  // The newest turn must sit immediately beneath the sticky composer. Recording the actual
  // post-scroll position distinguishes that automated placement from a reader's later scroll,
  // regardless of whatever height the issue header has at this breakpoint.
  const jumpToLatest = useCallback(() => {
    const newest = document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR);
    if (newest === null) {
      window.scrollTo({ top: 0 });
    } else {
      const composerHeight =
        document
          .querySelector<HTMLElement>('[aria-label="Message composer"]')
          ?.getBoundingClientRect().height ?? 0;
      const target = window.scrollY + newest.getBoundingClientRect().top - composerHeight;
      window.scrollTo({ top: Math.max(target, 0) });
    }
    followScrollY.current = window.scrollY;
    visibleThroughSeq.current = latestItemSeq;
    setNewItemCount(0);
  }, [latestItemSeq]);

  useEffect(() => {
    const onScroll = () => {
      const nextAtTop = followsLatest();
      const wasAtTop = atTopRef.current;
      atTopRef.current = nextAtTop;
      setAtTop(nextAtTop);
      if (wasAtTop !== nextAtTop) {
        visibleThroughSeq.current = latestItemSeq;
        setNewItemCount(0);
      }
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll);
  }, [followsLatest, latestItemSeq]);

  // The mutation's `onSuccess` bumps `ownSendCount` and invalidates the events query in the same
  // tick, but the SSE stream (features/api/sse.ts) can independently invalidate the same query
  // from the server push — sometimes *before* the mutation's own callback runs. `ownSendCount` and
  // `latestItemSeq` therefore change in either order across renders, not always together, so this
  // single effect reacts to a change in either: an own send always follows regardless of scroll
  // position, and — because reaching this branch already re-settles the reader at the newest turn
  // — a same-render or later arrival from anyone else is never mistaken for the send once
  // `ownSendCount` has been recorded.
  useLayoutEffect(() => {
    if (!enabled) {
      return;
    }
    const ownSend = ownSendCount !== previousSends.current;
    previousSends.current = ownSendCount;
    if (latestItemSeq === undefined) {
      return;
    }
    const first = previousItemSeq.current === undefined;
    const changed = previousItemSeq.current !== latestItemSeq;
    previousItemSeq.current = latestItemSeq;
    if (!changed && !ownSend) {
      return;
    }
    // A server event may render before the browser dispatches the scroll event that preceded it.
    // Measure now rather than trusting the listener's last state, or that arrival can pull a reader
    // back to the newest turn before their scroll was observed.
    const followsLatestNow = first || ownSend || followsLatest();
    atTopRef.current = followsLatestNow;
    setAtTop(followsLatestNow);
    if (followsLatestNow) {
      jumpToLatest();
      return;
    }
    const visibleThrough = visibleThroughSeq.current ?? latestItemSeq;
    setNewItemCount(itemSeqs.filter((seq) => seq > visibleThrough).length);
  }, [enabled, followsLatest, itemSeqs, jumpToLatest, latestItemSeq, ownSendCount]);

  return {
    atTop,
    jumpToLatest,
    newItemCount,
    pinnedToTop: followsLatest,
  };
}
