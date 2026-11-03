import { type ReactNode, useState } from "react";

import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  focusVisibleRing,
  surfaceMutedBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import {
  INBOX_FILTER_FIELDS,
  INBOX_PRIORITY_VALUES,
  INBOX_TYPE_VALUES,
  INBOX_URGENCY_VALUES,
  type InboxFilterField,
  type InboxGroupBy,
  type InboxSort,
  NO_FILTER_VALUE,
} from "../refs/routes";
import { AssigneeFilter } from "./AssigneeFilter";
import {
  DIMENSION_LABELS,
  dimensionValue,
  dimensionValueLabel,
  filterCounts,
  inFieldFilter,
} from "./grouping";
import type { InboxListRow } from "./list-row";
import type { ThreadOrder } from "./preferences";
import { EVERYONE_FILTER_VALUE, inAssigneeFilter, UNASSIGNED_FILTER_VALUE } from "./sections";
import { ViewMenu } from "./ViewMenu";

/** The chosen values of every field the address carries as a comma list. */
export type InboxFilters = Partial<Record<InboxFilterField, readonly string[]>>;

/** One removable thing the reader narrowed the list by. */
interface FilterToken {
  key: string;
  label: string;
  remove: () => void;
}

/** The values a field offers, in the order its popover lists them. Project is the one field
 *  whose vocabulary is the workspace's, so it is read off the rows themselves. */
function fieldOptions(
  field: InboxFilterField,
  rows: readonly InboxListRow[],
  viewer: string,
  chosen: readonly string[]
): string[] {
  let listed: readonly string[];
  if (field === "priority") listed = INBOX_PRIORITY_VALUES;
  else if (field === "urgency") listed = INBOX_URGENCY_VALUES;
  else if (field === "type") listed = INBOX_TYPE_VALUES;
  else {
    const keys = new Set<string>();
    for (const row of rows) {
      const value = dimensionValue(row, "project", viewer);
      if (value !== NO_FILTER_VALUE) keys.add(value);
    }
    listed = [...keys].sort((left, right) => left.localeCompare(right)).concat(NO_FILTER_VALUE);
  }
  // A value the address carries that this list has no row for is still a row of the popover,
  // so the reader can always see what they are filtering by and switch it off.
  return [...listed, ...chosen.filter((value) => !listed.includes(value))];
}

/**
 * The one bar above the Inbox's list: **Filter** at the left, **View** at the right end, and
 * beneath them the tokens of whatever the reader narrowed the list by. It is a sibling of the
 * scrolling list rather than an item in it, so it stays put while the list moves.
 *
 * **Filter** opens the five fields the Inbox filters by - priority, urgency, type, project and
 * assignee - each a searchable multi-select whose rows carry how many rows that value holds.
 * Every field's counts are taken under the OTHER four fields' filters, so a value the narrowed
 * list has something for never reads 0 and ticking one never empties the list by surprise.
 */
