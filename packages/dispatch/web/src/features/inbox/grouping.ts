/**
 * How the Inbox orders and bands its rows.
 *
 * Five fields describe a row - its priority, its urgency, what kind of row it is, its project
 * and who holds it - and the reader filters, sorts and groups by those same five, so each one
 * is read in exactly one place here (`dimensionValue`) and the bar's counts, the Filter
 * control's rows, the sort and the group headings can never mean different things by the same
 * word. **Waiting on** is a sixth banding with a vocabulary of its own (`sections.ts`), and
 * **None** is no banding at all.
 *
 * Snoozed asks are the exception every grouping shares: a row the reader deferred folds into
 * **Later**, last, whatever the list is grouped by, because deferring it is them saying it is
 * not for now.
 */

import { compareTimestamps } from "../../lib/timestamps";
import type { InboxDimension, InboxFilterField, InboxGroupBy, InboxSort } from "../refs/routes";
import { NO_FILTER_VALUE } from "../refs/routes";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "./ask-urgency";
import {
  INBOX_ROW_TYPES,
  type InboxListRow,
  rowAssignee,
  rowId,
  rowLastActivityAt,
  rowPriority,
  rowProject,
  rowType,
  rowUrgency,
} from "./list-row";
import { INBOX_SECTIONS, type InboxSection, sectionOf, UNASSIGNED_FILTER_VALUE } from "./sections";
import { isSnoozed } from "./snooze";

/** One band of the list: the rows under one heading, in the list's own order. */
export interface InboxGroup {
  /** What the band is, under this grouping: the fold preference records exactly this word, so
   *  a band stays folded across a reload and each grouping remembers its own. */
  key: string;
  /** The heading, or undefined for the one band that has none: the whole list under **None**. */
  label: string | undefined;
  rows: readonly InboxListRow[];
}

/** What the grouping needs beyond the rows: the clock a snooze is judged against, the Waiting
 *  on titles this list is showing (`sectionTitles`), and who is reading. */
export interface GroupingContext {
  now: number;
  titles: Record<InboxSection, string>;
  viewer: string;
}

/** The band every snoozed ask folds into, under every grouping. */
export const LATER_GROUP_KEY = "later";

/** The one band folded until the reader opens it. Their own fold, kept in the browser per
 *  grouping, replaces this from the moment they touch one. */
export const DEFAULT_FOLDED_GROUPS: readonly string[] = [LATER_GROUP_KEY];

/** The band holding every row under **None**, which draws no heading over them. */
export const ALL_GROUP_KEY = "all";

/**
 * What one dimension says about one row, in the vocabulary the address and the Filter control
 * both write: `P0`-`P3`, an urgency word, a row type, a project key, a login - or `none` where
 * the field says nothing about the row. Assignee answers `unassigned` instead, the word its own
 * repeated-parameter vocabulary reserves.
 */
export function dimensionValue(
  row: InboxListRow,
  dimension: InboxDimension,
  viewer: string
): string {
  if (dimension === "priority") {
    const priority = rowPriority(row);
    return priority === null ? NO_FILTER_VALUE : `P${priority}`;
  }
  if (dimension === "urgency") {
    return rowUrgency(row) ?? NO_FILTER_VALUE;
  }
  if (dimension === "type") {
    return rowType(row);
  }
  if (dimension === "project") {
    return rowProject(row) ?? NO_FILTER_VALUE;
  }
  return rowAssignee(row, viewer) ?? UNASSIGNED_FILTER_VALUE;
}

/** The plural name of each row type, as a Type band is headed. */
const TYPE_GROUP_LABELS: Record<(typeof INBOX_ROW_TYPES)[number], string> = {
  agent: "Agent messages",
  approval: "Approvals",
  decision: "Decisions",
  question: "Questions",
  reply: "Replies",
};

/** The singular name of each row type, as the Filter control and a row's own icon name it. */
export const TYPE_LABELS: Record<(typeof INBOX_ROW_TYPES)[number], string> = {
  agent: "Agent message",
  approval: "Approval",
  decision: "Decision",
  question: "Question",
  reply: "Reply",
};

/** What a value of one dimension is called where the reader reads it: a Filter row, a filter
 *  token, and (in its plural) a group heading. */
export function dimensionValueLabel(dimension: InboxDimension, value: string): string {
  if (value === NO_FILTER_VALUE) {
    if (dimension === "priority") return "No priority";
    if (dimension === "urgency") return "No urgency";
    return "No project";
  }
  if (value === UNASSIGNED_FILTER_VALUE) return "Unassigned";
  if (dimension === "urgency") return URGENCY_LABELS[value as keyof typeof URGENCY_LABELS] ?? value;
  if (dimension === "type") return TYPE_LABELS[value as keyof typeof TYPE_LABELS] ?? value;
  return value;
}

/** What each dimension calls its field, in a trigger, a token and a menu row. */
export const DIMENSION_LABELS: Record<InboxDimension, string> = {
  assignee: "Assignee",
  priority: "Priority",
  project: "Project",
  type: "Type",
  urgency: "Urgency",
};

/** Where a value sits among its own field's, lowest first; `none` is last everywhere, and a
 *  value with no fixed order (a project key, a login) falls back to its own text. */
