import { type RefObject, useEffect, useRef } from "react";

import type { Artifact } from "../../api/types";
import { COMPACT_VIEWPORT_QUERY } from "../shell/useDialog";
import { type MarginItem, type MarginTab, marginItemId } from "./useMarginItems";

interface UseMarginListenersOptions {
  focus: { itemId: string; seq: number } | undefined;
  items: MarginItem[];
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

/**
 * Scrolls the margin so `id`'s card sits in the middle of its scrollport, and reports whether
 * the card is in that scrollport afterwards. A card taller than the port counts once it fills
 * it: no scroll position shows more of it.
 */
function centerCardInMargin(margin: RefObject<HTMLElement | null>, id: string): boolean {
  const container = margin.current;
  const card = container?.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(id)}"]`);
  if (container === null || card === null || card === undefined) {
    return false;
  }
  const cardBounds = card.getBoundingClientRect();
  const containerBounds = container.getBoundingClientRect();
  const top =
    cardBounds.top -
    containerBounds.top +
    container.scrollTop -
    Math.max(0, (container.clientHeight - cardBounds.height) / 2);
  container.scrollTo({ top: Math.max(0, top) });
  const settled = card.getBoundingClientRect();
  if (settled.height > containerBounds.height) {
    return settled.top <= containerBounds.top;
  }
  return settled.top >= containerBounds.top && settled.bottom <= containerBounds.bottom;
}

/**
 * Brings `id`'s card into the margin's scrollport and keeps correcting until it is there.
 * One scroll is not enough: anchored cards are positioned from mark offsets the open document
 * reports later, and the compact sheet grows from its 64 px handle to `max-h-[85dvh]` as it
 * opens, so the geometry the first scroll reads is stale within a frame or two. Each relayout
 * inside the margin re-runs the scroll; once the card is in view this stops and never fights
 * the reader's own scrolling. Returns the teardown for the pending frame and observer.
 */
function settleCardIntoView(
  margin: RefObject<HTMLElement | null>,
  id: string,
  onSettled: () => void
): () => void {
  let frame: number | undefined;
  const observer = new MutationObserver(() => schedule());
  const stop = () => {
    observer.disconnect();
    if (frame !== undefined) {
      cancelAnimationFrame(frame);
      frame = undefined;
    }
  };
  const attempt = () => {
    frame = undefined;
    if (centerCardInMargin(margin, id)) {
      stop();
      onSettled();
    }
  };
  function schedule() {
    if (frame === undefined) {
      frame = requestAnimationFrame(attempt);
    }
  }
  const container = margin.current;
  if (container !== null) {
    observer.observe(container, {
      attributeFilter: ["class", "style"],
      attributes: true,
      childList: true,
      subtree: true,
    });
  }
  attempt();
  return stop;
}

export function useMarginListeners({
  focus,
  items,
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
  const scrolledFocusSequence = useRef<number | undefined>(undefined);
  const scrolledRouteItem = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (routeItemId !== undefined) {
      setTab("comments");
      selectItem(routeItemId);
    }
  }, [routeItemId, selectItem, setTab]);
  useEffect(() => {
    if (routeItemId === undefined) {
      scrolledRouteItem.current = undefined;
      return;
    }
    if (window.matchMedia(COMPACT_VIEWPORT_QUERY).matches && !sheetExpanded) {
      scrolledRouteItem.current = undefined;
      return;
    }
    if (
      tab !== "comments" ||
      scrolledRouteItem.current === routeItemId ||
      !items.some((item) => marginItemId(item) === routeItemId)
    ) {
      return;
    }
    return settleCardIntoView(margin, routeItemId, () => {
      scrolledRouteItem.current = routeItemId;
    });
  }, [items, margin, routeItemId, sheetExpanded, tab]);

  useEffect(() => {
    if (focus === undefined) {
      scrolledFocusSequence.current = undefined;
      return;
    }
    if (
      tab !== "comments" ||
      scrolledFocusSequence.current === focus.seq ||
      !items.some((item) => marginItemId(item) === focus.itemId) ||
      (window.matchMedia(COMPACT_VIEWPORT_QUERY).matches && !sheetExpanded)
    ) {
      return;
    }
    const seq = focus.seq;
    return settleCardIntoView(margin, focus.itemId, () => {
      scrolledFocusSequence.current = seq;
    });
  }, [focus, items, margin, sheetExpanded, tab]);

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