export function InboxBar({
  agentToken,
  allThreadCount,
  assignees,
  filters,
  group,
  onAssigneesChange,
  onClearAgent,
  onFiltersChange,
  onGroupChange,
  onPopoverOpenChange,
  onShowReadThreadsChange,
  onSortChange,
  onThreadOrderChange,
  rows,
  selection,
  showReadThreads,
  sort,
  threadOrder,
  unreadThreadCount,
  viewer,
}: {
  /** The `?agent=` chip, as the Inbox draws it (an `AgentName` whose control clears the
   *  filter, with the agent's live link beside it): one of the tokens, and the only one the
   *  bar does not build for itself, because naming an agent is `AgentName`'s job. */
  agentToken: ReactNode;
  /** Undefined while the list is on Unread, where that count is not read (`ViewMenu`). */
  allThreadCount: number | undefined;
  /** `?assignee=` as the address carries it: undefined is the default, the reader alone. */
  assignees: readonly string[] | undefined;
  filters: InboxFilters;
  group: InboxGroupBy;
  onAssigneesChange: (next: readonly string[] | undefined) => void;
  onClearAgent: () => void;
  onFiltersChange: (next: InboxFilters) => void;
  onGroupChange: (next: InboxGroupBy) => void;
  /** Whether any of the bar's popovers is open, which the Inbox's Escape binding stands down
   *  for: one press closes the popover rather than also clearing what the reader marked. */
  onPopoverOpenChange: (open: boolean) => void;
  onShowReadThreadsChange: (next: boolean) => void;
  onSortChange: (next: InboxSort) => void;
  onThreadOrderChange: (next: ThreadOrder) => void;
  /** Every row the list could hold before these five fields narrow it: what the counts count. */
  rows: readonly InboxListRow[];
  /** Who the list is for, as the assignee filter resolves it (the reader alone by default). */
  selection: readonly string[];
  showReadThreads: boolean;
  sort: InboxSort;
  threadOrder: ThreadOrder;
  unreadThreadCount: number;
  viewer: string;
}): ReactNode {
  const [fieldsOpen, setFieldsOpen] = useState(false);
  /** Which single popover is open: one of the five fields, the View menu, or none. */
  const [openControl, setOpenControl] = useState<string | undefined>(undefined);
  const openPopover = (name: string, open: boolean) => {
    setOpenControl(open ? name : undefined);
    onPopoverOpenChange(open);
  };

  /** Whether a row survives every filter but `field`'s own, which is what that field counts
   *  against: a reader opening Priority is told what each priority holds inside the list they
   *  have already narrowed. */
  const matchesOthers = (row: InboxListRow, field: InboxFilterField | "assignee"): boolean =>
    INBOX_FILTER_FIELDS.every(
      (other) => other === field || inFieldFilter(row, other, filters[other], viewer)
    ) &&
    (field === "assignee" || inAssigneeFilter(row, selection, viewer));

  const chooseField = (field: InboxFilterField, chosen: readonly string[]) => {
    const next: InboxFilters = { ...filters };
    if (chosen.length === 0) delete next[field];
    else next[field] = chosen;
    onFiltersChange(next);
  };

  const tokens: FilterToken[] = [];
  for (const field of INBOX_FILTER_FIELDS) {
    for (const value of filters[field] ?? []) {
      tokens.push({
        key: `${field}:${value}`,
        label: `${DIMENSION_LABELS[field]}: ${dimensionValueLabel(field, value)}`,
        remove: () =>
          chooseField(
            field,
            (filters[field] ?? []).filter((current) => current !== value)
          ),
      });
    }
  }
  if (assignees !== undefined) {
    if (assignees.length === 0) {
      tokens.push({
        key: `assignee:${EVERYONE_FILTER_VALUE}`,
        label: "Assignee: everyone",
        remove: () => onAssigneesChange(undefined),
      });
    }
    for (const value of assignees) {
      tokens.push({
        key: `assignee:${value}`,
        label: `Assignee: ${value === UNASSIGNED_FILTER_VALUE ? "Unassigned" : value}`,
        remove: () => {
          const kept = assignees.filter((current) => current !== value);
          onAssigneesChange(kept.length === 0 ? undefined : kept);
        },
      });
    }
  }

  const clearAll = () => {
    onClearAgent();
    onFiltersChange({});
    onAssigneesChange(undefined);
  };

  return (
    <div className="space-y-2" data-inbox-bar="">
      <div className="flex flex-wrap items-center gap-2">
        <button
          aria-expanded={fieldsOpen}
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold outline-none focus-visible:ring-2 ${focusVisibleRing} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
          data-inbox-filter=""
          onClick={() => setFieldsOpen((open) => !open)}
          type="button"
        >
          Filter
        </button>
        {/* The right end of the row. **Mark all read** sits between these two. */}
        <div className="ml-auto">
          <ViewMenu
            allThreadCount={allThreadCount}
            group={group}
            onGroupChange={onGroupChange}
            onOpenChange={(open) => openPopover("view", open)}
            onShowReadThreadsChange={onShowReadThreadsChange}
            onSortChange={onSortChange}
            onThreadOrderChange={onThreadOrderChange}
            open={openControl === "view"}
            showReadThreads={showReadThreads}
            sort={sort}
            threadOrder={threadOrder}
            unreadThreadCount={unreadThreadCount}
          />
        </div>
      </div>
      {fieldsOpen ? (
        <div className="flex flex-wrap items-center gap-2" data-inbox-filter-fields="">
          {INBOX_FILTER_FIELDS.map((field) => {
            const chosen = filters[field] ?? [];
            const counts = filterCounts(rows, field, (row) => matchesOthers(row, field), viewer);
            return (
              <MultiSelect
                emptyMessage={`No matching ${DIMENSION_LABELS[field].toLowerCase()}.`}
                key={field}
                label={DIMENSION_LABELS[field]}
                onChange={(next) => chooseField(field, next)}
                onOpenChange={(open) => openPopover(field, open)}
                open={openControl === field}
                optionDetail={(value) => `${counts.get(value) ?? 0}`}
                optionDetailLabel={(value) => `${counts.get(value) ?? 0} rows`}
                optionLabel={(value) => dimensionValueLabel(field, value)}
                options={fieldOptions(field, rows, viewer, chosen)}
                searchLabel={`Search ${DIMENSION_LABELS[field].toLowerCase()}`}
                selected={chosen}
                triggerAriaLabel={`Filter by ${DIMENSION_LABELS[field].toLowerCase()}`}
              />
            );
          })}
          <AssigneeFilter
            counts={filterCounts(rows, "assignee", (row) => matchesOthers(row, "assignee"), viewer)}
            onChange={(next) => onAssigneesChange(next)}
            onOpenChange={(open) => openPopover("assignee", open)}
            open={openControl === "assignee"}
            selected={selection}
            viewer={viewer}
          />
        </div>
      ) : null}
      {agentToken === undefined && tokens.length === 0 ? null : (
        <div className="flex flex-wrap items-center gap-2" data-inbox-filter-tokens="">
          {agentToken}
          {tokens.map((token) => (
            <button
              aria-label={`Remove filter ${token.label}`}
              className={`inline-flex min-h-11 shrink-0 items-center gap-1 rounded-full border px-3 text-xs font-medium outline-none focus-visible:ring-2 ${focusVisibleRing} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
              data-inbox-filter-token={token.key}
              key={token.key}
              onClick={token.remove}
              type="button"
            >
              {token.label}
              <span aria-hidden="true">×</span>
            </button>
          ))}
          <button
            className={`min-h-11 rounded-lg px-2 text-xs font-medium outline-none focus-visible:ring-2 ${focusVisibleRing} ${textSecondaryOnCanvas}`}
            data-inbox-clear-filters=""
            onClick={clearAll}
            type="button"
          >
            Clear filters
          </button>
        </div>
      )}
    </div>
  );
}
