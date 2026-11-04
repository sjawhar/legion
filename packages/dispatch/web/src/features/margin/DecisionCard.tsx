import { type ReactNode, useCallback, useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import {
  card,
  decisionUnsureBorder,
  decisionUnsureText,
  linkHoverText,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textMutedOnSelectedCard,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { buildReferencePath, parseDispatchReference } from "../refs/routes";
import { regionOrigin } from "./ThreadList";
import type { DecisionMarginItem, MarkPlacement } from "./useMarginItems";

/** One decision beside a spec block it shaped: whose choice it is, its uncertainty, and the
 * source node linked by its own reference. Selecting the card lights up the shaped block. */
export function DecisionCard({
  hovered = false,
  item,
  onSelect,
  selected,
}: {
  hovered?: boolean;
  item: DecisionMarginItem;
  onSelect(id: string): void;
  selected: boolean;
}): ReactNode {
  const { decision } = item;
  const route = parseDispatchReference(decision.ref);
  const active = selected || hovered;
  return (
    <article
      aria-current={selected ? "true" : undefined}
      aria-label={`Decision: ${decision.heading}`}
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        active ? `${selectedCardBorder} ${selectedCardBg}` : card
      } ${decision.unsure ? `border-l-4 ${decisionUnsureBorder}` : ""}`}
      data-decision-unsure={decision.unsure ? "true" : undefined}
      data-margin-item={item.id}
      onClick={(event) => {
        if (event.target instanceof Element && event.target.closest("a, button") !== null) {
          return;
        }
        event.stopPropagation();
        onSelect(item.id);
      }}
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget || (event.key !== "Enter" && event.key !== " ")) {
          return;
        }
        event.preventDefault();
        onSelect(item.id);
      }}
    >
      <p className={`flex flex-wrap gap-x-2 text-xs ${textMutedOnSelectedCard}`}>
        <span>{decision.kind === "person" ? "A person's decision" : "Agent's decision"}</span>
        {decision.unsure ? (
          <span className={`font-semibold ${decisionUnsureText}`}>Unsure</span>
        ) : null}
      </p>
      <h3 className={`mt-1 font-medium ${textPrimaryOnSurface}`}>
        {route === undefined ? (
          decision.heading
        ) : (
          <Link className={`${linkText} ${linkHoverText}`} to={buildReferencePath(route)}>
            {decision.heading}
          </Link>
        )}
      </h3>
      <MarkdownPreview
        className={`mt-1 ${textPrimaryOnSurface}`}
        lines={3}
        markdown={decision.text}
      />
    </article>
  );
}

/**
 * The decision cards, each beside the spec block it shaped: placed at that block's height in the
 * document, below the card before it where two would overlap, as the anchored comments are.
 */
export function DecisionList({
  blockPlacements,
  decisions,
  hoveredItemId,
  onSelect,
  selectedItemId,
}: {
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  decisions: readonly DecisionMarginItem[];
  hoveredItemId: string | undefined;
  onSelect(id: string, blockId: string): void;
  selectedItemId: string | undefined;
}): ReactNode {
  const region = useRef<HTMLElement>(null);
  const cardRefs = useRef(new Map<string, HTMLDivElement>());
  const [tops, setTops] = useState<ReadonlyMap<string, number>>(() => new Map());
  const measure = useCallback(() => {
    const origin = region.current === null ? undefined : regionOrigin(region.current);
    const next = new Map<string, number>();
    let previousBottom = 0;
    for (const item of decisions) {
      const placement = blockPlacements.get(item.blockId);
      const block = document.querySelector<HTMLElement>(
        `[data-block-id="${CSS.escape(item.blockId)}"]`
      );
      const wanted =
        placement === undefined
          ? previousBottom
          : block === null || origin === undefined
            ? placement.top
            : block.getBoundingClientRect().top - origin;
      const top = Math.max(wanted, previousBottom);
      next.set(item.id, top);
      previousBottom = top + (cardRefs.current.get(item.id)?.offsetHeight ?? 0) + 8;
    }
    setTops((current) =>
      current.size === next.size && [...next].every(([id, top]) => current.get(id) === top)
        ? current
        : next
    );
  }, [blockPlacements, decisions]);
  useLayoutEffect(() => {
    measure();
  }, [measure]);
  useLayoutEffect(() => {
    const observer = new ResizeObserver(measure);
    for (const element of cardRefs.current.values()) {
      observer.observe(element);
    }
    return () => observer.disconnect();
  }, [measure]);
  if (decisions.length === 0) {
    return null;
  }
  const bottom = Math.max(
    ...decisions.map(
      (item) => (tops.get(item.id) ?? 0) + (cardRefs.current.get(item.id)?.offsetHeight ?? 0) + 8
    )
  );
  return (
    <section aria-label="Decisions" className="relative" ref={region} style={{ minHeight: bottom }}>
      {decisions.map((item) => (
        <div
          key={item.id}
          ref={(element) => {
            if (element === null) {
              cardRefs.current.delete(item.id);
            } else {
              cardRefs.current.set(item.id, element);
            }
          }}
          style={{ left: 0, position: "absolute", right: 0, top: tops.get(item.id) ?? 0 }}
        >
          <DecisionCard
            hovered={hoveredItemId === item.id}
            item={item}
            onSelect={(id) => onSelect(id, item.blockId)}
            selected={selectedItemId === item.id}
          />
        </div>
      ))}
    </section>
  );
}
