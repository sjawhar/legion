import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  KeyboardSensor,
  PointerSensor,
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
import { useState } from "react";
import { Link } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { AttentionBadge, PriorityBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  cardHoverBorder,
  dragHandleBg,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import {
  type BoardColumn,
  groupIssuesByStatus,
  type IssueStatus,
  issueStatuses,
  rankInputForInsertion,
  statusLabel,
} from "./board-model";

const boardCollisionDetection: CollisionDetection = (args) => {
  const pointerCollisions = pointerWithin(args);
  return pointerCollisions.length > 0 ? pointerCollisions : closestCorners(args);
};

function moveIssue(
  issues: readonly IssueSummary[],
  activeKey: string,
  targetStatus: IssueStatus,
  overKey: string
): { issues: IssueSummary[]; rank: { before?: string; after?: string } } | undefined {
  const active = issues.find((issue) => issue.key === activeKey);
  if (active === undefined) {
    return undefined;
  }
  const columns = groupIssuesByStatus(issues.filter((issue) => issue.key !== activeKey));
  const target = columns.find((column) => column.status === targetStatus);
  if (target === undefined) {
    return undefined;
  }
  const overIndex = target.issues.findIndex((issue) => issue.key === overKey);
  const insertionIndex = overIndex < 0 ? target.issues.length : overIndex;
  const rank = rankInputForInsertion(target.issues, insertionIndex);
  target.issues.splice(insertionIndex, 0, { ...active, status: targetStatus });
  return { issues: columns.flatMap((column) => column.issues), rank };
}

function IssueCard({ issue }: { issue: IssueSummary }): ReactNode {
  const {
    attributes,
    isDragging,
    listeners,
    setActivatorNodeRef,
    setNodeRef,
    transform,
    transition,
  } = useSortable({ id: issue.key });
  const style: CSSProperties = {
    transform:
      transform === null ? undefined : `translate3d(${transform.x}px, ${transform.y}px, 0)`,
    transition,
  };

  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`rounded-xl border p-3 ${card} ${cardHoverBorder} ${isDragging ? "opacity-50" : ""}`}
      ref={setNodeRef}
      style={style}
    >
      <div className="flex items-start gap-2">
        <Link
          className={`min-w-0 flex-1 text-sm ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
        >
          <span className="font-semibold">{issue.key}</span>
          <span className={`mt-1 block ${textPrimaryOnSurface}`}>{issue.title}</span>
        </Link>
        <button
          aria-label={`Reorder ${issue.key}`}
          className={`min-h-11 shrink-0 rounded-lg border px-3 text-xs font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          ref={setActivatorNodeRef}
          type="button"
          {...attributes}
          {...listeners}
        >
          Reorder
        </button>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <StatusPill>{statusLabel(issue.status as IssueStatus)}</StatusPill>
        <PriorityBadge priority={issue.priority} />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks === 0 ? null : (
          <span title={`${issue.open_asks} open asks`}>
            <AttentionBadge count={issue.open_asks} />
          </span>
        )}
        <span aria-hidden className={`ml-auto h-1.5 w-6 rounded-full ${dragHandleBg}`} />
      </div>
    </article>
  );
}

function BoardColumnView({ column }: { column: BoardColumn }): ReactNode {
  const { isOver, setNodeRef } = useDroppable({
    id: `status:${column.status}`,
  });

  return (
    <section aria-label={statusLabel(column.status)} className="w-72 shrink-0" ref={setNodeRef}>
      <header
        className={`min-h-[52px] rounded-xl border px-3 py-2 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? cardHoverBorder : ""
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
              <IssueCard issue={issue} key={issue.key} />
            ))}
          </div>
        </SortableContext>
      )}
    </section>
  );
}

export function IssueBoard({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const issues = useQuery({
    queryKey: ["issues", "project", project],
    queryFn: () => api.listIssues({ project }),
  });
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 150, tolerance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates })
  );
  const onDragEnd = async ({ active, over }: DragEndEvent) => {
    if (over === null || issues.data === undefined) {
      return;
    }
    const activeKey = String(active.id);
    const activeIssue = issues.data.find((issue) => issue.key === activeKey);
    const overKey = String(over.id);
    const overIssue = issues.data.find((issue) => issue.key === overKey);
    const targetStatus = overIssue?.status ?? overKey.replace(/^status:/, "");
    if (activeKey === overKey && activeIssue?.status === targetStatus) {
      return;
    }
    if (activeIssue === undefined || !issueStatuses.includes(targetStatus as IssueStatus)) {
      return;
    }
    const moved = moveIssue(issues.data, activeKey, targetStatus as IssueStatus, overKey);
    if (moved === undefined) {
      return;
    }
    const queryKey = ["issues", "project", project] as const;
    const previous = issues.data;
    queryClient.setQueryData(queryKey, moved.issues);
    try {
      await api.patchIssue(activeKey, {
        ...(activeIssue.status === targetStatus ? {} : { status: targetStatus }),
        rank: moved.rank,
      });
      await queryClient.invalidateQueries({ queryKey });
    } catch (cause) {
      queryClient.setQueryData(queryKey, previous);
      setError(cause instanceof ApiError ? cause.message : "Could not reorder issue.");
    }
  };

  if (issues.isPending) {
    return <LoadingSkeleton label="Loading board" />;
  }
  if (issues.isError) {
    return <p className={textSecondaryOnSurface}>Could not load board.</p>;
  }

  return (
    <section aria-label="Project board" className="min-w-0">
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
          onDragEnd={onDragEnd}
          sensors={sensors}
        >
          <div className="overflow-x-auto pb-3" data-testid="board-scroll-container">
            <div className="flex w-max items-start gap-4">
              {groupIssuesByStatus(issues.data).map((column) => (
                <BoardColumnView column={column} key={column.status} />
              ))}
            </div>
          </div>
        </DndContext>
      )}
    </section>
  );
}
