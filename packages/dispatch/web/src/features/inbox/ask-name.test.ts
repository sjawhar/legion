import { expect, test } from "bun:test";

import { askIssueKey, askOrdinals, askOwner, controlName, type NamedAsk } from "./ask-name";

/** A row as the Inbox read carries it: the issue object the row renders, plus its flat key. */
function onIssue(key: string, rest: Partial<NamedAsk> = {}): NamedAsk {
  return {
    id: "a",
    issue: { assignee: "alice", key, title: "Ship the migration" },
    issue_key: key,
    question: "Which release?",
    ...rest,
  };
}

const issueAsk = onIssue("CORE-12");
const documentAsk: NamedAsk = {
  document: { name: "Rollout plan", project: "CORE", slug: "rollout" },
  id: "b",
  issue_key: null,
  question: "Sign this off?",
};

test("an ask belongs to its issue, its document, or neither", () => {
  expect(askOwner(issueAsk)).toEqual({ key: "issue:CORE-12", label: "CORE-12" });
  expect(askOwner({ id: "c", issue_key: "CORE-3", question: "?" })).toEqual({
    key: "issue:CORE-3",
    label: "CORE-3",
  });
  expect(askOwner(documentAsk)).toEqual({
    key: "document:CORE/rollout",
    label: "Rollout plan",
  });
  expect(askOwner({ id: "d", issue_key: null, question: "?" })).toEqual({
    key: "none",
    label: "Document ask",
  });
  expect(askIssueKey(documentAsk)).toBe(null);
});

test("two documents of one project are two owners", () => {
  const other: NamedAsk = {
    ...documentAsk,
    document: { name: "Cutover", project: "CORE", slug: "cutover" },
    id: "e",
  };
  expect(askOwner(other).key).not.toBe(askOwner(documentAsk).key);
});

test("an ordinal is given only where an owner has more than one row, in list order", () => {
  const first = onIssue("CORE-12", { id: "first" });
  const second = onIssue("CORE-12", { id: "second", question: "Ship it?" });
  const alone = onIssue("CORE-99", { id: "alone", question: "Alone?" });
  const ordinals = askOrdinals([first, alone, second]);

  expect(ordinals.get("first")).toEqual({ index: 1, total: 2 });
  expect(ordinals.get("second")).toEqual({ index: 2, total: 2 });
  expect(ordinals.get("alone")).toBeUndefined();
});

test("a name is the owner and the start of the question", () => {
  expect(controlName(issueAsk, undefined)).toBe("CORE-12: Which release?");
  expect(controlName(documentAsk, undefined)).toBe("Rollout plan: Sign this off?");
  expect(controlName(issueAsk, { index: 2, total: 2 })).toBe("CORE-12, ask 2 of 2: Which release?");
});

test("a question longer than a name can carry is cut without a trailing space", () => {
  const long = {
    ...issueAsk,
    question: "Which of the two long-running migration strategies do we adopt?",
  };
  expect(controlName(long, undefined)).toBe(
    "CORE-12: Which of the two long-running migration strategi…"
  );

  // The cut lands on a space in this one: the ellipsis follows the word, not a gap.
  const atSpace = { ...issueAsk, question: `${"x".repeat(47)} tail` };
  expect(controlName(atSpace, undefined)).toBe(`CORE-12: ${"x".repeat(47)}…`);
});

test("a question's own line breaks and runs of spaces collapse to single spaces", () => {
  expect(controlName({ ...issueAsk, question: "  Which\n\n release? " }, undefined)).toBe(
    "CORE-12: Which release?"
  );
});
