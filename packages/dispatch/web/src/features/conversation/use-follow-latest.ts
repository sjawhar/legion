import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import { onMarkdownLatePaint } from "../refs/useRenderedMarkdown";

const FOLLOW_POSITION_TOLERANCE_PX = 1;
/** Every card and reply carries `data-turn`; the first in the document is the newest card. */
const NEWEST_TURN_SELECTOR = "[data-turn]";

/**
 * The box the turns scroll in: the nearest ancestor of the newest card that scrolls down of its
 * own accord, and the window where there is none. The issue's conversation is drawn in the
 * page's one right-hand panel, which is a scrollport of its own at every width the margin keeps
 * a column at (`features/margin/MarginSheet.tsx`'s `h-dvh overflow-y-auto`), so a placement that
 * scrolled the window alone never moved the reader to the newest card there at all.
 */
function scrollportOf(element: Element | null): HTMLElement | undefined {
  const root = element?.ownerDocument.documentElement;
  for (
    let node = element?.parentElement;
    node !== null && node !== undefined && node !== root;
    node = node.parentElement
  ) {
    const overflow = getComputedStyle(node).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && node.scrollHeight > node.clientHeight) {
      return node;
    }
  }
  return undefined;
}

/** Where the turns' own scrollport stands, which is the window's scroll where there is none. */
function positionIn(port: HTMLElement | undefined): number {
  return port === undefined ? window.scrollY : port.scrollTop;
}

/** The box the issue's conversation scrolls in as the page stands, and `undefined` while the
 *  page itself is that box: what a page recording where a reader was in the conversation has
 *  to read and write back (`features/issue/IssuePage.tsx`). */
export function conversationScrollport(): HTMLElement | undefined {
  return scrollportOf(document.querySelector(NEWEST_TURN_SELECTOR));
}

export function useFollowLatest({
  enabled,
  latestItemSeq,
  onFirstPlacement,
  ownSendCount,
}: {
  enabled: boolean;
  /** A number that rises with the newest card's recency, or `undefined` until that card is on
   *  the page (while the caller's list is loading, failed or empty): the hook follows that card
   *  and needs nothing else about the list. It spends its one first placement on the first
   *  defined value, so a caller must not pass one before the card renders. */
  latestItemSeq: number | undefined;
  /** Called once, in the layout effect that spends the first placement, right after the reader
   *  is placed at the newest card - so before any parent layout effect of that commit. It tells
   *  the caller the reader has just been placed for the first time: a scroll position recorded
   *  before the list was ever on the page (a tab switch's, say) is stale and must not be
   *  applied over this placement. A later arrival, own send or re-enable never calls it. */
  onFirstPlacement?: () => void;
  ownSendCount: number;
}): {
  atTop: boolean;
  jumpToLatest: () => void;
  pinnedToTop: () => boolean;
} {
  const [atTop, setAtTop] = useState(true);
  const atTopRef = useRef(true);
  const previousItemSeq = useRef<number | undefined>(undefined);
  const previousSends = useRef(ownSendCount);
  const followScrollY = useRef<number | undefined>(undefined);
  // The latest callback, read when the placement happens, so a new identity never re-runs the
  // placement effect. Declared before it, so it is current in the same commit.
  const onFirstPlacementRef = useRef(onFirstPlacement);
  useLayoutEffect(() => {
    onFirstPlacementRef.current = onFirstPlacement;
  });
  const followsLatest = useCallback(() => {
    const at = positionIn(scrollportOf(document.querySelector(NEWEST_TURN_SELECTOR)));
    return (
      at <= 0 ||
      (followScrollY.current !== undefined &&
        Math.abs(at - followScrollY.current) <= FOLLOW_POSITION_TOLERANCE_PX)
    );
  }, []);

  // The newest turn must sit immediately beneath the sticky composer, in whichever box the
  // turns scroll in. Recording the actual post-scroll position distinguishes that automated
  // placement from a reader's later scroll, regardless of whatever height the issue header has
  // at this breakpoint.
  const jumpToLatest = useCallback(() => {
    const newest = document.querySelector<HTMLElement>(NEWEST_TURN_SELECTOR);
    const port = scrollportOf(newest);
    if (newest === null) {
      (port ?? window).scrollTo({ top: 0 });
    } else {
      const composerHeight =
        document
          .querySelector<HTMLElement>('[aria-label="Comment composer"]')
          ?.getBoundingClientRect().height ?? 0;
      const above = port === undefined ? 0 : port.getBoundingClientRect().top;
      const target = positionIn(port) + newest.getBoundingClientRect().top - above - composerHeight;
      (port ?? window).scrollTo({ top: Math.max(target, 0) });
    }
    followScrollY.current = positionIn(port);
  }, []);

  useEffect(() => {
    const onScroll = () => {
      const nextAtTop = followsLatest();
      atTopRef.current = nextAtTop;
      setAtTop(nextAtTop);
    };
    // The scrollport is found afresh on every scroll: it is the window until the panel the
    // turns are drawn in has more than one screenful in it, and the listener sees both.
    window.addEventListener("scroll", onScroll, { capture: true, passive: true });
    onScroll();
    return () => window.removeEventListener("scroll", onScroll, { capture: true });
  }, [followsLatest]);

  // The mutation's `onSuccess` bumps `ownSendCount` and invalidates the events query in the same
  // tick, but the event feed (api/event-feed.ts) can independently invalidate the same query
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
    }
    if (first) {
      onFirstPlacementRef.current?.();
    }
  }, [enabled, followsLatest, jumpToLatest, latestItemSeq, ownSendCount]);

  // A body longer than the parse worker's threshold formats a frame or more after the commit
  // that drew it (`refs/markdown-engine.ts`), which is after the placement above: where the turn
  // that just formatted is the newest one and the reader is still at it, it is placed again,
  // once per late paint. A reader who has scrolled away keeps their place: `followsLatest` is
  // what says so, as it does for an arrival.
  useEffect(
    () =>
      onMarkdownLatePaint((element) => {
        if (!enabled || !followsLatest()) {
          return;
        }
        if (document.querySelector(NEWEST_TURN_SELECTOR)?.contains(element) === true) {
          jumpToLatest();
        }
      }),
    [enabled, followsLatest, jumpToLatest]
  );

  return {
    atTop,
    jumpToLatest,
    pinnedToTop: followsLatest,
  };
}
