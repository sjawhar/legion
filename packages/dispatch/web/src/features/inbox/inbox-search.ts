/**
 * What a word typed in the Inbox's search box matches, in one place: the list, the counts and
 * anything else that narrows by that text read the same rule, so a row can never be in one of
 * them and out of another. Both kinds of row the list holds go through it - an ask and a thread
 * - each against the fields its own shape carries.
 */

import type { InboxRow, MyReplyRow } from "../../api/types";
import { askIssueKey } from "./ask-name";
import type { InboxListRow } from "./list-row";

/**
 * The text an ask row's words match against: its issue key (which carries the project key), the
 * issue's title, the document's name where the row stands on a document alone, the question, and
 * the author. Joined and lowercased once per row, since every word of the query then tests the
 * one string.
 *
 * The question is where an approval request's generated prefix lives, beside the requester's own
 * message, so the words of that prefix match an approval row like any other word.
 *
 * The author is matched as the ask itself carries it - the session id, the title its origin
 * recorded, the owner - never the agent label a card resolves from a later registry read. A
 * registry read lands at its own time and only where a card draws one, so matching on it would
 * make one row match a word at one width and not at another.
 */
function inboxSearchHaystack(ask: InboxRow): string {
  const issueKey = askIssueKey(ask);
  const parts: (string | null | undefined)[] = [issueKey, ask.issue?.title];
  // The requirement is the name of the document a row stands on *where it has no issue*, which
  // is how `askOwner` (`ask-name.ts`) draws the row's owner too. The server sets `document` on
  // issue-owned rows as well (`packages/envoy/internal/dispatch/api/inbox.go` fills it whenever
  // the ask has a document at all), so without this guard typing `spec` would match every
  // approval request in the list.
  if (issueKey === null) {
    parts.push(ask.document?.name);
  }
  parts.push(ask.question, ask.author.id);
  if (ask.author.kind === "session") {
    parts.push(ask.author.origin?.session_title, ask.author.owner);
  }
  return parts.filter(Boolean).join(" ").toLowerCase();
}

/**
 * The text a thread row's words match against. A thread is not an `InboxRow`, so the same fields
 * are read off its own shape: the issue it hangs under, by key and title; the document's name
 * where it stands on a document alone, as an ask's haystack takes it only where the row has no
 * issue; its subject, which is what a thread says of itself; and whoever wrote in it last, as
 * the ask rule takes an author - the id, and a session's stamped title and owner beside it.
 *
 * An Agent row belongs to no issue and no document, and the session it was sent to is what names
 * it, so its id and the name Dispatch knows it by stand where an issue's words would.
 */
function threadSearchHaystack(thread: MyReplyRow): string {
  const parts: (string | null | undefined)[] = [];
  if (thread.kind === "agent") {
    parts.push(thread.session.id, thread.session.name);
  } else {
    parts.push(thread.issue?.key, thread.issue?.title);
    if (thread.issue === undefined) parts.push(thread.document?.name);
  }
  const author = thread.reply.author;
  parts.push(thread.subject, author.id);
  if (author.kind === "session") {
    parts.push(author.origin?.session_title, author.owner);
  }
  return parts.filter(Boolean).join(" ").toLowerCase();
}

/** Whether a row matches every word of the query; an empty query matches every row. One rule for
 *  both kinds of row the Inbox lists, so a word can never keep an ask and drop a thread for a
 *  reason neither of them states. `words` is `searchWords(query)`, split once by the caller
 *  rather than once per row. */
export function matchesInboxSearch(row: InboxListRow, words: readonly string[]): boolean {
  if (words.length === 0) return true;
  const haystack =
    row.kind === "ask" ? inboxSearchHaystack(row.ask) : threadSearchHaystack(row.thread);
  return words.every((word) => haystack.includes(word));
}
