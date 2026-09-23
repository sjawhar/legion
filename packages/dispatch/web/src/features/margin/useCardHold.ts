import { type RefObject, useEffect, useRef } from "react";

import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import type { MarginTab } from "./useMarginItems";

/** The margin's card for `id`, once the margin has rendered it. */
function cardInMargin(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(id)}"]`);
}

/**
 * Whether the element sits inside the margin's scrollport. An element taller than the port counts
 * once it fills it: no scroll position shows more of it.
 */
export function isInMarginView(bounds: DOMRect, containerBounds: DOMRect): boolean {
  if (bounds.height > containerBounds.height) {
    return bounds.top <= containerBounds.top && bounds.bottom >= containerBounds.bottom;
  }
  return bounds.top >= containerBounds.top && bounds.bottom <= containerBounds.bottom;
}

/**
 * Scrolls the margin so the already-measured element starts `padding` below the top of its
 * scrollport: the card's caller centres it, the composer's starts it.
 */
export function scrollMarginTo(
  container: HTMLElement,
  bounds: DOMRect,
  containerBounds: DOMRect,
  padding = 0
): void {
  const top = bounds.top - containerBounds.top + container.scrollTop - padding;
  container.scrollTo({ top: Math.max(0, top) });
}

const marginGestures = ["keydown", "touchstart", "wheel"] as const;

/**
 * How long after the margin changes shape its own scrolls are still the layout's, not a reader's.
 * A frame or two would cover the mechanics - anchoring while the content changes, clamping as it
 * settles - and the window is an order of magnitude longer than that on purpose: a real reader is
 * recognised by the gestures, which end the hold unconditionally and never consult this, so the
 * only thing a generous window can absorb is a scroll with no gesture at all.
 *
 * What that leaves, measured in Chromium: a scrollbar press, middle-click autoscroll and a wheel
 * all end the hold through their gestures. Find-in-page, `scrollIntoView`, an assistive-tech
 * focus move and Tab from the document into the margin do not carry one, so a scroll of theirs
 * that lands while the window is open is read as the layout's. The window is opened by any
 * class/style change in the margin's subtree - hovering a card re-arms it, harmlessly, since
 * every reader path that matters takes over without consulting it - and a remote collaborator
 * typing in the open document keeps it open while they type, because each republished placement
 * rewrites the cards' `style.top`. The cost of an absorbed scroll is bounded: the hold stays
 * armed and only moves the margin again if the linked card has left the scrollport, and the next
 * scroll or any gesture ends it.
 *
 * Focus is deliberately not a takeover signal, which would otherwise catch Tab and assistive
 * focus moves. Measured on a document item link in Chromium, the landing itself puts focus inside
 * the margin on both viewports: on a desktop the linked thread expands and its reply composer
 * takes it (`focusin` on a TEXTAREA inside the margin), and on a phone `useDialog` focuses the
 * review sheet's first control as it expands - the same moment the hold arms. A `focusin` rule
 * would end every landing before it began.
 */
export const RELAYOUT_SETTLES_MS = 250;

/**
 * Holds `id`'s card in the margin's scrollport the way a browser holds a fragment target while
 * a page loads: the card the link names stays in view until the reader takes over. A single
 * scroll cannot do that. The margin fills in over several frames - it renders "Loading margin…",
 * then its cards, then the "Needs you" group above them, and anchored cards move again as the
 * open document reports its mark offsets - and each of those relayouts can push the card out of
 * view after it was in it. So every relayout inside the margin re-checks, and a card that is out
 * of view is scrolled back; a card already in view is left where it is.
 *
 * The reader wins from the moment they take part: a wheel, a touch, a key, a pointer press, or a
 * scroll this did not perform inside the margin, and - once the open document has reported where
 * its blocks and marks sit - a pointer press in the document, since pressing into the text is how
 * a reader starts a selection and someone working the passage is no longer being landed. A press
 * before those offsets land is the reader arriving, not leaving: every anchored card is still
 * stacked at the top of the margin, and dropping the hold there leaves the card below the fold
 * once its real placement arrives, which is the defect the hold exists to fix. Scrolling the
 * document is never taking part: that is when holding the linked card matters most. Returns the
 * teardown a new link or an unmount uses.
 */
