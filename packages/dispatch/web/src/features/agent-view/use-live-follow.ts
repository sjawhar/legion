/**
 * Scrolling for the agent live view's transcript (`features/agent-view/AgentThread.tsx`): whether
 * the viewport follows what the session is writing, how many turns have arrived since the reader
 * stopped following, and the jump back to the live end. The view owns this rather than leaving it
 * to assistant-ui's viewport, whose auto-scroll yanks the reader to the bottom every time the
 * session starts a turn, so reading back through history while the session works is impossible.
 *
 * Deliberately not `features/conversation/use-follow-latest.ts`, the issue page's hook: that one
 * scrolls the window with the newest turn pinned at the top under the composer, this one scrolls
 * an element with the newest turn at the bottom, and only a live session rebuilds its whole
 * transcript from a replay (rule: `replayCount`). One shared hook would be a flag per difference.
 */
import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

/** assistant-ui's own tolerance: a sub-pixel remainder is still the end of the transcript. */
const END_TOLERANCE_PX = 1;
const TURN_ID_ATTRIBUTE = "data-message-id";
const TURN_SELECTOR = `[${TURN_ID_ATTRIBUTE}]`;

/** What the view renders its own scrolling from. */
export interface LiveFollowControl {
  /** Whether the viewport is following the session's newest output. */
  atEnd: boolean;
  jumpToEnd: () => void;
  /** Turns that have arrived since the reader stopped following. */
  newCount: number;
}

/** The turn a reader who has stopped following was reading, and where it sat on their screen. */
interface RememberedTurn {
  readonly id: string;
  /** Its top edge in pixels below the viewport's own top edge; negative once scrolled past. */
  readonly top: number;
}

/** A transcript shorter than the viewport is always at its live end. */
function atLiveEnd(element: HTMLElement): boolean {
  return (
    element.scrollHeight - element.scrollTop - element.clientHeight <= END_TOLERANCE_PX ||
    element.scrollHeight <= element.clientHeight
  );
}

/** The topmost turn the viewport shows any part of, with its offset from the viewport's top edge.
 *  `querySelectorAll` answers in document order, so the first hit is the topmost. */
function rememberTurn(element: HTMLElement): RememberedTurn | null {
  const top = element.getBoundingClientRect().top;
  const bottom = top + element.clientHeight;
  for (const turn of element.querySelectorAll<HTMLElement>(TURN_SELECTOR)) {
    const box = turn.getBoundingClientRect();
    const id = turn.getAttribute(TURN_ID_ATTRIBUTE);
    if (box.bottom > top && box.top < bottom && id !== null) {
      return { id, top: box.top - top };
    }
  }
  return null;
}

