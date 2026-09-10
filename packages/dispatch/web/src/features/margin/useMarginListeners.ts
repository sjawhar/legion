import { type RefObject, useEffect, useRef } from "react";

import type { Artifact } from "../../api/types";
import { type MarginItem, type MarginTab, marginItemId } from "./useMarginItems";

interface UseMarginListenersOptions {
  items: MarginItem[];
  list: RefObject<HTMLDivElement | null>;
  routeItemId: string | undefined;
  selectItem: (id: string) => void;
  setHoveredItemId: (id: string | undefined) => void;
  setTab: (tab: MarginTab) => void;
  sheetExpanded: boolean;
  tab: MarginTab;
  visibleArtifact: Artifact | undefined;
}

export function useMarginListeners({
  items,
  list,
  routeItemId,
  selectItem,
  setHoveredItemId,
  setTab,
  sheetExpanded,
  tab,
  visibleArtifact,
}: UseMarginListenersOptions): void {
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
    if (
      tab !== "comments" ||
      scrolledRouteItem.current === routeItemId ||
      !items.some((item) => marginItemId(item) === routeItemId) ||
      (window.matchMedia("(max-width: 767px)").matches && !sheetExpanded)
    ) {
      return;
    }
    const container = list.current;
    if (container === null) {
      return;
    }
    const card = container.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(routeItemId)}"]`
    );
    if (card === null) {
      return;
    }
    const top =
      card.getBoundingClientRect().top -
      container.getBoundingClientRect().top +
      container.scrollTop -
      container.clientHeight / 4;
    container.scrollTo({ top: Math.max(0, top) });
    scrolledRouteItem.current = routeItemId;
  }, [items, list, routeItemId, sheetExpanded, tab]);

  // The list container mounts only on the Comments tab for a visible artifact, so the
  // listeners must re-attach when either changes; the ref itself is not reactive.
  // biome-ignore lint/correctness/useExhaustiveDependencies: tab and artifact gate the container's existence
  useEffect(() => {
    const container = list.current;
    if (container === null) {
      return;
    }
    const cardForTarget = (target: EventTarget | null) =>
      target instanceof Element ? target.closest<HTMLElement>("[data-margin-item]") : null;
    const selectCard = (event: MouseEvent) => {
      const card = cardForTarget(event.target);
      if (card?.dataset.marginItem !== undefined) {
        selectItem(card.dataset.marginItem);
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
  }, [selectItem, setHoveredItemId, tab, visibleArtifact?.id]);
}
