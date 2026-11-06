import { expect, test } from "bun:test";

import type { InboxRow } from "../../api/types";
import { searchWords } from "../agents/agent-search";
import { issueAsk } from "./inbox-fixture";
import { matchesInboxSearch } from "./inbox-search";

/** Whether the row matches the query a reader typed, split the way the box splits it. */
function matches(ask: InboxRow, query: string): boolean {
  return matchesInboxSearch(ask, searchWords(query));
}

const sessionAuthor = {
  id: "session-7",
  kind: "session",
  origin: { session_title: "Migration runner" },
  owner: "ana@example.com",
} as const;

test("an empty query matches every row", () => {
  expect(matches(issueAsk(), "")).toBe(true);
  expect(matches(issueAsk(), "   ")).toBe(true);
});

test("every word must match, in any order and in any case", () => {
  const ask = issueAsk({ issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" } });
  expect(matches(ask, "fix thing")).toBe(true);
  expect(matches(ask, "thing fix")).toBe(true);
  expect(matches(ask, "FIX The Thing")).toBe(true);
  expect(matches(ask, "fix migration")).toBe(false);
});

test("the issue key, its title, the question and the author are each matched", () => {
  const ask = issueAsk({
    author: sessionAuthor,
    issue: { assignee: "alice", key: "CORE-12", title: "Ship the migration" },
    issue_key: "CORE-12",
    question: "Approve the plan for CORE-12?",
  });
  // The key carries the project key, so typing either finds the row.
  expect(matches(ask, "core-12")).toBe(true);
  expect(matches(ask, "core")).toBe(true);
  expect(matches(ask, "ship migration")).toBe(true);
  expect(matches(ask, "approve plan")).toBe(true);
  expect(matches(ask, "session-7")).toBe(true);
  expect(matches(ask, "migration runner")).toBe(true);
  expect(matches(ask, "ana@example.com")).toBe(true);
});

// An approval request's question holds the generated prefix as well as the requester's message,
// so the prefix's own words narrow the list like any other word.
test("an approval request matches the words of its generated question", () => {
  const ask = issueAsk({
    kind: "approval",
    question: "Review requested on rollout-plan: please check the cutover order",
  });
  expect(matches(ask, "review requested")).toBe(true);
  expect(matches(ask, "cutover order")).toBe(true);
});

test("a document's name matches only where the row has no issue", () => {
  const document = { name: "Rollout plan", project: "CORE", slug: "rollout" };
  const documentAsk = issueAsk({ document, issue: undefined, issue_key: null });
  expect(matches(documentAsk, "rollout plan")).toBe(true);

  // The server sets `document` on issue-owned rows too, so a row with an issue must not match
  // its document's name: otherwise one word would pull in every approval request in the list.
  const onIssue = issueAsk({ document });
  expect(matches(onIssue, "rollout plan")).toBe(false);
  expect(matches(onIssue, "core-1")).toBe(true);
});

test("an agent title the ask does not carry never matches, whatever a registry knows", () => {
  // The card resolves "Migration runner" from a later registry read at some widths only; the
  // match reads the ask alone, so the same row answers the same words everywhere.
  const ask = issueAsk({ author: { id: "session-7", kind: "session" } });
  expect(matches(ask, "session-7")).toBe(true);
  expect(matches(ask, "migration runner")).toBe(false);
});
