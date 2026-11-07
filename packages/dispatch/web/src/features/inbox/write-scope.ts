/**
 * The narrowing the Inbox's two list writes carry - **Mark all read** (`mark-all.ts`) and
 * **Dismiss** (`useInboxDismiss.ts`) - and the page's own copy of the rule that says which thread
 * row one of them takes.
 *
 * Both routes read the same four fields, each absent list meaning every value, and both act on
 * exactly the threads `GET /api/v1/me/replies` answers under them. So the body's fields are built
 * here once and that rule is read here once: a value the list spells one way is written by that
 * same spelling, and the rows the page takes off the screen are the rows the request takes.
 */

import { INBOX_THREAD_KINDS, type InboxThreadKind } from "@legion/contracts";

import type { MarkInboxReadPriority, MarkInboxReadRequest, MyReplyRow } from "../../api/types";
import { INBOX_PRIORITY_VALUES, INBOX_URGENCY_VALUES } from "../refs/routes";
import { inFieldFilter } from "./grouping";
import type { InboxFilters } from "./InboxBar";
import { type InboxListRow, rowType } from "./list-row";

/** The four narrowing fields both bodies carry; the assignee filter is not among them, because a
 *  thread row is the reader's own and nobody else's, so a selection naming anyone else leaves the
 *  list with no thread row to write. */
export type InboxWriteScope = Pick<
  MarkInboxReadRequest,
  "kind" | "priority" | "project" | "urgency"
>;

/** The viewer `inFieldFilter` takes for the assignee field, which is not one of the fields read
 *  below: a thread row is the reader's own, so no write ever narrows by holder. */
const NO_VIEWER = "";

/** The chosen values of one field that this route's vocabulary holds. The address is parsed
 *  against that same vocabulary (`parseInboxSearch`), so this drops nothing a reader can pick;
 *  it is how the body is typed without a cast. */
function chosen<T extends string>(
  values: readonly string[] | undefined,
  vocabulary: readonly T[]
): T[] | undefined {
  if (values === undefined || values.length === 0) return undefined;
  const kept = values.filter((value): value is T =>
    (vocabulary as readonly string[]).includes(value)
  );
  return kept.length === 0 ? [] : kept;
}

/** The kinds of thread row the Type filter is showing: both while it shows every type, since the
 *  button sits over one list of both. A Type filter naming only ask types shows no thread row at
 *  all, and neither write is offered then. */
export function scopeKinds(filters: InboxFilters): InboxThreadKind[] {
  const type = filters.type;
  if (type === undefined || type.length === 0) return [...INBOX_THREAD_KINDS];
  return INBOX_THREAD_KINDS.filter((kind) => type.includes(kind));
}

/** The list's own narrowing as a body carries it: the kinds the Type filter shows, and the
 *  priorities, urgencies and projects the other three show. A field the reader has not narrowed
 *  is left out, which the route reads as every value. */
export function inboxWriteScope(filters: InboxFilters): InboxWriteScope {
  const priority = chosen<MarkInboxReadPriority>(filters.priority, INBOX_PRIORITY_VALUES);
  const urgency = chosen(filters.urgency, INBOX_URGENCY_VALUES);
  const project = filters.project;
  return {
    kind: scopeKinds(filters),
    ...(priority === undefined ? {} : { priority }),
    ...(urgency === undefined ? {} : { urgency }),
    ...(project === undefined || project.length === 0 ? {} : { project: [...project] }),
  };
}

/**
 * Whether `scope` covers `thread`: the server's own rule (`inboxReadScope`), read here so the
 * page takes off exactly the rows the request takes. An Agent row, which carries no issue, no
 * project, no priority and no urgency, is covered only while each of those filters admits a row
 * that has none. Priority, urgency and project are read through the Filter's own rule
 * (`inFieldFilter`), so a value the list spells one way is written by that same spelling.
 */
export function inWriteScope(thread: MyReplyRow, scope: InboxWriteScope): boolean {
  const row: InboxListRow = { kind: "thread", thread };
  // An absent `kind` is every kind, the way the server reads it (`parseInboxReadScope`), so a
  // filter the reader has not touched is left out of the body rather than spelled out.
  const kinds: readonly string[] = scope.kind ?? INBOX_THREAD_KINDS;
  if (!kinds.includes(rowType(row))) return false;
  return (
    inFieldFilter(row, "priority", scope.priority, NO_VIEWER) &&
    inFieldFilter(row, "urgency", scope.urgency, NO_VIEWER) &&
    inFieldFilter(row, "project", scope.project, NO_VIEWER)
  );
}
