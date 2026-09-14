import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  KeyboardSensor,
  MouseSensor,
  pointerWithin,
  TouchSensor,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { CSSProperties, ReactNode } from "react";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  cardHoverBorder,
  linkHoverText,
  linkText,
  selectedCardBorder,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { PriorityControl } from "../issue/PriorityControl";
import { referenceTriggerProps, refPreview } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { type BoardColumn, dropTarget, groupIssuesByStatus, statusLabel } from "./board-model";
import { useBoardMoves } from "./board-moves";
import { CollapsedColumn } from "./CollapsedColumn";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

const boardCollisionDetection: CollisionDetection = (args) => {
  const pointerCollisions = pointerWithin(args);
  return pointerCollisions.length > 0 ? pointerCollisions : closestCorners(args);
};

/**
 * The whole card is the drag activator: press and move 8 px (mouse) or hold 150 ms (touch)
 * anywhere on it to lift it, so a plain click on the title still follows the link and a
 * vertical swipe still scrolls. Mouse and touch are separate sensors on purpose: a pointer
 * sensor would also see a finger's first pointermove and lift the card for an instant before
 * the browser's pan cancels it. The card is also the keyboard activator - focus it, Space or
 * Enter lifts, arrows move, Space drops - and dnd-kit only treats a key press as a lift when
 * its target is the activator itself, so Enter on the title link navigates and Space on the
 * priority select opens it. dnd-kit's default drag attributes would make the card a `button`;
 * it stays an `article` so nothing interactive is nested in a control.
 */
function IssueCard({ issue, unread }: { issue: IssueSummary; unread: boolean }): ReactNode {
  const {
    attributes,
    isDragging,
    listeners,
    setActivatorNodeRef,
    setNodeRef,
    transform,
    transition,
  } = useSortable({
    attributes: { role: "article", roleDescription: "card" },
    id: issue.key,
  });
  const setCardRef = useCallback(
    (node: HTMLElement | null) => {
      setNodeRef(node);
      setActivatorNodeRef(node);
    },
    [setActivatorNodeRef, setNodeRef]
  );
  const style: CSSProperties = {
    transform:
      transform === null ? undefined : `translate3d(${transform.x}px, ${transform.y}px, 0)`,
    transition,
  };

  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`cursor-grab rounded-xl border p-3 ${card} ${cardHoverBorder} ${
        isDragging ? "opacity-50" : ""
      }`}
      ref={setCardRef}
      style={style}
      {...attributes}
      {...listeners}
    >
      {/* Below `xl` every anchor is `inline-flex` with centred items (the 44 px touch rule in
          styles.css), so the key and title are stacked and start-aligned explicitly. */}
      <Link
        className={`flex flex-col text-sm ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
        {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
      >
        <span className="self-start font-semibold">{issue.key}</span>
        <span className={`mt-1 self-start ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Link>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {unread ? <UnreadDot /> : null}
        <PriorityControl
          disabled={issue.status === "done"}
          issueKey={issue.key}
          priority={issue.priority}
        />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks === 0 ? null : (
          <span title={`${issue.open_asks} open asks`}>
            <AttentionBadge count={issue.open_asks} />
          </span>
        )}
      </div>
    </article>
  );
}

/**
 * One lifecycle column: the `status:<s>` droppable. Every column stretches to the board's
 * height, so a card can be dropped anywhere in a column's body - below its last card, or into
 * an empty column whose header has scrolled out of view on a phone - not only on the header.
 */
