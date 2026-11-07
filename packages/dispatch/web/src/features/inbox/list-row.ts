/**
 * What a row of the Inbox is, now that the list holds two kinds of them.
 *
 * Every Inbox helper used to be typed on `InboxRow`, the open ask the server lists. The list
 * also holds the reader's own threads - a reply somebody wrote where they wrote, and an agent's
 * answer to a message they sent - which are `MyReplyRow`s from another route entirely. The two
 * share nothing but the fields the list sorts, groups, filters and draws by, so those fields are
 * read through the accessors below and nowhere else: the bar's counts, the grouping, the sort,
 * the selection and the row itself all ask the same seven questions of whichever row they hold.
 */

import { inboxThreadRow } from "@legion/contracts";

import type {
  AskUrgency,
  InboxRow,
  IssuePriority,
  MyReplyRow,
  OpenAskOwner,
} from "../../api/types";
import { issuePriority } from "../issue/issue-priority";
import type { DispatchReferenceRoute } from "../refs/routes";
import { isDecisionAsk } from "./ask-decision";
import { askUrgency } from "./ask-urgency";

/** One row of the Inbox list: an open ask, or a thread of the reader's own. */
export type InboxListRow = { kind: "ask"; ask: InboxRow } | { kind: "thread"; thread: MyReplyRow };

/** What kind of row this is, as the Type filter, the Type grouping and the row's own icon name
 *  it: the three shapes an ask comes in, and the two a thread does. */
export const INBOX_ROW_TYPES = ["question", "decision", "approval", "reply", "agent"] as const;
export type InboxRowType = (typeof INBOX_ROW_TYPES)[number];

/** The row's identity, and exactly what `?item=` carries: an ask by its own id, a thread by its
 *  kind and its thread id, so an ask and a thread can never collide in the address. */
export function rowId(row: InboxListRow): string {
  if (row.kind === "ask") return row.ask.id;
  return `${rowType(row)}:${row.thread.thread_id}`;
}

/** When the row last had something written in it: the thread's own reply, or the ask's newest
 *  reply, falling back to the asking itself where nobody has replied. The sort's tiebreak and
 *  `newest` itself read this. */
export function rowLastActivityAt(row: InboxListRow): string {
  if (row.kind === "thread") return row.thread.reply.created_at;
  return row.ask.last_reply?.created_at ?? row.ask.created_at;
}

/** The owning issue's coarse priority, or null where there is none to show: a thread with no
 *  issue, an issue with no priority set, and every Agent row. */
export function rowPriority(row: InboxListRow): IssuePriority | null {
  if (row.kind === "ask") return issuePriority(row.ask.priority);
  const thread = inboxThreadRow(row.thread);
  return thread.kind === "agent" ? null : issuePriority(thread.priority);
}

/** The ask's urgency. A thread has none at all - urgency is a property of a question - so the
 *  Urgency filter and grouping put every thread row under `none`. */
export function rowUrgency(row: InboxListRow): AskUrgency | null {
  return row.kind === "ask" ? askUrgency(row.ask.urgency) : null;
}

/** Which project the row belongs to, or null for a row that belongs to none: the thread's own
 *  project (an Agent row has none), and for an ask the project of its issue or of the document
 *  it was asked on. A project key holds no hyphen, so the part of an issue key before the first
 *  one is its project: `CORE-12` belongs to `CORE`. */
export function rowProject(row: InboxListRow): string | null {
  if (row.kind === "thread") {
    const thread = inboxThreadRow(row.thread);
    return thread.kind === "agent" ? null : (thread.project ?? null);
  }
  const issue = row.ask.issue?.key ?? row.ask.issue_key;
  if (issue !== null && issue !== undefined) return issue.split("-")[0] ?? issue;
  return row.ask.document?.project ?? null;
}

/**
 * Who holds the row. An ask's holder is its issue's assignee, and a document ask has none (a
 * document has no assignee). A thread is the reader's own - the route answers nobody else's -
 * so it is held by whoever is reading, which is why the viewer is an argument here rather than
 * something this module could look up: the Inbox has it already, and a helper that read it
 * again would be a second answer to the same question.
 */
export function rowAssignee(row: InboxListRow, viewer: string): string | null {
  return row.kind === "ask" ? (row.ask.issue?.assignee ?? null) : viewer;
}

/** What kind of row this is. An ask is a decision when it was written as a document's typed
 *  block, an approval when it is a document review, and a question otherwise - today's
 *  `askKind` rule, which named the row's badge before it named its icon. */
export function rowType(row: InboxListRow): InboxRowType {
  if (row.kind === "thread") return row.thread.kind === "agent" ? "agent" : "reply";
  if (isDecisionAsk(row.ask)) return "decision";
  return row.ask.kind === "approval" ? "approval" : "question";
}

/** The issue or project document a thread belongs to, or null for an Agent row, which belongs
 *  to neither: its thread is a message the reader sent a session. */
export function threadOwner(thread: MyReplyRow): OpenAskOwner | null {
  if (thread.kind === "agent") return null;
  if (thread.issue !== undefined) return { issue: thread.issue };
  return thread.document === undefined ? null : { document: thread.document };
}

/**
 * The `dispatch://` reference a thread row copies: the thread's own root, under its issue when
 * it has one and under its project document otherwise. An Agent row has none - its thread is a
 * message the reader sent a session, which no reference names - so it draws no copy control and
 * `y` does nothing on it. A message thread on a project document has none either: only asks and
 * comments are referenced under a document.
 */
export function threadReferenceRoute(row: MyReplyRow): DispatchReferenceRoute | undefined {
  const thread = inboxThreadRow(row);
  if (thread.kind === "agent") return undefined;
  const key = thread.issue?.key;
  if (key !== undefined) return { id: thread.thread_id, key, kind: thread.kind };
  const document = thread.document;
  if (document === undefined || thread.kind === "message") return undefined;
  return {
    item: { id: thread.thread_id, kind: thread.kind },
    kind: "document",
    project: document.project,
    slug: document.slug,
  };
}