function valueRank(dimension: InboxDimension, value: string): number {
  if (value === NO_FILTER_VALUE || value === UNASSIGNED_FILTER_VALUE)
    return Number.MAX_SAFE_INTEGER;
  if (dimension === "priority") return Number(value.slice(1));
  if (dimension === "urgency") {
    // The bands read most urgent first, which is `ASK_URGENCIES_ASCENDING` read backwards; an
    // urgency this build does not know keeps the -1 it has always ranked at.
    const ascending = (ASK_URGENCIES_ASCENDING as readonly string[]).indexOf(value);
    return ascending < 0 ? -1 : ASK_URGENCIES_ASCENDING.length - 1 - ascending;
  }
  if (dimension === "type") return (INBOX_ROW_TYPES as readonly string[]).indexOf(value);
  return 0;
}

/** Two values of one field, in the order their bands and their rows are shown: the field's own
 *  order where it has one, else the text's, with `none` last either way. */
function compareValues(dimension: InboxDimension, left: string, right: string): number {
  const ranked = valueRank(dimension, left) - valueRank(dimension, right);
  if (ranked !== 0) return ranked;
  return left === right ? 0 : left.localeCompare(right);
}

/**
 * The rows in the order the reader chose. `newest` is the last write in each row; every other
 * sort is that field, with `none` last, and rows that tie on it newest first. The row's id
 * breaks a tie that reaches it, so the order never depends on how the two reads happened to
 * arrive.
 */
export function sortRows(
  rows: readonly InboxListRow[],
  sort: InboxSort,
  viewer: string
): InboxListRow[] {
  return [...rows].sort((left, right) => {
    if (sort !== "newest") {
      const byField = compareValues(
        sort,
        dimensionValue(left, sort, viewer),
        dimensionValue(right, sort, viewer)
      );
      if (byField !== 0) return byField;
    }
    const byTime = compareTimestamps(rowLastActivityAt(right), rowLastActivityAt(left));
    return byTime !== 0 ? byTime : rowId(left).localeCompare(rowId(right));
  });
}

/** Whether `row` is a snoozed ask, the one row every grouping puts in **Later** instead. */
function deferred(row: InboxListRow, now: number): boolean {
  return row.kind === "ask" && isSnoozed(row.ask, now);
}

/** What a band's heading reads: the **Waiting on** bands have titles of their own, the Type
 *  bands their own words, and every other field labels its own value. **None** has no heading
 *  at all - there is one band and it holds the whole list. */
function bandLabel(
  by: InboxGroupBy,
  key: string,
  titles: Record<InboxSection, string>
): string | undefined {
  if (by === "none") return undefined;
  if (by === "waiting-on") return titles[key as InboxSection] ?? key;
  if (by === "type") return TYPE_GROUP_LABELS[key as keyof typeof TYPE_GROUP_LABELS];
  return dimensionValueLabel(by, key);
}

/**
 * The rows banded by `by`, in the order they were given, with every snoozed ask in **Later** at
 * the end. Bands are ordered by their own field - P0 first, blocking first, question to agent
 * message, a project by key, a person by login, the turn bands top to bottom - and a band is
 * drawn only when it holds a row.
 */
export function group(
  rows: readonly InboxListRow[],
  by: InboxGroupBy,
  { now, titles, viewer }: GroupingContext
): InboxGroup[] {
  const later: InboxListRow[] = [];
  const bands = new Map<string, InboxListRow[]>();
  const band = (key: string): InboxListRow[] => {
    const existing = bands.get(key);
    if (existing !== undefined) return existing;
    const made: InboxListRow[] = [];
    bands.set(key, made);
    return made;
  };
  for (const row of rows) {
    if (deferred(row, now)) {
      later.push(row);
      continue;
    }
    if (by === "none") {
      band(ALL_GROUP_KEY).push(row);
    } else if (by === "waiting-on") {
      band(sectionOf(row, now) ?? NO_FILTER_VALUE).push(row);
    } else {
      band(dimensionValue(row, by, viewer)).push(row);
    }
  }
  const keys = [...bands.keys()];
  if (by === "waiting-on") {
    keys.sort(
      (left, right) =>
        (INBOX_SECTIONS as readonly string[]).indexOf(left) -
        (INBOX_SECTIONS as readonly string[]).indexOf(right)
    );
  } else if (by !== "none") {
    keys.sort((left, right) => compareValues(by, left, right));
  }
  const groups: InboxGroup[] = keys.map((key) => ({
    key,
    label: bandLabel(by, key, titles),
    rows: bands.get(key) ?? [],
  }));
  if (later.length > 0) {
    groups.push({ key: LATER_GROUP_KEY, label: titles.later, rows: later });
  }
  return groups;
}

/**
 * How many rows each value of `field` would leave, counted under every OTHER field's chosen
 * values: a reader opening Priority sees what each priority holds inside the list they have
 * already narrowed, so ticking one of them never answers zero.
 */
export function filterCounts(
  rows: readonly InboxListRow[],
  field: InboxDimension,
  matchesOtherFields: (row: InboxListRow) => boolean,
  viewer: string
): ReadonlyMap<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    if (!matchesOtherFields(row)) continue;
    const value = dimensionValue(row, field, viewer);
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return counts;
}

/** Whether a row survives one filter field's chosen values; nothing chosen keeps every row. */
export function inFieldFilter(
  row: InboxListRow,
  field: InboxFilterField,
  chosen: readonly string[] | undefined,
  viewer: string
): boolean {
  return (
    chosen === undefined ||
    chosen.length === 0 ||
    chosen.includes(dimensionValue(row, field, viewer))
  );
}
