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

function renderedIds(element: HTMLElement): readonly string[] {
  return [...element.querySelectorAll<HTMLElement>(TURN_SELECTOR)].map(
    (turn) => turn.getAttribute(TURN_ID_ATTRIBUTE) ?? ""
  );
}

/** Whether the turns on screen are exactly these ids, in this order. */
function showsIds(element: HTMLElement, ids: readonly string[]): boolean {
  const shown = renderedIds(element);
  return shown.length === ids.length && shown.every((id, index) => id === ids[index]);
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
  // The transcript `seen` was built from, so `follow()` rebuilds the set only when the transcript
  // itself is a new array: it is called from every scroll event at the live end and from the
  // resize observer as a turn streams, and copying the whole transcript into a new set several
  // times a frame buys nothing. `null` until the first build, which no transcript array equals.
  const seenIds = useRef<readonly string[] | null>(null);
  const remembered = useRef<RememberedTurn | null>(null);
  const previousSends = useRef(ownSendCount);
  const previousReplays = useRef(replayCount);
  const previousSession = useRef(sessionId);
  // The `scrollHeight` the hook left behind when it last put the reader back after a rebuild.
  // The browser answers that same rebuild a frame later, by subtracting the height it dropped
  // above the reader (scroll anchoring), and by then the settle has recorded the new height, so
  // there is no shrink left for the arriving event to see. The height identifies that one event;
  // any other scroll, and any later one, is the reader's.
  const settleHeight = useRef<number | null>(null);
  // A rebuild waiting for its turns to reach the DOM, with the ids that were on screen when it
  // arrived: the watcher below settles it as soon as they differ.
  const pendingRebuild = useRef(false);
  const armedIds = useRef<readonly string[]>([]);
  const rebuildWatcher = useRef<MutationObserver | null>(null);
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
    if (seenIds.current !== ids.current) {
      seen.current = new Set(ids.current);
      seenIds.current = ids.current;
    }
    setAtEnd(true);
    setNewCount(0);
  }, []);

  /** Turns that have arrived since the reader stopped following: those the transcript holds and
   *  the reader has not seen. `ids.current` is this render's `contentIds` by the time either
   *  caller runs, because the layout effect that assigns it is declared first. */
  const countUnseen = useCallback(() => {
    setNewCount(ids.current.filter((id) => !seen.current.has(id)).length);
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
   * which clamps it, and the scroll anchoring that answers a rebuild the hook has just settled,
   * which subtracts the height the rebuild dropped above the reader (measured in Chromium on
   * this view: a jump to the end of a rebuilt transcript came back as `scrollTop` 0). Neither is
   * the reader, so neither stops following, and whatever they did to the position a following
   * viewport takes back. Deliberately not assistant-ui's rule, which also demands `scrollHeight`
   * be *unchanged* and so misses the reader who scrolls back while a turn is streaming, this
   * view's whole complaint.
   */
  const observe = useCallback(
    (element: HTMLElement): boolean => {
      const fell = lastScrollTop.current - element.scrollTop > END_TOLERANCE_PX;
      const shrank = element.scrollHeight < lastScrollHeight.current;
      const settling = settleHeight.current === element.scrollHeight;
      settleHeight.current = null;
      const excused = shrank || settling;
      record(element);
      if (atLiveEnd(element)) {
        follow();
        return true;
      }
      if (following.current && fell && !excused) {
        following.current = false;
        remembered.current = rememberTurn(element);
        setAtEnd(false);
        return false;
      }
      if (following.current) {
        // Still following and off the end: nothing the reader did put it there, so take the end
        // back rather than leave the newest text below the fold until the next arrival.
        element.scrollTop = element.scrollHeight;
        record(element);
      }
      return following.current;
    },
    [follow, record]
  );

  /**
   * Put the reader back after a rebuild: the turn they were reading where it was, or the live end
   * when it is gone (and the live end whatever the ids say while following, where there is no
   * remembered turn to hold). Runs in the commit the turns themselves changed in, which is not
   * always the one that carried the new ids.
   */
  const settleRebuild = useCallback(
    (element: HTMLElement) => {
      const place = remembered.current;
      // Not a `[data-message-id="…"]` selector: the key is the session's to choose, and nothing
      // promises it is a valid CSS string.
      const turn = [...element.querySelectorAll<HTMLElement>(TURN_SELECTOR)].find(
        (candidate) => candidate.getAttribute(TURN_ID_ATTRIBUTE) === place?.id
      );
      if (place === null || turn === undefined) {
        jumpToEnd();
      } else {
        // The turn survived: put it back where the reader had it, the compensation
        // `features/shell/ViewportAnchor.tsx` performs for the issue list.
        const wanted = element.getBoundingClientRect().top + place.top;
        element.scrollTop += turn.getBoundingClientRect().top - wanted;
        record(element);
        countUnseen();
      }
      settleHeight.current = element.scrollHeight;
    },
    [countUnseen, jumpToEnd, record]
  );

  // The turns are not always this hook's to await: assistant-ui applies a rebuilt transcript in a
  // passive effect, one commit after the render that carried the new ids
  // (`useExternalStoreRuntime`), and that commit need not render this hook at all. A rebuild
  // whose turns have not landed yet waits here for them - measured in Chromium, compensating
  // against the DOM still on screen moves the reader by nothing and leaves their place to the
  // browser. The watcher runs only between a rebuild and its turns, never while one streams.
  useEffect(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    const watcher = new MutationObserver(() => {
      if (!pendingRebuild.current || showsIds(element, armedIds.current)) {
        return;
      }
      pendingRebuild.current = false;
      watcher.disconnect();
      settleRebuild(element);
    });
    rebuildWatcher.current = watcher;
    return () => {
      watcher.disconnect();
      rebuildWatcher.current = null;
    };
  }, [settleRebuild, viewport]);

  useEffect(() => {
    const element = viewport.current;
    if (element === null) {
      return;
    }
    const onScroll = () => {
      // Always current when a rebuild lands: the reader's place is re-read on every scroll they
      // make away from the live end, not only on the scroll that stopped following - which is
      // the one scroll `observe` has already read it on itself.
      const wasFollowing = following.current;
      if (!observe(element) && !wasFollowing) {
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
    const follows = observe(element);
    if (rebuilt) {
      // A rebuild answers the replay newest-first inside a budget, so it routinely drops the
      // oldest turns - exactly the ones a reader reading history sits on. It is the one event
      // that may take the reader's place away, and only when that place is gone.
      if (showsIds(element, contentIds)) {
        settleRebuild(element);
        return;
      }
      pendingRebuild.current = true;
      armedIds.current = renderedIds(element);
      rebuildWatcher.current?.observe(element, { childList: true, subtree: true });
    }
    if (follows) {
      jumpToEnd();
      return;
    }
    // Every change of the list, not only a change of its last id: a turn updated under one id
    // counts once, and an id that left the list is simply absent.
    countUnseen();
  }, [
    contentIds,
    countUnseen,
    jumpToEnd,
    observe,
    ownSendCount,
    replayCount,
    sessionId,
    settleRebuild,
    viewport,
  ]);

  return { atEnd, jumpToEnd, newCount };
}
