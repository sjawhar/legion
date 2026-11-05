/**
 * What a word typed in the Inbox's search box matches, in one place: the list, the counts and
 * anything else that narrows by that text read the same rule, so a row can never be in one of
 * them and out of another.
 */

import type { InboxRow } from "../../api/types";
import { askIssueKey } from "./ask-name";

/**
 * The text one row's words match against: its issue key (which carries the project key), the
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
export function inboxSearchHaystack(ask: InboxRow): string {
  const issueKey = askIssueKey(ask);
  const parts: (string | undefined)[] = [issueKey ?? undefined, ask.issue?.title];
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

/** Whether a row matches every word of the query; an empty query matches every row. `words` is
 *  `searchWords(query)`, split once by the caller rather than once per row. */
export function matchesInboxSearch(ask: InboxRow, words: readonly string[]): boolean {
  if (words.length === 0) return true;
  const haystack = inboxSearchHaystack(ask);
  return words.every((word) => haystack.includes(word));
}
