import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

const TOP_SLACK_PX = 64;
const COMPOSER_SELECTOR = '[aria-label="Message composer"]';
const NEWEST_TURN_SELECTOR = "[data-event-seq]";

function composerHeight(): number {
  return (
    document.querySelector<HTMLElement>(COMPOSER_SELECTOR)?.getBoundingClientRect().height ?? 0
  );
}

function pinnedToTop(): boolean {
  const newest = document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR);
  if (newest === null) {
    return window.scrollY <= TOP_SLACK_PX;
  }
  return Math.abs(newest.getBoundingClientRect().top - composerHeight()) <= TOP_SLACK_PX;
}

/**
 * Scrolls the newest turn into view directly under the sticky composer. Scrolling to the document
 * top would leave the composer — itself `position: sticky` — rendered over however much of the
 * newest turn its own height covers: the composer and the turns below it keep their pre-scroll
 * document position, and only the composer's own paint position moves to the viewport top once
 * scrolled far enough. The target position places the newest turn's top edge exactly one composer
 * height below the viewport top, computed from its current on-screen position rather than its
 * document offset, so it is correct regardless of whether the composer is already stuck.
 */
function scrollToNewest(): void {
  const newest = document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR);
  if (newest === null) {
    window.scrollTo({ top: 0 });
    return;
  }
  const target = window.scrollY + newest.getBoundingClientRect().top - composerHeight();
  window.scrollTo({ top: Math.max(target, 0) });
}

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

  const jumpToLatest = useCallback(() => {
    scrollToNewest();
    visibleThroughSeq.current = latestItemSeq;
    setNewItemCount(0);
  }, [latestItemSeq]);

  useEffect(() => {
    const onScroll = () => {
      const nextAtTop = pinnedToTop();
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
  }, [latestItemSeq]);

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
    if (first || ownSend || atTopRef.current) {
      jumpToLatest();
      return;
    }
    const visibleThrough = visibleThroughSeq.current ?? latestItemSeq;
    setNewItemCount(itemSeqs.filter((seq) => seq > visibleThrough).length);
  }, [enabled, itemSeqs, jumpToLatest, latestItemSeq, ownSendCount]);

  return {
    atTop,
    jumpToLatest,
    newItemCount,
    pinnedToTop: () => atTop || pinnedToTop(),
  };
}
