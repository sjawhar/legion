import { type RefObject, useEffect, useRef } from "react";

import type { Artifact } from "../../api/types";
import { COMPACT_VIEWPORT_QUERY } from "../shell/useDialog";
import type { MarginTab } from "./useMarginItems";

interface UseMarginListenersOptions {
  focus: { itemId: string; seq: number } | undefined;
  margin: RefObject<HTMLElement | null>;
  onSelectCard: (id: string, blockID?: string) => void;
  routeItemId: string | undefined;
  selectItem: (id: string) => void;
  setHoveredItemId: (id: string | undefined) => void;
  setTab: (tab: MarginTab) => void;
  sheetExpanded: boolean;
  tab: MarginTab;
  visibleArtifact: Artifact | undefined;
}

/** The margin's card for `id`, once the margin has rendered it. */
function cardInMargin(container: HTMLElement, id: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(id)}"]`);
}

/**
 * Whether the card sits inside the margin's scrollport. A card taller than the port counts
 * once it fills it: no scroll position shows more of it.
 */
function cardIsInMarginView(container: HTMLElement, card: HTMLElement): boolean {
  const cardBounds = card.getBoundingClientRect();
  const containerBounds = container.getBoundingClientRect();
  if (cardBounds.height > containerBounds.height) {
    return cardBounds.top <= containerBounds.top && cardBounds.bottom >= containerBounds.bottom;
  }
  return cardBounds.top >= containerBounds.top && cardBounds.bottom <= containerBounds.bottom;
}

/** Scrolls the margin so the card sits in the middle of its scrollport. */
function centerCardInMargin(container: HTMLElement, card: HTMLElement): void {
  const cardBounds = card.getBoundingClientRect();
  const containerBounds = container.getBoundingClientRect();
  const top =
    cardBounds.top -
    containerBounds.top +
    container.scrollTop -
    Math.max(0, (container.clientHeight - cardBounds.height) / 2);
  container.scrollTo({ top: Math.max(0, top) });
}

const readerGestures = ["keydown", "pointerdown", "touchstart", "wheel"] as const;

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
 * scroll this did not perform ends the correction for good. Returns the teardown a new link or
 * an unmount uses.
 */
function keepCardInView(
  margin: RefObject<HTMLElement | null>,
  id: string,
  onReaderTakeover: () => void
): () => void {
  const container = margin.current;
  if (container === null) {
    return () => {};
  }
  let frame: number | undefined;
  let appliedTop = container.scrollTop;
  let relaidOut = false;
  const observer = new MutationObserver(() => {
    relaidOut = true;
    schedule();
  });
  const stop = () => {
    observer.disconnect();
    container.removeEventListener("scroll", scrolled);
    for (const gesture of readerGestures) {
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
  // A relayout moves the scroll on its own - the browser's scroll anchoring holds the content
  // the reader can see, and a margin whose content shrank clamps its offset - so only a move
  // no layout change and no correction of ours explains is the reader's.
  const scrolled = () => {
    if (relaidOut) {
      relaidOut = false;
      appliedTop = container.scrollTop;
      return;
    }
    if (Math.abs(container.scrollTop - appliedTop) > 1) {
      readerTookOver();
    }
  };
  const attempt = () => {
    frame = undefined;
    const card = cardInMargin(container, id);
    if (card === null || cardIsInMarginView(container, card)) {
      return;
    }
    centerCardInMargin(container, card);
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
  for (const gesture of readerGestures) {
    container.addEventListener(gesture, readerTookOver, { passive: true });
  }
  attempt();
  return stop;
}

export function useMarginListeners({
  focus,
  margin,
  onSelectCard,
  routeItemId,
  selectItem,
  setHoveredItemId,
  setTab,
  sheetExpanded,
  tab,
  visibleArtifact,
}: UseMarginListenersOptions): void {
  const readerScrolledFocus = useRef<number | undefined>(undefined);
  const readerScrolledRoute = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (routeItemId !== undefined) {
      setTab("comments");
      selectItem(routeItemId);
    }
  }, [routeItemId, selectItem, setTab]);
  useEffect(() => {
    if (routeItemId === undefined) {
      readerScrolledRoute.current = undefined;
      return;
    }
    if (window.matchMedia(COMPACT_VIEWPORT_QUERY).matches && !sheetExpanded) {
      readerScrolledRoute.current = undefined;
      return;
    }
    if (tab !== "comments" || readerScrolledRoute.current === routeItemId) {
      return;
    }
    return keepCardInView(margin, routeItemId, () => {
      readerScrolledRoute.current = routeItemId;
    });
  }, [margin, routeItemId, sheetExpanded, tab]);

  useEffect(() => {
    if (focus === undefined) {
      readerScrolledFocus.current = undefined;
      return;
    }
    if (
      tab !== "comments" ||
      readerScrolledFocus.current === focus.seq ||
      (window.matchMedia(COMPACT_VIEWPORT_QUERY).matches && !sheetExpanded)
    ) {
      return;
    }
    const seq = focus.seq;
    return keepCardInView(margin, focus.itemId, () => {
      readerScrolledFocus.current = seq;
    });
  }, [focus, margin, sheetExpanded, tab]);

  // The margin sheet stays mounted while comments change, so listeners must re-attach when the
  // tab or artifact changes; the ref itself is not reactive.
  // biome-ignore lint/correctness/useExhaustiveDependencies: tab and artifact gate the cards' existence
  useEffect(() => {
    const container = margin.current;
    if (container === null) {
      return;
    }
    const cardForTarget = (target: EventTarget | null) =>
      target instanceof Element ? target.closest<HTMLElement>("[data-margin-item]") : null;
    const selectCard = (event: MouseEvent) => {
      if (
        event.target instanceof Element &&
        event.target.closest("a, button, input, label, select, textarea") !== null
      ) {
        return;
      }
      const card = cardForTarget(event.target);
      if (card?.dataset.marginItem !== undefined) {
        onSelectCard(card.dataset.marginItem);
      }
    };
    const hoverCard = (event: MouseEvent) => {
      const card = cardForTarget(event.target);
      if (card?.dataset.marginItem !== undefined) {
        setHoveredItemId(card.dataset.marginItem);
      }
    };
    const leaveCard = (event: MouseEvent) => {
      if (cardForTarget(event.target) !== cardForTarget(event.relatedTarget)) {
        setHoveredItemId(undefined);
      }
    };
    container.addEventListener("click", selectCard);
    container.addEventListener("mouseover", hoverCard);
    container.addEventListener("mouseout", leaveCard);
    return () => {
      container.removeEventListener("click", selectCard);
      container.removeEventListener("mouseover", hoverCard);
      container.removeEventListener("mouseout", leaveCard);
    };
  }, [onSelectCard, setHoveredItemId, tab, visibleArtifact?.id]);
}
