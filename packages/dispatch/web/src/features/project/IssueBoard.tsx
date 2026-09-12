import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  type DragStartEvent,
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
import {
  badgeBlocking,
  borderDefault,
  card,
  cardHoverBorder,
  dragHandleBg,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import {
  type BoardColumn,
  groupIssuesByStatus,
  type IssueStatus,
  isHumanSettableStatus,
  issueStatuses,
  rankInputForInsertion,
} from "./board-model";

const statusLabels: Record<IssueStatus, string> = {
  triage: "Triage",
  icebox: "Icebox",
  backlog: "Backlog",
  todo: "Todo",
  in_progress: "In progress",
  testing: "Testing",
  needs_review: "Needs review",
  retro: "Retro",
  done: "Done",
};

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

function IssueCard({ issue, disabled }: { issue: IssueSummary; disabled: boolean }): ReactNode {
  const {
    attributes,
    isDragging,
    listeners,
    setActivatorNodeRef,
    setNodeRef,
    transform,
    transition,
  } = useSortable({ disabled, id: issue.key });
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
          className={`min-h-11 shrink-0 rounded-lg border px-3 text-xs font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          disabled={disabled}
          ref={setActivatorNodeRef}
          type="button"
          {...attributes}
          {...listeners}
        >
          Reorder
        </button>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <span
          className={`rounded-full px-2 py-1 text-xs font-medium ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
        >
          {statusLabels[issue.status as IssueStatus] ?? issue.status}
        </span>
        {(issue.labels ?? []).map((label) => (
          <span
            className={`rounded-full px-2 py-1 text-xs ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
            key={label}
          >
            {label}
          </span>
        ))}
        {issue.open_asks === 0 ? null : (
          <span
            className={`rounded-full px-2 py-1 text-xs font-medium ${badgeBlocking.bg} ${badgeBlocking.text}`}
            title={`${issue.open_asks} open asks`}
          >
            {issue.open_asks}
          </span>
        )}
        <span aria-hidden className={`ml-auto h-1.5 w-6 rounded-full ${dragHandleBg}`} />
      </div>
    </article>
  );
}

function BoardColumnView({
  activeStatus,
  column,
}: {
  activeStatus: IssueStatus | undefined;
  column: BoardColumn;
}): ReactNode {
  const humanSettable = isHumanSettableStatus(column.status);
  const dropDisabled =
    activeStatus !== undefined && activeStatus !== column.status && !humanSettable;
  const { isOver, setNodeRef } = useDroppable({
    id: `status:${column.status}`,
  });

  const daemonReason = `Only the Legion daemon can move issues to ${statusLabels[column.status]}.`;

  return (
    <section
      aria-label={statusLabels[column.status]}
      className={`shrink-0 ${column.issues.length === 0 ? "w-40" : "w-72"}`}
      data-drop-disabled={dropDisabled ? "true" : undefined}
      ref={setNodeRef}
    >
      <header
        className={`rounded-xl border px-3 py-3 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? cardHoverBorder : ""
        }`}
        title={humanSettable ? undefined : daemonReason}
      >
        <div
          className={`flex items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
        >
          <span>{statusLabels[column.status]}</span>
          <span
            className={`rounded-full px-2 py-1 text-xs ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
          >
            {column.issues.length}
          </span>
        </div>
        {humanSettable ? null : (
          <p className={`mt-2 text-xs ${textMutedOnSurfaceMuted}`}>{daemonReason}</p>
        )}
      </header>
      {column.issues.length === 0 ? null : (
        <SortableContext
          items={column.issues.map((issue) => issue.key)}
          strategy={verticalListSortingStrategy}
        >
          <div className="mt-3 space-y-3">
            {column.issues.map((issue) => (
              <IssueCard disabled={dropDisabled} issue={issue} key={issue.key} />
            ))}
          </div>
        </SortableContext>
      )}
    </section>
  );
}

export function IssueBoard({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const [activeStatus, setActiveStatus] = useState<IssueStatus>();
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
  const onDragStart = ({ active }: DragStartEvent) => {
    const issue = issues.data?.find((candidate) => candidate.key === active.id);
    if (issue !== undefined && issueStatuses.includes(issue.status as IssueStatus)) {
      setActiveStatus(issue.status as IssueStatus);
    }
  };
  const onDragEnd = async ({ active, over }: DragEndEvent) => {
    setActiveStatus(undefined);
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
    if (
      activeIssue === undefined ||
      !issueStatuses.includes(targetStatus as IssueStatus) ||
      (activeIssue.status !== targetStatus && !isHumanSettableStatus(targetStatus as IssueStatus))
    ) {
      if (issueStatuses.includes(targetStatus as IssueStatus)) {
        setError(
          `Only the Legion daemon can move issues to ${statusLabels[targetStatus as IssueStatus]}.`
        );
      }
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
    return <p className={textMutedOnCanvas}>Loading board…</p>;
  }
  if (issues.isError) {
    return <p className={textMutedOnCanvas}>Could not load board.</p>;
  }

  return (
    <section aria-label="Project board">
      {error === undefined ? null : (
        <div
          aria-live="assertive"
          className={`mb-4 rounded-xl border px-4 py-3 text-sm ${card} ${textPrimaryOnSurface}`}
          role="alert"
        >
          {error}
        </div>
      )}
      <DndContext
        collisionDetection={boardCollisionDetection}
        onDragCancel={() => setActiveStatus(undefined)}
        onDragEnd={onDragEnd}
        onDragStart={onDragStart}
        sensors={sensors}
      >
        <div className="overflow-x-auto pb-3">
          <div className="flex min-w-max items-start gap-4">
            {groupIssuesByStatus(issues.data ?? []).map((column) => (
              <BoardColumnView activeStatus={activeStatus} column={column} key={column.status} />
            ))}
          </div>
        </div>
      </DndContext>
    </section>
  );
}
