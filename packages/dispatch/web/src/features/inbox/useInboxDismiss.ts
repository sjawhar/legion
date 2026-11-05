/**
 * The Inbox's **Dismiss**, and the **Move to Inbox** that puts a thread back: what one press
 * sends, what it takes off the page before the server answers, and what the page says when the
 * server refuses it.
 *
 * A thread stays in the Inbox, read or not, until it is dismissed, as an email stays until it is
 * archived: reading it, and **Mark all read**, leave it where it is. **Dismiss** is the one thing
 * that takes it out, and in the Dismissed view the same write with `restore` puts it back.
 *
 * One press is one `POST /api/v1/me/inbox/dismiss` (`inboxDismissRequest`), naming either the row
 * ids it changes or - for **Select all**, where the ids of rows the reader has not loaded are not
 * known - the narrowing the list is under with `dismiss_through`, that list read's own `as_of`.
 * The press is applied here first, because the reader pressed it: the rows leave the view they
 * were in and the unread count every badge shares drops by the unread ones among them. A refused
 * press puts each row back where it stood and the page says so (`dismissRefusalText`), which an
 * old server with no such route is one way of causing.
 */

import { INBOX_DISMISS_ROW_PREFIXES } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { inboxDismissRequest } from "../../api/client";
import {
  dismissedThreadsQuery,
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesKey,
  userAgentStateQuery,
} from "../../api/queries";
import type {
  InboxDismissRequest,
  InboxDismissResponse,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { InboxFilters } from "./InboxBar";
import { type InboxListRow, rowId } from "./list-row";
import { inboxWriteScope, inWriteScope } from "./write-scope";

/** The id `POST /api/v1/me/inbox/dismiss` names `row` by, or undefined for a row it cannot name:
 *  an Ask row has no Dismiss at all, and naming `ask:<id>` is refused. A thread row's own `rowId`
 *  is already that id - a Reply row is `reply:<thread id>` and an Agent row
 *  `agent:<root message id>` - and this is the one place the list's two spellings are held to the
 *  contract's (`INBOX_DISMISS_ROW_PREFIXES`). */
export function dismissRowId(row: InboxListRow): string | undefined {
  if (row.kind !== "thread") return undefined;
  const id = rowId(row);
  const prefixes = Object.values(INBOX_DISMISS_ROW_PREFIXES);
  return prefixes.some((prefix) => id.startsWith(prefix)) ? id : undefined;
}

/** What one press changes: the rows the reader picked, or the whole group under the list's own
 *  narrowing, which is what **Select all** picks - the ids of rows behind **Show more** are not
 *  all loaded, so the request names the narrowing and the list's read mark instead. `restore`
 *  puts the threads back in the Inbox rather than taking them out. */
export type InboxDismissInput =
  | { readonly restore?: boolean; readonly rows: readonly string[] }
  | { readonly filters: InboxFilters; readonly restore?: boolean; readonly through: string };

/** The body one press sends. */
export function inboxDismissBody(input: InboxDismissInput): InboxDismissRequest {
  const restore = input.restore === true ? { restore: true } : {};
  if ("rows" in input) return { threads: [...input.rows], ...restore };
  return { ...inboxWriteScope(input.filters), dismiss_through: input.through, ...restore };
}

/**
 * Whether one press changes `thread`: the row ids it names, or - under the narrowing - the
 * server's own rule, which is every thread the list answers under it whose newest reply, or whose
 * dismissal, is at or before the mark (`inWriteScope`).
 */
export function dismissesThread(thread: MyReplyRow, body: InboxDismissRequest): boolean {
  const threads = body.threads;
  if (threads !== undefined) return threads.includes(rowId({ kind: "thread", thread }));
  const through = body.dismiss_through;
  if (through === undefined) return false;
  const at =
    body.restore === true
      ? (thread.dismissed_at ?? thread.reply.created_at)
      : thread.reply.created_at;
  if (compareTimestamps(at, through) > 0) return false;
  return inWriteScope(thread, body);
}

/** What the page says about a press the server refused: the rows are back, so it says what did
 *  not happen and the reason the server gave, as a refused bulk snooze does (`refusalText` in
 *  `Inbox.tsx`). */
export function dismissRefusalText(count: number, restore: boolean, reason: string): string {
  const threads = `${count} ${count === 1 ? "thread" : "threads"}`;
  const what = restore ? `move ${threads} back to the Inbox` : `dismiss ${threads}`;
  return `Could not ${what}: ${reason}.`;
}

/** One row a press took out of a cached read, and where it stood in it, so a refusal puts it back
 *  where the reader saw it rather than at the end of the list. */
interface DroppedRow {
  readonly at: number;
  readonly row: MyReplyRow;
}

/** `answer` without the rows the press takes, and what it took. */
function withoutRows(
  answer: MyRepliesResponse,
  body: InboxDismissRequest
): { answer: MyRepliesResponse; dropped: readonly DroppedRow[] } {
  const dropped: DroppedRow[] = [];
  const rows = answer.rows.filter((row, at) => {
    if (!dismissesThread(row, body)) return true;
    dropped.push({ at, row });
    return false;
  });
  if (dropped.length === 0) return { answer, dropped };
  return {
    answer: { ...answer, rows, total: Math.max(0, answer.total - dropped.length) },
    dropped,
  };
}

/** `answer` with the rows a refused press took put back where each stood. */
function withRows(answer: MyRepliesResponse, dropped: readonly DroppedRow[]): MyRepliesResponse {
  if (dropped.length === 0) return answer;
  const rows = [...answer.rows];
  // Oldest index first, so each row lands at the place it held before any of them left.
  for (const { at, row } of [...dropped].sort((left, right) => left.at - right.at)) {
    rows.splice(Math.min(at, rows.length), 0, row);
  }
  return { ...answer, rows, total: answer.total + dropped.length };
}

/** What a press rolls back to: the unread count as it stood, and the rows it took out of the view
 *  it was made in, with the place each held. Only one of the two views is written: a dismiss is
 *  made in the Inbox, a restore in the Dismissed view. */
interface DismissRollback {
  readonly count: number | undefined;
  /** What left the Inbox's own read. */
  readonly inbox: readonly DroppedRow[];
  /** What left each page of the Dismissed read, by page. */
  readonly dismissed: readonly (readonly DroppedRow[])[];
}

/** The pages of the Dismissed read, as the infinite query holds them. */
interface DismissedPages {
  readonly pageParams: readonly unknown[];
  readonly pages: readonly MyRepliesResponse[];
}

export interface InboxDismiss {
  /** The row ids whose own press is still out, so each control says what it is doing. Empty for
   *  a press made under the narrowing, which names no row. */
  readonly pending: ReadonlySet<string>;
  /** What the reader is told about the last refused press, until the next one starts. */
  readonly refusal: string | undefined;
  /** Dismisses, or restores, the rows one press names. */
  readonly submit: (input: InboxDismissInput) => void;
}

/**
 * The Inbox's one dismiss write, for one list: its own optimistic apply, its own rollback and its
 * own refusal. The Inbox owns it rather than each control, because the write takes the row - and
 * with it every control drawn on it - off the page, so a refusal owned by the row would be
 * unmounted before it arrived. The drawer mounts its own Inbox, so it gets its own.
 *
 * `onSettled` is told the rows one press named once Dispatch has decided it, either way: the
 * Inbox keeps those rows out of its hand-hold while the press is out, and a row the rollback put
 * back is an ordinary row again.
 */
export function useInboxDismiss({
  onSettled,
}: {
  readonly onSettled?: (ids: readonly string[]) => void;
} = {}): InboxDismiss {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<ReadonlySet<string>>(() => new Set());
  const [refusal, setRefusal] = useState<string | undefined>(undefined);
  const inboxKey = inboxThreadsQuery().queryKey;
  const dismissedKey = dismissedThreadsQuery().queryKey;
  const countKey = myRepliesCountQuery().queryKey;

  const live = (rows: readonly string[], out: boolean) => {
    setPending((current) => {
      const next = new Set(current);
      for (const id of rows) {
        if (out) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  };

  const write = useKeyedWrite<InboxDismissResponse, InboxDismissInput, DismissRollback>({
    inline: true,
    label: (input) => (input.restore === true ? "Threads moved to the Inbox" : "Threads dismissed"),
    onError: (error, input, rollback) => {
      const taken = "rows" in input ? input.rows.length : (rollback?.inbox.length ?? 0);
      setRefusal(dismissRefusalText(taken, input.restore === true, error.message));
      if (rollback === undefined) return;
      queryClient.setQueryData<MyRepliesResponse>(inboxKey, (current) =>
        current === undefined ? current : withRows(current, rollback.inbox)
      );
      queryClient.setQueryData<DismissedPages>(dismissedKey, (current) =>
        current === undefined
          ? current
          : {
              ...current,
              pages: current.pages.map((page, index) =>
                withRows(page, rollback.dismissed[index] ?? [])
              ),
            }
      );
      if (rollback.count !== undefined) queryClient.setQueryData(countKey, rollback.count);
    },
    onMutate: (input) => {
      setRefusal(undefined);
      const body = inboxDismissBody(input);
      const restoring = input.restore === true;
      // A dismiss takes its rows out of the Inbox's own read; a restore takes them out of the
      // Dismissed read's pages. The other view is read again when the answer lands, so a press
      // never has to guess what the row looks like on the far side of it.
      let inbox: readonly DroppedRow[] = [];
      let dismissed: readonly (readonly DroppedRow[])[] = [];
      if (restoring) {
        const pages = queryClient.getQueryData<DismissedPages>(dismissedKey);
        if (pages !== undefined) {
          const applied = pages.pages.map((page) => withoutRows(page, body));
          dismissed = applied.map(({ dropped }) => dropped);
          queryClient.setQueryData<DismissedPages>(dismissedKey, {
            ...pages,
            pages: applied.map(({ answer }) => answer),
          });
        }
      } else {
        const answer = queryClient.getQueryData<MyRepliesResponse>(inboxKey);
        if (answer !== undefined) {
          const applied = withoutRows(answer, body);
          inbox = applied.dropped;
          queryClient.setQueryData(inboxKey, applied.answer);
        }
      }
      // The count every badge shares drops by the unread rows this press takes out, clamped at
      // zero. A restored thread was marked read by the dismiss that took it out, so putting it
      // back counts for nothing.
      const count = queryClient.getQueryData<number>(countKey);
      const unread = inbox.filter(({ row }) => row.unread).length;
      if (count !== undefined && unread > 0) {
        queryClient.setQueryData(countKey, Math.max(0, count - unread));
      }
      return { count: unread > 0 ? count : undefined, dismissed, inbox };
    },
    onSuccess: () => {
      // What the server changed is read again from the server: both views, the counts, and the
      // per-session agent state the Agents page's **New replies** badge counts, since a dismiss
      // marks its threads read as well.
      void queryClient.invalidateQueries({ queryKey: myRepliesKey });
      void queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey });
    },
    onSettled: (_data, _error, input, context) => {
      if ("rows" in input) {
        live(input.rows, false);
        onSettled?.(input.rows);
        return;
      }
      // A press under the narrowing names no row, so the rows it settles are the ones its own
      // optimistic apply took out of the view.
      onSettled?.(
        [...(context?.inbox ?? []), ...(context?.dismissed ?? []).flat()].map(({ row }) =>
          rowId({ kind: "thread", thread: row })
        )
      );
    },
    request: (input) => inboxDismissRequest(inboxDismissBody(input)),
    target: () => "inbox:dismiss",
  });

  return {
    pending,
    refusal,
    submit: (input) => {
      if ("rows" in input) {
        if (input.rows.length === 0) return;
        live(input.rows, true);
      }
      write.mutate(input);
    },
  };
}
