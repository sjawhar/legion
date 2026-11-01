/**
 * Scrolling for the agent live view's transcript (`features/agent-view/AgentThread.tsx`): whether
 * the viewport follows what the session is writing, how many turns have arrived since the reader
 * stopped following, and the jump back to the live end. The view owns this rather than leaving it
 * to assistant-ui's viewport, whose auto-scroll yanks the reader to the bottom every time the
 * session starts a turn, so reading back through history while the session works is impossible.
 *
 * It owns the anchoring too: the viewport carries `overflow-anchor: none`, so every adjustment
 * the transcript makes is this hook's own and arrives nowhere as a scroll event it would have to
 * tell from the reader's.
 *
 * Deliberately not `features/conversation/use-follow-latest.ts`, the issue page's hook: that one
 * scrolls the window with the newest turn pinned at the top under the composer, this one scrolls
 * an element with the newest turn at the bottom, and only a live session rebuilds its whole
 * transcript from a replay (rule: `replayCount`). One shared hook would be a flag per difference.
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

/** assistant-ui's own tolerance: a sub-pixel remainder is still the end of the transcript. */
const END_TOLERANCE_PX = 1;
const TURN_ID_ATTRIBUTE = "data-message-id";
const TURN_SELECTOR = `[${TURN_ID_ATTRIBUTE}]`;
/** How far below the viewport's top edge the probe below walks, and in what steps, while it
 *  lands on nothing a turn owns. */
const PROBE_STEPS = 5;
const PROBE_STEP_PX = 8;

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

/**
 * The topmost turn the viewport shows any part of, with its offset from the viewport's top edge.
 * A hit test just inside that edge rather than a `getBoundingClientRect` scan over every turn,
 * the choice `features/shell/ViewportAnchor.tsx`'s `rowAtCenter` documents for the issue list and
 * for the same reason: this runs on every scroll event a scrolled-back reader makes, and a scan
 * forces a layout read per turn above them. A point that lands between two turns walks downward
 * in small steps, so the answer there is the turn below - the one being read into.
 */
function rememberTurn(element: HTMLElement): RememberedTurn | null {
  const rect = element.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const hit = (offset: number): HTMLElement | null => {
    const at = document.elementFromPoint(x, rect.top + offset);
    const turn = at?.closest<HTMLElement>(TURN_SELECTOR) ?? null;
    return turn !== null && element.contains(turn) ? turn : null;
  };
  let found = hit(END_TOLERANCE_PX);
  for (let step = 1; found === null && step <= PROBE_STEPS; step += 1) {
    found = hit(END_TOLERANCE_PX + step * PROBE_STEP_PX);
  }
  // Every probe missing is a viewport nothing can hit-test - one scrolled off the screen, or a
  // DOM with no layout at all - where the first turn is still a place to remember: losing the
  // place outright would send the reader to the live end at the next rebuild.
  const turn = found ?? element.querySelector<HTMLElement>(TURN_SELECTOR);
  const id = turn?.getAttribute(TURN_ID_ATTRIBUTE) ?? null;
  return turn === null || id === null
    ? null
    : { id, top: turn.getBoundingClientRect().top - rect.top };
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
  /** The scroller itself, not a ref holding it: the view renders it inside an error boundary,
   *  which replaces the node when a reset clears the error it held, and every listener below
   *  binds to the node it is given rather than to the first one there was. */
  viewport: HTMLElement | null;
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
    if (viewport === null) {
      return;
    }
    viewport.scrollTop = viewport.scrollHeight;
    record(viewport);
    follow();
  }, [follow, record, viewport]);

  /**
   * What the viewport's current geometry says about following, recording it for the next reading.
   * A fall in `scrollTop` is the one signal that the reader took over: growth never lowers it,
   * and with `overflow-anchor: none` on the viewport the browser adjusts nothing of its own, so
   * the only fall no reader made is a shrink's clamp (a fold collapsing, a shorter rebuild),
   * which by definition lands at the live end and is answered there. Deliberately not
   * assistant-ui's rule, which also demands `scrollHeight` be *unchanged* and so misses the
   * reader who scrolls back while a turn is streaming, this view's whole complaint.
   */
  const observe = useCallback(
    (element: HTMLElement): boolean => {
      const fell = lastScrollTop.current - element.scrollTop > END_TOLERANCE_PX;
      record(element);
      if (atLiveEnd(element)) {
        follow();
        return true;
      }
      if (following.current && fell) {
        following.current = false;
        remembered.current = rememberTurn(element);
        setAtEnd(false);
        return false;
      }
      // A following viewport off the end is left to the callers that can put it back: the layout
      // effect below jumps to the end on what it is told, and the resize observer does it as a
      // turn grows. A scroll event has nothing of its own to answer there.
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
        return;
      }
      // The turn survived: put it back where the reader had it, the compensation
      // `features/shell/ViewportAnchor.tsx` performs for the issue list.
      const wanted = element.getBoundingClientRect().top + place.top;
      element.scrollTop += turn.getBoundingClientRect().top - wanted;
      record(element);
      countUnseen();
    },
    [countUnseen, jumpToEnd, record]
  );

  // The scroller is a node this hook can be handed more than once: the view renders it inside an
  // `ErrorBoundary`, whose reset replaces it with a fresh one, at `scrollTop` 0 and carrying none
  // of the old one's position. Re-baseline before the effect below reads that 0 as the reader
  // scrolling back: a viewport that was following takes the live end, and one the reader had
  // scrolled back in has their turn put back where they had it.
  useLayoutEffect(() => {
    const element = viewport;
    if (element === null) {
      return;
    }
    record(element);
    if (following.current) {
      jumpToEnd();
    } else {
      settleRebuild(element);
    }
  }, [jumpToEnd, record, settleRebuild, viewport]);

  // The turns are not always this hook's to await: assistant-ui applies a rebuilt transcript in a
  // passive effect, one commit after the render that carried the new ids
  // (`useExternalStoreRuntime`), and that commit need not render this hook at all. A rebuild
  // whose turns have not landed yet waits here for them - measured in Chromium, compensating
  // against the DOM still on screen moves the reader by nothing and leaves their place to the
  // browser. The watcher runs only between a rebuild and its turns, never while one streams.
  useEffect(() => {
    const element = viewport;
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
    const element = viewport;
    if (element === null) {
      return;
    }
    const onScroll = () => {
      // The reader's place is re-read on every scroll they make away from the live end, not only
      // on the one that stopped following: `rememberTurn` is a hit test, so taking the place
      // again on an event `observe` has already read it on costs one layout read.
      if (!observe(element)) {
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
    const element = viewport;
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
    const element = viewport;
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
