import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  MouseSensor,
  pointerWithin,
  TouchSensor,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import { SortableContext, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { CSSProperties, ReactNode } from "react";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { userStateQuery } from "../../api/queries";
import type { IssueSummary, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import { copyText } from "../../lib/clipboard";
import {
  borderDefault,
  card,
  cardHoverBorder,
  focusVisibleRing,
  linkHoverText,
  linkText,
  selectedCardBorder,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { PriorityControl } from "../issue/PriorityControl";
import { referenceTriggerProps, refPreview } from "../refs/RefPreview";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching } from "../shell/roving";
import {
  announceMove,
  type BoardFocus,
  focusTarget,
  keyboardMove,
  type MoveDirection,
  type RoveKey,
} from "./board-keys";
import {
  type BoardColumn,
  dropTarget,
  groupIssuesByStatus,
  type IssueStatus,
  statusLabel,
} from "./board-model";
import { useBoardMoves } from "./board-moves";
import { CollapsedColumn } from "./CollapsedColumn";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

const boardCollisionDetection: CollisionDetection = (args) => {
  const pointerCollisions = pointerWithin(args);
  return pointerCollisions.length > 0 ? pointerCollisions : closestCorners(args);
};

const CARD_SELECTOR = "[data-board-card]";
const COLUMN_SELECTOR = "[data-board-column]";

/** The focus ring every board focus target draws: cards, columns and rails alike. */
const boardFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

function cardAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, CARD_SELECTOR);
}

function columnAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, COLUMN_SELECTOR);
}

