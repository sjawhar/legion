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

function scrollCardIntoView(margin: RefObject<HTMLElement | null>, id: string): boolean {
  const container = margin.current;
  const selector = `[data-margin-item="${CSS.escape(id)}"]`;
  const card =
    container?.querySelector<HTMLElement>(selector) ??
    document.querySelector<HTMLElement>(selector);
  if (card === null) {
    return false;
  }
  if (container === null) {
    card.scrollIntoView({ block: "center" });
    return true;
  }
  const cardBounds = card.getBoundingClientRect();
  const containerBounds = container.getBoundingClientRect();
  const top =
    cardBounds.top -
    containerBounds.top +
    container.scrollTop -
    Math.max(0, (container.clientHeight - cardBounds.height) / 2);
  container.scrollTo({ top: Math.max(0, top) });
  return true;
}
/**
 * Scrolls `id`'s card to the middle of the margin and keeps doing so until it is actually
 * inside the margin's own viewport. The compact sheet grows from its 64 px handle to
 * `max-h-[85dvh]` and lays its anchored cards out from measured mark positions, so the first
 * frame's geometry is the collapsed shell's: one scroll lands short. Returns the cancel for
 * the pending frame.
 */
function settleCardIntoView(
  margin: RefObject<HTMLElement | null>,
  id: string,
  onSettled: () => void
): () => void {
  let frame: number | undefined;
  const attempt = () => {
    frame = undefined;
    if (!scrollCardIntoView(margin, id)) {
      return;
    }
    const container = margin.current;
    if (container === null) {
      onSettled();
      return;
    }
    const card = container.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(id)}"]`);
    const cardBounds = card?.getBoundingClientRect();
    const containerBounds = container.getBoundingClientRect();
    if (
      cardBounds !== undefined &&
      cardBounds.top >= containerBounds.top &&
      cardBounds.bottom <= containerBounds.bottom
    ) {
      onSettled();
      return;
    }
    frame = requestAnimationFrame(attempt);
  };
  frame = requestAnimationFrame(attempt);
  return () => {
    if (frame !== undefined) {
      cancelAnimationFrame(frame);
    }
  };
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
    const compact = window.matchMedia(COMPACT_VIEWPORT_QUERY).matches;
    if (compact && !sheetExpanded) {
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
    if (compact) {
      return settleCardIntoView(margin, routeItemId, () => {
        scrolledRouteItem.current = routeItemId;
      });
    }
    if (scrollCardIntoView(margin, routeItemId)) {
      scrolledRouteItem.current = routeItemId;
    }
  }, [items, margin, routeItemId, sheetExpanded, tab]);

  useEffect(() => {
    if (focus === undefined) {
      scrolledFocusSequence.current = undefined;
      return;
    }
    const compact = window.matchMedia(COMPACT_VIEWPORT_QUERY).matches;
    if (
      tab !== "comments" ||
      scrolledFocusSequence.current === focus.seq ||
      !items.some((item) => marginItemId(item) === focus.itemId) ||
      (compact && !sheetExpanded)
    ) {
      return;
    }
    if (compact) {
      const seq = focus.seq;
      return settleCardIntoView(margin, focus.itemId, () => {
        scrolledFocusSequence.current = seq;
      });
    }
    if (scrollCardIntoView(margin, focus.itemId)) {
      scrolledFocusSequence.current = focus.seq;
    }
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
