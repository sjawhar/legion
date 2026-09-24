import { type RefObject, useEffect, useMemo, useRef } from "react";

import type { Artifact } from "../../api/types";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { scrollMarginTo, useCardHold } from "./useCardHold";
import type { MarginTab } from "./useMarginItems";

interface UseMarginListenersOptions {
  /** Whether the margin is showing a composer the reader opened. */
  composerOpen: boolean;
  focus: { itemId: string; seq: number } | undefined;
  margin: RefObject<HTMLElement | null>;
  onSelectCard: (id: string, blockID?: string) => void;
  /** Whether the open document has reported where its blocks and marks sit. */
  placementsPublished: boolean;
  routeItemId: string | undefined;
  /** The link's request: a new navigation to the same item is a new one. */
  routeItemKey: string | undefined;
  setHoveredItemId: (id: string | undefined) => void;
  setTab: (tab: MarginTab) => void;
  sheetExpanded: boolean;
  tab: MarginTab;
  visibleArtifact: Artifact | undefined;
}

export function useMarginListeners({
  composerOpen,
  focus,
  margin,
  onSelectCard,
  placementsPublished,
  routeItemId,
  routeItemKey,
  setHoveredItemId,
  setTab,
  sheetExpanded,
  tab,
  visibleArtifact,
}: UseMarginListenersOptions): void {
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  // Read when the reader presses, not when a hold is armed, so a hold is not torn down and
  // rebuilt every time the open document republishes its offsets.
  const placed = useRef(placementsPublished);
  useEffect(() => {
    placed.current = placementsPublished;
  }, [placementsPublished]);

  // Margin.tsx applies the route's selection once the item exists; this only has to make sure
  // the comments tab is the one showing.
  useEffect(() => {
    if (routeItemId !== undefined) {
      setTab("comments");
    }
  }, [routeItemId, setTab]);

  // The link's card and a mark focus are held by the same rule, each keyed on the request that
  // named it: a navigation for the link, even one to the URL the reader is already on, and a
  // sequence for a focus, so focusing the same mark again is a new request too.
  const routeCard = useMemo(
    () =>
      routeItemId === undefined || routeItemKey === undefined
        ? undefined
        : { itemId: routeItemId, key: routeItemKey },
    [routeItemId, routeItemKey]
  );
  const focusItemId = focus?.itemId;
  const focusSeq = focus?.seq;
  const focusCard = useMemo(
    () =>
      focusItemId === undefined || focusSeq === undefined
        ? undefined
        : { itemId: focusItemId, key: focusSeq },
    [focusItemId, focusSeq]
  );
  useCardHold({ card: routeCard, composerOpen, margin, placed, sheetExpanded, tab });
  useCardHold({ card: focusCard, composerOpen, margin, placed, sheetExpanded, tab });

  // Opening a composer is the reader taking part, so the corrections above stand down - and the
  // composer itself has to be shown. It renders at the top of the margin's scroll content, and
  // the reader who opened it from the document never touched the margin, so a margin parked on
  // a linked card would keep the form off screen entirely. A collapsed compact sheet has no
  // scrollport and no composer laid out yet; opening one expands the sheet, and that expansion
  // is what brings the reader here.
  useEffect(() => {
    const container = margin.current;
    if (!composerOpen || container === null || tab !== "comments") {
      return;
    }
    if (isCompactViewport && !sheetExpanded) {
      return;
    }
    const composer = container.querySelector<HTMLElement>("[data-margin-composer]");
    if (composer !== null) {
      scrollMarginTo(
        container,
        composer.getBoundingClientRect(),
        container.getBoundingClientRect()
      );
    }
  }, [composerOpen, isCompactViewport, margin, sheetExpanded, tab]);

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