const BoardColumnView = memo(function BoardColumnView({
  column,
  userState,
}: {
  column: BoardColumn;
  userState: UserState | undefined;
}): ReactNode {
  const { isOver, setNodeRef } = useDroppable({
    id: `status:${column.status}`,
  });

  return (
    <section
      aria-label={statusLabel(column.status)}
      className="w-72 shrink-0 snap-start"
      ref={setNodeRef}
    >
      <header
        className={`min-h-[52px] rounded-xl border px-3 py-2 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-header"
      >
        <div
          className={`flex items-center justify-between gap-2 text-base font-semibold ${textPrimaryOnSurface}`}
        >
          <StatusPill>{statusLabel(column.status)}</StatusPill>
          <Pill>{column.issues.length}</Pill>
        </div>
      </header>
      {column.issues.length === 0 ? null : (
        <SortableContext
          items={column.issues.map((issue) => issue.key)}
          strategy={verticalListSortingStrategy}
        >
          <div className="mt-3 space-y-3">
            {column.issues.map((issue) => (
              <IssueCard
                issue={issue}
                key={issue.key}
                unread={issueIsUnread(issue, userState?.[issue.key]?.last_read_seq ?? 0)}
              />
            ))}
          </div>
        </SortableContext>
      )}
    </section>
  );
});

export function IssueBoard({
  project,
  showEdges = false,
}: {
  project: string;
  /** Render Icebox and Done as full columns instead of collapsed rails. */
  showEdges?: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const queryKey = ["issues", "project", project] as const;
  const issues = useQuery({
    queryKey,
    queryFn: () => api.listIssues({ project }),
  });
  const userState = useQuery({
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
  const { error, moveCard } = useBoardMoves(project);
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 150, tolerance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates })
  );
  // While a card is in flight the scroller must not snap: dnd-kit auto-scrolls the board when
  // the pointer nears an edge, and with mandatory snapping every scroll tick jumps a whole
  // column, so one drag near the edge flies to the far end. Snapping is for finger swipes at
  // rest, so it is switched off from lift to drop.
  const [dragging, setDragging] = useState(false);
  // The click that trails a drop (mouseup or touchend on the card) is stopped by dnd-kit before
  // React sees it, which is enough for buttons but not for the title link: the browser's own
  // navigation still runs and reloads the app on the issue page. Cancel that default for the
  // instant after a card lands, on the board only - a keyboard lift stays active until Space
  // or Escape, and a link elsewhere on the page must keep working meanwhile.
  const boardRef = useRef<HTMLElement>(null);
  const dragEndedAt = useRef(Number.NEGATIVE_INFINITY);
  useEffect(() => {
    const cancelTrailingClick = (event: MouseEvent) => {
      if (
        performance.now() - dragEndedAt.current < 150 &&
        event.target instanceof Node &&
        boardRef.current?.contains(event.target)
      ) {
        event.preventDefault();
      }
    };
    window.addEventListener("click", cancelTrailingClick, { capture: true });
    return () => window.removeEventListener("click", cancelTrailingClick, { capture: true });
  }, []);
  // The title link is both the hover-card trigger and part of the whole-card drag activator, and
  // the click that would dismiss an open card after the drop is the one swallowed above - so a
  // lift closes the card (open or still pending) itself.
  const liftCard = () => {
    refPreview.close();
    setDragging(true);
  };
  const landCard = () => {
    dragEndedAt.current = performance.now();
    setDragging(false);
  };
  const columns = useMemo(
    () => (issues.data === undefined ? [] : groupIssuesByStatus(issues.data)),
    [issues.data]
  );
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    landCard();
    const current = queryClient.getQueryData<IssueSummary[]>(queryKey);
    if (over === null || current === undefined) {
      return;
    }
    const target = dropTarget(current, String(active.id), String(over.id));
    if (target !== undefined) {
      void moveCard(String(active.id), target.status, target.insertionIndex);
    }
  };

  if (issues.isPending) {
    return <LoadingSkeleton label="Loading board" />;
  }
  if (issues.isError) {
    return <p className={textSecondaryOnSurface}>Could not load board.</p>;
  }

  return (
    <section aria-label="Project board" className="min-w-0" ref={boardRef}>
      {error === undefined ? null : (
        <div
          aria-live="assertive"
          className={`mb-4 rounded-xl border px-4 py-3 text-sm ${card} ${textPrimaryOnSurface}`}
          role="alert"
        >
          {error}
        </div>
      )}
      {issues.data.length === 0 ? (
        <EmptyState label="Empty project board" message="No issues in this project." />
      ) : (
        <DndContext
          collisionDetection={boardCollisionDetection}
          onDragCancel={landCard}
          onDragEnd={onDragEnd}
          onDragStart={liftCard}
          sensors={sensors}
        >
          <div
            className={`overflow-x-auto pb-3 ${dragging ? "" : "snap-x snap-mandatory"}`}
            data-testid="board-scroll-container"
          >
            <div className="flex w-max items-stretch gap-4">
              {columns.map((column) =>
                !showEdges && (column.status === "icebox" || column.status === "done") ? (
                  <CollapsedColumn column={column} key={column.status} />
                ) : (
                  <BoardColumnView column={column} key={column.status} userState={userState.data} />
                )
              )}
            </div>
          </div>
        </DndContext>
      )}
    </section>
  );
}