export function useLiveFollow({
  contentIds,
  ownSendCount,
  replayCount,
  sessionId,
  viewport,
}: {
  /** Every turn in the transcript, in the order it is rendered, by the key the DOM carries in
   *  `data-message-id`. */
  contentIds: readonly string[];
  /** Bumped once per message this viewer sent that the session accepted. */
  ownSendCount: number;
  /** Bumped once per rebuild of the whole transcript from the session's replay. */
  replayCount: number;
  sessionId: string;
  /** The scroller. */
  viewport: RefObject<HTMLElement | null>;
}): LiveFollowControl {
  // A fresh mount - an open, a reload - follows.
  const [atEnd, setAtEnd] = useState(true);
  const [newCount, setNewCount] = useState(0);
  const following = useRef(true);
  const lastScrollTop = useRef(0);
  const lastScrollHeight = useRef(0);
  const seen = useRef<ReadonlySet<string>>(new Set<string>());
  const remembered = useRef<RememberedTurn | null>(null);
  const previousSends = useRef(ownSendCount);
  const previousReplays = useRef(replayCount);
  const previousSession = useRef(sessionId);
  // The scroll listener, the resize observer and the `jumpToEnd` the caller holds all outlive the
  // render that made them, and `contentIds` is a fresh array on every render: they read the
  // transcript through this ref instead of a closure, so none of them is torn down and rebuilt
  // several times a second while a turn streams. Declared first, so it is current by the time the
  // effects below read it in the same commit.
  const ids = useRef(contentIds);
  useLayoutEffect(() => {
    ids.current = contentIds;
  });

  const record = useCallback((element: HTMLElement) => {
    lastScrollTop.current = element.scrollTop;
    lastScrollHeight.current = element.scrollHeight;
  }, []);

  /** Follow from here: every turn on screen is a turn the reader has seen. */
  const follow = useCallback(() => {
    following.current = true;
    remembered.current = null;
    seen.current = new Set(ids.current);
    setAtEnd(true);
    setNewCount(0);
  }, []);

  const jumpToEnd = useCallback(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    element.scrollTop = element.scrollHeight;
    record(element);
    follow();
  }, [follow, record, viewport]);

  /**
   * What the viewport's current geometry says about following, recording it for the next reading.
   * A fall in `scrollTop` is the one signal that the reader took over: growth never lowers it, so
   * nothing but a reader moves it down - except a shrink (a fold collapsing, a shorter rebuild),
   * which clamps it with no reader action and must leave following alone. Deliberately not
   * assistant-ui's rule, which also demands `scrollHeight` be *unchanged* and so misses the
   * reader who scrolls back while a turn is streaming, this view's whole complaint.
   */
  const observe = useCallback(
    (element: HTMLElement): boolean => {
      const fell = lastScrollTop.current - element.scrollTop > END_TOLERANCE_PX;
      const shrank = element.scrollHeight < lastScrollHeight.current;
      record(element);
      if (atLiveEnd(element)) {
        follow();
        return true;
      }
      if (following.current && fell && !shrank) {
        following.current = false;
        remembered.current = rememberTurn(element);
        setAtEnd(false);
      }
      return following.current;
    },
    [follow, record]
  );

  useEffect(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    const onScroll = () => {
      if (!observe(element)) {
        // Always current when a rebuild lands: the reader's place is re-read on every scroll they
        // make away from the live end, not only on the scroll that stopped following.
        remembered.current = rememberTurn(element);
      }
    };
    element.addEventListener("scroll", onScroll, { passive: true });
    return () => element.removeEventListener("scroll", onScroll);
  }, [observe, viewport]);

  // Neither a turn's text growing as it streams nor the viewport's own height changing is a render
  // of this hook's: without the observer the newest line settles just below the fold and stays
  // there. The first element child is the content wrapper, whose height is the transcript's.
  useEffect(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    const sizes = new ResizeObserver(() => {
      if (following.current) {
        jumpToEnd();
      }
    });
    sizes.observe(element);
    const content = element.firstElementChild;
    if (content !== null) {
      sizes.observe(content);
    }
    return () => sizes.disconnect();
  }, [jumpToEnd, viewport]);

  // One effect for all four inputs, for the reason `use-follow-latest.ts:72-79` documents: a
  // send's own callback and the refreshed transcript land in either order across renders, so an
  // effect that reacted to one of them alone would miss the other's arrival. It measures the
  // viewport rather than trusting the last scroll event, because a server push can commit before
  // the browser has dispatched the reader's preceding scroll.
  useLayoutEffect(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    const ownSend = ownSendCount !== previousSends.current;
    previousSends.current = ownSendCount;
    const switched = sessionId !== previousSession.current;
    previousSession.current = sessionId;
    const rebuilt = replayCount !== previousReplays.current;
    previousReplays.current = replayCount;
    if (ownSend || switched) {
      jumpToEnd();
      return;
    }
    if (observe(element)) {
      jumpToEnd();
      return;
    }
    if (rebuilt) {
      // A rebuild answers the replay newest-first inside a budget, so it routinely drops the
      // oldest turns - exactly the ones a reader reading history sits on. It is the one event
      // that may take the reader's place away, and only when that place is gone: the remembered
      // turn is rendered from `contentIds`, so its element is there precisely while its id is.
      const place = remembered.current;
      // Not a `[data-message-id="…"]` selector: the key is the session's to choose, and nothing
      // promises it is a valid CSS string.
      const turn = [...element.querySelectorAll<HTMLElement>(TURN_SELECTOR)].find(
        (candidate) => candidate.getAttribute(TURN_ID_ATTRIBUTE) === place?.id
      );
      if (place === null || turn === undefined) {
        jumpToEnd();
        return;
      }
      // The turn survived: put it back where the reader had it, the compensation
      // `features/shell/ViewportAnchor.tsx` performs for the issue list.
      const wanted = element.getBoundingClientRect().top + place.top;
      element.scrollTop += turn.getBoundingClientRect().top - wanted;
      record(element);
    }
    // Every change of the list, not only a change of its last id: a turn updated under one id
    // counts once, and an id that left the list is simply absent.
    setNewCount(contentIds.filter((id) => !seen.current.has(id)).length);
  }, [contentIds, jumpToEnd, observe, ownSendCount, record, replayCount, sessionId, viewport]);

  return { atEnd, jumpToEnd, newCount };
}
