import { type ReactNode, useCallback, useLayoutEffect, useRef, useState } from "react";

import { ThreadCard } from "./ThreadCard";
import type { MarginItemAction, MarkPlacement, Thread } from "./useMarginItems";
import { threadMarkId } from "./useMarginItems";

interface ThreadListProps {
  actionErrorId: string | undefined;
  artifactSlug: string;
  expandedThreadKey: string | undefined;
  hoveredItemId: string | undefined;
  hoveredMarkId: string | undefined;
  isClosed: boolean;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  onAction(id: string, action: MarginItemAction): void;
  onEdit(id: string, body: string): Promise<unknown>;
  onRetryAction(): void;
  onSelect(id: string): void;
  onToggle(key: string): void;
  onToggleResolved(): void;
  pendingActionId: string | undefined;
  resolvedThreads: Thread[];
  showResolved: boolean;
  threads: Thread[];
  viewerLogin: string;
}

function placementFor(
  thread: Thread,
  markPlacements: ReadonlyMap<string, MarkPlacement>
): MarkPlacement | undefined {
  const markId = threadMarkId(thread);
  return markId === undefined ? undefined : markPlacements.get(markId);
}

export function ThreadList({
  actionErrorId,
  artifactSlug,
  expandedThreadKey,
  hoveredItemId,
  hoveredMarkId,
  isClosed,
  markPlacements,
  onAction,
  onEdit,
  onRetryAction,
  onSelect,
  onToggle,
  onToggleResolved,
  pendingActionId,
  resolvedThreads,
  showResolved,
  threads,
  viewerLogin,
}: ThreadListProps): ReactNode {
  const anchoredRegion = useRef<HTMLElement>(null);
  const cardRefs = useRef(new Map<string, HTMLDivElement>());
  const [layoutTops, setLayoutTops] = useState<ReadonlyMap<string, number>>(() => new Map());
  const anchored = threads.filter((thread) => placementFor(thread, markPlacements) !== undefined);
  const discussion = threads.filter((thread) => placementFor(thread, markPlacements) === undefined);
  const topFor = useCallback(
    (thread: Thread) => {
      const placement = placementFor(thread, markPlacements);
      if (placement === undefined) {
        return 0;
      }
      const markId = threadMarkId(thread);
      const mark =
        markId === undefined
          ? null
          : document.querySelector<HTMLElement>(`[data-id="${CSS.escape(markId)}"]`);
      const region = anchoredRegion.current;
      return mark === null || region === null
        ? placement.top
        : mark.getBoundingClientRect().top - region.getBoundingClientRect().top;
    },
    [markPlacements]
  );
  const measure = useCallback(() => {
    const next = new Map<string, number>();
    let previousBottom = 0;
    for (const thread of anchored) {
      const top = Math.max(topFor(thread), previousBottom);
      next.set(thread.key, top);
      previousBottom = top + (cardRefs.current.get(thread.key)?.offsetHeight ?? 0) + 8;
    }
    setLayoutTops((current) => {
      if (current.size === next.size && [...next].every(([key, top]) => current.get(key) === top)) {
        return current;
      }
      return next;
    });
  }, [anchored, topFor]);

  useLayoutEffect(() => {
    measure();
  }, [measure]);

  useLayoutEffect(() => {
    const observer = new ResizeObserver(measure);
    for (const card of cardRefs.current.values()) {
      observer.observe(card);
    }
    return () => observer.disconnect();
  }, [measure]);

  const card = (thread: Thread, composerClassName?: string) => (
    <ThreadCard
      actionError={actionErrorId === thread.key}
      artifactSlug={artifactSlug}
      composerClassName={composerClassName}
      expanded={expandedThreadKey === thread.key}
      hovered={hoveredItemId === thread.key || hoveredMarkId === threadMarkId(thread)}
      isClosed={isClosed}
      key={thread.key}
      onAction={onAction}
      onEdit={onEdit}
      onRetryAction={onRetryAction}
      onSelect={() => onSelect(thread.key)}
      onToggle={() => onToggle(thread.key)}
      pendingAction={pendingActionId === thread.key}
      thread={thread}
      viewerLogin={viewerLogin}
    />
  );

  return (
    <div className="space-y-3">
      {anchored.length === 0 ? null : (
        <section
          aria-label="Anchored comments"
          className="relative"
          ref={anchoredRegion}
          style={{
            minHeight: Math.max(
              ...anchored.map((thread) => {
                const top = layoutTops.get(thread.key) ?? topFor(thread);
                return top + (cardRefs.current.get(thread.key)?.offsetHeight ?? 0) + 8;
              })
            ),
          }}
        >
          {anchored.map((thread) => (
            <div
              key={thread.key}
              ref={(element) => {
                if (element === null) {
                  cardRefs.current.delete(thread.key);
                } else {
                  cardRefs.current.set(thread.key, element);
                }
              }}
              style={{
                left: 0,
                position: "absolute",
                right: 0,
                top: layoutTops.get(thread.key) ?? topFor(thread),
              }}
            >
              {card(thread)}
            </div>
          ))}
        </section>
      )}
      {discussion.length === 0 ? null : (
        <section aria-label="Discussion" className="space-y-3">
          {discussion.map((thread) => card(thread))}
        </section>
      )}
      {resolvedThreads.length === 0 ? null : (
        <>
          <button
            aria-expanded={showResolved}
            className="min-h-11 text-sm font-medium"
            onClick={onToggleResolved}
            type="button"
          >
            Resolved ({resolvedThreads.length})
          </button>
          {showResolved ? (
            <section aria-label="Resolved" className="space-y-3">
              {resolvedThreads.map((thread) => card(thread))}
            </section>
          ) : null}
        </>
      )}
    </div>
  );
}
