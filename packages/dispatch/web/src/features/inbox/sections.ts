import type { AskTurn } from "../../api/types";
import type { InboxListRow } from "./list-row";
import { isSnoozed } from "./snooze";

/** Where a row sits: whose turn it is, or the Later band the reader deferred it into. */
export type InboxSection = AskTurn | "later";

/**
 * The **Waiting on** grouping's bands, top to bottom, and `sectionOf` below is its whole rule.
 * It is one grouping among seven now (`grouping.ts`), not the list's only banding, and the
 * order WITHIN a band is the list's own sort (`sortRows`), not the server's: the SPA re-sorts
 * every read it draws, so `listInbox`'s `order by`
 * (`packages/envoy/internal/dispatch/api/inbox.go`) decides only what a tie falls back to.
 */
export const INBOX_SECTIONS = ["human", "agent", "later"] as const;

/** The bands as the reader's own list names them; `sectionTitles` below picks the record a
 *  filtered list needs. */
const SECTION_TITLES: Record<InboxSection, string> = {
  agent: "Waiting on agents",
  human: "Waiting on you",
  later: "Later",
};

/** The viewer's own rows: asks on issues assigned to their lowercase login, and every thread
 *  row, which the route answers for the reader alone. */
export function isMine(row: InboxListRow, viewer: string): boolean {
  return row.kind === "thread" || row.ask.issue?.assignee === viewer;
}

/** Rows nobody holds: asks on unassigned issues, and every document ask (a document has no
 *  assignee). Also what decides whether a row offers "Assign to me". A thread row is the
 *  reader's own, so nobody is never its holder. */
export function isUnassigned(row: InboxListRow): boolean {
  return row.kind === "ask" && (row.ask.issue?.assignee ?? null) === null;
}

/** The two words the `?assignee=` vocabulary reserves beside a person's login: `unassigned`,
 *  which `inAssigneeFilter` below reads as "nobody holds this row", and `everyone`, which the
 *  route reads as the cleared filter and no selection ever holds (`parseInboxSearch` turns it
 *  into the empty selection). Both live here so the route, the filter control and the list's
 *  own rule cannot drift, and so the control can keep either word out of the people it offers. */
export const UNASSIGNED_FILTER_VALUE = "unassigned";
export const EVERYONE_FILTER_VALUE = "everyone";

/**
 * Whether a row survives the assignee filter, the one place that decides what a filtered list
 * holds. An empty selection is the cleared filter and keeps everything; otherwise an ask is
 * kept when its issue's assignee is one of the selected people, or when nobody holds it
 * (`isUnassigned`, every document ask included) and `unassigned` is selected.
 *
 * A thread row is the reader's own and nobody else's, so it survives a selection naming them
 * and no other: `unassigned` names nobody, so it drops the threads too.
 */
export function inAssigneeFilter(
  row: InboxListRow,
  selection: readonly string[],
  viewer: string
): boolean {
  if (selection.length === 0) return true;
  if (row.kind === "thread") return selection.includes(viewer);
  const assignee = row.ask.issue?.assignee ?? null;
  if (assignee === null) return selection.includes(UNASSIGNED_FILTER_VALUE);
  return selection.includes(assignee);
}

/** Whether the list is the reader's own: the one selection whose bands, empty state and
 *  trigger speak in the second person. Every other selection - cleared, anybody else,
 *  `unassigned` included - is about somebody, so it is spoken of in the third. */
export function isOwnList(selection: readonly string[], viewer: string): boolean {
  return selection.length === 1 && selection[0] === viewer;
}

/**
 * The band titles for a selection. With the Unassigned band gone, another person's question now
 * lands in a turn band for the first time, and "Waiting on you" would be false there: so only
 * the reader's own list keeps the second person, and every other selection - cleared, anybody
 * else, `unassigned` included - says "Waiting on a person".
 */
export function sectionTitles(
  selection: readonly string[],
  viewer: string
): Record<InboxSection, string> {
  if (isOwnList(selection, viewer)) return SECTION_TITLES;
  return { ...SECTION_TITLES, human: "Waiting on a person" };
}

/**
 * Which **Waiting on** band a row belongs to. A snooze the reader set wins over everything: the
 * whole point of deferring an ask is that the agent answering it does not pull it back onto the
 * list, so a row inside its snooze window is in Later whatever its turn, its assignee, or how
 * recently anyone replied. Every other ask sits under whose turn it is, whoever the filter
 * listed it for; an ask the inbox lists without a turn belongs to no band.
 *
 * A thread row is a write nobody but the reader can deal with, so it waits on them: `human`.
 * Nothing snoozes a thread, so none of them is ever in Later.
 */
export function sectionOf(row: InboxListRow, now: number): InboxSection | undefined {
  if (row.kind === "thread") return "human";
  if (isSnoozed(row.ask, now)) return "later";
  return row.ask.waiting_on;
}