/**
 * The whole card is the drag activator: press and move 8 px (mouse) or hold 150 ms (touch)
 * anywhere on it to lift it, so a plain click on the title still follows the link and a
 * vertical swipe still scrolls. Mouse and touch are separate sensors on purpose: a pointer
 * sensor would also see a finger's first pointermove and lift the card for an instant before
 * the browser's pan cancels it. The keyboard never lifts a card: the board's registry rows
 * (`j`/`k`/`h`/`l` rove focus, `Shift+J/K/H/L` move through `moveCard`) are the accessible path,
 * so the card is `tabIndex={-1}` - reached by those keys, not by Tab - and `data-board-card`
 * names it for them. dnd-kit's default drag attributes would make the card a `button`; it stays
 * an `article` so nothing interactive is nested in a control.
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
    attributes: { role: "article", roleDescription: "card", tabIndex: -1 },
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
      className={`cursor-grab rounded-xl border p-3 ${card} ${cardHoverBorder} ${boardFocusRing} ${
        isDragging ? "opacity-50" : ""
      }`}
      data-board-card={issue.key}
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
      className={`w-72 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={column.status}
      ref={setNodeRef}
      tabIndex={-1}
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
  const { labels, matches } = useIssueFilters();
  // The same query-key pair the List keeps: the plain project list, or the server-filtered
  // list while labels are active - shared keys, shared caches.
  const queryKey = projectIssuesQueryKey(project, labels);
  const allIssues = useQuery({
    queryKey: projectIssuesQueryKey(project, []),
    queryFn: () => api.listIssues({ project }),
  });
  const labelledIssues = useQuery({
    enabled: labels.length > 0,
    queryKey: projectIssuesQueryKey(project, labels),
    queryFn: () => api.listIssues({ labels, project }),
  });
  const issues = labels.length === 0 ? allIssues : labelledIssues;
  const userState = useQuery(userStateQuery());
  /** The strip's client-side filters against this viewer's read state: what the board renders,
   *  what drop indices count, and what the PATCH names as neighbours. */
  const isVisible = useCallback(
    (issue: IssueSummary) => matches(issue, userState.data?.[issue.key]?.last_read_seq ?? 0),
    [matches, userState.data]
  );
  const { error, moveCard } = useBoardMoves(project, labels, isVisible);
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 150, tolerance: 5 } })
  );
  // While a card is in flight the scroller must not snap: dnd-kit auto-scrolls the board when
  // the pointer nears an edge, and with mandatory snapping every scroll tick jumps a whole
  // column, so one drag near the edge flies to the far end. Snapping is for finger swipes at
  // rest, so it is switched off from lift to drop.
  const [dragging, setDragging] = useState(false);
  // The click that trails a drop (mouseup or touchend on the card) is stopped by dnd-kit before
  // React sees it, which is enough for buttons but not for the title link: the browser's own
  // navigation still runs and reloads the app on the issue page. Cancel that default for the
  // instant after a card lands, on the board only - a link elsewhere on the page must keep
  // working meanwhile.
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
    () => (issues.data === undefined ? [] : groupIssuesByStatus(issues.data.filter(isVisible))),
    [issues.data, isVisible]
  );
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    landCard();
    const current = queryClient.getQueryData<IssueSummary[]>(queryKey);
    if (over === null || current === undefined) {
      return;
    }
    // Indices count the rendered - visible - cards, so `moveCard` sends visible neighbours.
    const target = dropTarget(current.filter(isVisible), String(active.id), String(over.id));
    if (target !== undefined) {
      void moveCard(String(active.id), target.status, target.insertionIndex);
    }
  };
  /** Icebox and Done are rails - no cards to focus, still a move target - while the edges are
   *  hidden. The render below and the roving keys read the same rule. */
  const isCollapsed = (status: IssueStatus) =>
    !showEdges && (status === "icebox" || status === "done");

  // One `aria-live` sentence per keyboard move; `focusAfterMove` names the card focus follows
  // once the optimistic list has rendered (a status move remounts the article under its new
  // column, so the old node cannot keep focus; a card moved into a rail has no node, and focus
  // lands on the rail). The effect runs on the `columns` render - the announcement's own render
  // comes first, before the query observer has notified, and would focus the doomed node.
  const [announcement, setAnnouncement] = useState("");
  const focusAfterMove = useRef<{ key: string; status: IssueStatus } | null>(null);
  const boardNode = (selector: string) => boardRef.current?.querySelector<HTMLElement>(selector);
  // biome-ignore lint/correctness/useExhaustiveDependencies: `columns` is the trigger, not a read; `boardNode` reads a ref
  useEffect(() => {
    const pending = focusAfterMove.current;
    if (pending === null) {
      return;
    }
    focusAfterMove.current = null;
    (
      boardNode(`[data-board-card="${pending.key}"]`) ??
      boardNode(`[data-board-column="${pending.status}"]`)
    )?.focus();
  }, [columns]);
  /** Where keyboard focus sits on the board right now, or `null` when it is elsewhere. */
  const currentFocus = (): BoardFocus | null => {
    const active = document.activeElement;
    const column = columnAround(active);
    if (column === null) {
      return null;
    }
    const status = column.dataset.boardColumn as IssueStatus;
    const key = cardAround(active)?.dataset.boardCard;
    const index =
      key === undefined
        ? -1
        : (columns
            .find((entry) => entry.status === status)
            ?.issues.findIndex((issue) => issue.key === key) ?? -1);
    return { status, index: index === -1 ? null : index };
  };
  const rove = (key: RoveKey) => {
    const target = focusTarget(columns, isCollapsed, currentFocus(), key);
    const issue =
      target.index === null
        ? undefined
        : columns.find((entry) => entry.status === target.status)?.issues[target.index];
    (issue === undefined
      ? boardNode(`[data-board-column="${target.status}"]`)
      : boardNode(`[data-board-card="${issue.key}"]`)
    )?.focus();
  };
  const move = (direction: MoveDirection) => {
    const key = cardAround(document.activeElement)?.dataset.boardCard;
    if (key === undefined) {
      return;
    }
    const target = keyboardMove(columns, key, direction);
    if (target === undefined) {
      return;
    }
    // `moveCard` places the card in the cache synchronously before its request goes out, so
    // the announcement reads the optimistic position - the one the user sees.
    void moveCard(key, target.status, target.insertionIndex);
    // The announcement's position and count are the visible column - the one the user sees.
    const column = (queryClient.getQueryData<IssueSummary[]>(queryKey) ?? []).filter(
      (issue) => issue.status === target.status && isVisible(issue)
    );
    const position = column.findIndex((issue) => issue.key === key);
    setAnnouncement(announceMove(key, target.status, position + 1, column.length));
    focusAfterMove.current = { key, status: target.status };
  };
  const focusedCard = () => cardAround(document.activeElement) !== null;
  /** Copies the focused card's key or `dispatch://` reference and announces which. */
  const copyFocusedCard = (form: "key" | "reference") => {
    const key = cardAround(document.activeElement)?.dataset.boardCard;
    if (key === undefined) {
      return;
    }
    const text = form === "key" ? key : buildDispatchReference({ key, kind: "issue" });
    void copyText(text).then((copied) =>
      setAnnouncement(copied ? `Copied ${text}` : `Copy failed - ${text}`)
    );
  };
  const focusedBoardNode = () =>
    document.activeElement?.matches(`${CARD_SELECTOR}, ${COLUMN_SELECTOR}`) === true;
  useKeymapScope("board");
  useKeymap("board", [
    { id: "next", keys: "j", label: "Next card", run: () => rove("j") },
    { id: "previous", keys: "k", label: "Previous card", run: () => rove("k") },
    { id: "column", keys: "l", label: "Next column", run: () => rove("l") },
    { id: "previous-column", keys: "h", label: "Previous column", run: () => rove("h") },
    {
      id: "arrows-vertical",
      keys: ["ArrowDown", "ArrowUp"],
      label: "Next / previous card while one is focused",
      run: (event) => rove(event.key === "ArrowDown" ? "j" : "k"),
      when: focusedBoardNode,
    },
    {
      id: "arrows-horizontal",
      keys: ["ArrowRight", "ArrowLeft"],
      label: "Next / previous column while one is focused",
      run: (event) => rove(event.key === "ArrowRight" ? "l" : "h"),
      when: focusedBoardNode,
    },
    {
      id: "move-down",
      keys: "Shift+J",
      label: "Move card down",
      run: () => move("down"),
      when: focusedCard,
    },
    {
      id: "move-up",
      keys: "Shift+K",
      label: "Move card up",
      run: () => move("up"),
      when: focusedCard,
    },
    {
      id: "move-next",
      keys: "Shift+L",
      label: "Move card to the next status",
      run: () => move("next"),
      when: focusedCard,
    },
    {
      id: "move-previous",
      keys: "Shift+H",
      label: "Move card to the previous status",
      run: () => move("prev"),
      when: focusedCard,
    },
    {
      id: "open",
      keys: "o",
      label: "Open issue",
      run: () => cardAround(document.activeElement)?.querySelector("a")?.click(),
      when: focusedCard,
    },
    {
      // Only from the card itself: Enter on the title link is the browser's own navigation.
      id: "open-enter",
      keys: "Enter",
      label: "Open the focused card's issue",
      run: () => cardAround(document.activeElement)?.querySelector("a")?.click(),
      when: () => document.activeElement?.matches(CARD_SELECTOR) === true,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the card's priority",
      run: () => cardAround(document.activeElement)?.querySelector("select")?.focus(),
      when: focusedCard,
    },
    {
      id: "copy-ref",
      keys: "y",
      label: "Copy the card's issue reference",
      run: () => copyFocusedCard("reference"),
      when: focusedCard,
    },
    {
      id: "copy-key",
      keys: "Shift+Y",
      label: "Copy the card's issue key",
      run: () => copyFocusedCard("key"),
      when: focusedCard,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the card, then out",
      run: () => {
        const active = document.activeElement;
        const card = cardAround(active);
        if (card === active || (card === null && columnAround(active) === active)) {
          (active as HTMLElement).blur();
        } else {
          card?.focus();
        }
      },
      when: () =>
        cardAround(document.activeElement) !== null ||
        columnAround(document.activeElement) === document.activeElement,
    },
  ]);
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
      <p aria-label="Board announcements" aria-live="polite" className="sr-only" role="status">
        {announcement}
      </p>
      {issues.data.length === 0 ? (
        // The active query is the label-filtered list, so an empty answer under a label filter
        // means "nothing matches", not "empty project" - the same copy the List shows.
        labels.length === 0 ? (
          <EmptyState label="Empty project board" message="No issues in this project." />
        ) : (
          <EmptyState label="No matching project issues" message="No issues match these filters." />
        )
      ) : (
        <DndContext
          accessibility={{
            screenReaderInstructions: {
              draggable:
                "Focus a card and press Shift with J, K, H or L to move it; press ? for every shortcut.",
            },
          }}
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
                isCollapsed(column.status) ? (
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