function keepCardInView(
  margin: RefObject<HTMLElement | null>,
  id: string,
  placed: RefObject<boolean>,
  onReaderTakeover: () => void
): () => void {
  const container = margin.current;
  if (container === null) {
    return () => {};
  }
  let frame: number | undefined;
  let appliedTop = container.scrollTop;
  // When the margin last changed shape. A relayout can move the scroll more than once - scroll
  // anchoring during the change, then clamping as the content settles - so this is a window, not
  // a flag one scroll consumes.
  let relaidOutAt = Number.NEGATIVE_INFINITY;
  const observer = new MutationObserver(() => {
    relaidOutAt = performance.now();
    schedule();
  });
  const stop = () => {
    observer.disconnect();
    container.removeEventListener("scroll", scrolled);
    container.ownerDocument.removeEventListener("pointerdown", pressed, true);
    for (const gesture of marginGestures) {
      container.removeEventListener(gesture, readerTookOver);
    }
    if (frame !== undefined) {
      cancelAnimationFrame(frame);
      frame = undefined;
    }
  };
  const readerTookOver = () => {
    stop();
    onReaderTakeover();
  };
  // A relayout moves the scroll on its own - the browser's scroll anchoring holds the content the
  // reader can see, and a margin whose content shrank clamps its offset - so only a move that no
  // layout change and no correction of ours explains is the reader's. The observer's callback is
  // a microtask and a scroll event is a task, so the records for a relayout can still be queued
  // when its own scroll arrives: draining them here is what keeps that scroll from being read as
  // the reader's, and it is the only thing that re-opens the window. A scroll that merely lands
  // inside an open one does not extend it, or a stream of them would slide it along indefinitely.
  const scrolled = () => {
    // Draining the records also suppresses the observer's callback for them, so this branch owes
    // the re-check the callback would have scheduled.
    const relaidOut = observer.takeRecords().length > 0;
    if (relaidOut) {
      relaidOutAt = performance.now();
      schedule();
    }
    if (relaidOut || performance.now() - relaidOutAt < RELAYOUT_SETTLES_MS) {
      appliedTop = container.scrollTop;
      return;
    }
    if (Math.abs(container.scrollTop - appliedTop) > 1) {
      readerTookOver();
    }
  };
  // A press inside the margin is always the reader. A press in the document counts only once the
  // document has placed its blocks and marks: before that every anchored card is still stacked at
  // the top of the margin, and the reader is arriving, not leaving.
  const pressed = (event: PointerEvent) => {
    const target = event.target;
    if (placed.current || (target instanceof Node && container.contains(target))) {
      readerTookOver();
    }
  };
  const attempt = () => {
    frame = undefined;
    const card = cardInMargin(container, id);
    if (card === null) {
      return;
    }
    const cardBounds = card.getBoundingClientRect();
    const containerBounds = container.getBoundingClientRect();
    if (isInMarginView(cardBounds, containerBounds)) {
      return;
    }
    scrollMarginTo(
      container,
      cardBounds,
      containerBounds,
      Math.max(0, (container.clientHeight - cardBounds.height) / 2)
    );
    appliedTop = container.scrollTop;
  };
  function schedule() {
    if (frame === undefined) {
      frame = requestAnimationFrame(attempt);
    }
  }
  observer.observe(container, {
    attributeFilter: ["class", "style"],
    attributes: true,
    childList: true,
    subtree: true,
  });
  container.addEventListener("scroll", scrolled, { passive: true });
  container.ownerDocument.addEventListener("pointerdown", pressed, {
    capture: true,
    passive: true,
  });
  for (const gesture of marginGestures) {
    container.addEventListener(gesture, readerTookOver, { passive: true });
  }
  attempt();
  return stop;
}

interface CardHold {
  /** Whether the margin is showing a composer the reader opened. */
  composerOpen: boolean;
  /** The card to hold, and the request that named it: a new key re-arms the hold. */
  card: { itemId: string; key: string | number } | undefined;
  margin: RefObject<HTMLElement | null>;
  placed: RefObject<boolean>;
  sheetExpanded: boolean;
  tab: MarginTab;
}

/**
 * Arms `keepCardInView` for the card a link or a mark focus named, and remembers - per card key -
 * that the hold was released, so nothing re-arms it behind the reader. A composer opening
 * releases the hold without ever arming it: the reader is writing, not landing.
 *
 * Release is for the life of the request, including across a compact sheet the reader collapses
 * and re-opens: they took the margin over once, and re-opening the panel is not asking to be
 * landed again. Only a new card key starts a new hold.
 */
export function useCardHold({
  composerOpen,
  card,
  margin,
  placed,
  sheetExpanded,
  tab,
}: CardHold): void {
  const released = useRef<string | number | undefined>(undefined);
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const itemId = card?.itemId;
  const key = card?.key;

  useEffect(() => {
    if (itemId === undefined || key === undefined) {
      released.current = undefined;
      return;
    }
    if (tab !== "comments" || released.current === key) {
      return;
    }
    // A collapsed compact sheet has no scrollport to hold anything in; expanding it re-runs this.
    if (isCompactViewport && !sheetExpanded) {
      return;
    }
    if (composerOpen) {
      released.current = key;
      return;
    }
    return keepCardInView(margin, itemId, placed, () => {
      released.current = key;
    });
  }, [composerOpen, isCompactViewport, itemId, key, margin, placed, sheetExpanded, tab]);
}
