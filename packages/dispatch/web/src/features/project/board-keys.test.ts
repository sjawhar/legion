import { expect, test } from "bun:test";

import type { IssueSummary } from "../../api/types";
import { announceMove, focusTarget, keyboardMove } from "./board-keys";
import { groupIssuesByStatus, type IssueStatus } from "./board-model";

function issue(key: string, status: IssueStatus): IssueSummary {
  return {
    key,
    labels: [],
    last_seq: 1,
    open_asks: 0,
    parent: null,
    assignee: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    priority: null,
    rank: "U",
    status,
    title: key,
    updated_at: "2026-09-15T00:00:00Z",
  };
}

// Todo: Alpha, Bravo, Charlie; In progress: Delta; Retro: Echo, Foxtrot; everything else empty.
const columns = groupIssuesByStatus([
  issue("Alpha", "todo"),
  issue("Bravo", "todo"),
  issue("Charlie", "todo"),
  issue("Delta", "in_progress"),
  issue("Echo", "retro"),
  issue("Foxtrot", "retro"),
]);
const rails = (status: IssueStatus) => status === "icebox" || status === "done";
const none = () => false;

test("j and k rove within a column and clamp at both ends", () => {
  expect(focusTarget(columns, rails, { status: "todo", index: 0 }, "j")).toEqual({
    status: "todo",
    index: 1,
  });
  expect(focusTarget(columns, rails, { status: "todo", index: 2 }, "j")).toEqual({
    status: "todo",
    index: 2,
  });
  expect(focusTarget(columns, rails, { status: "todo", index: 1 }, "k")).toEqual({
    status: "todo",
    index: 0,
  });
  expect(focusTarget(columns, rails, { status: "todo", index: 0 }, "k")).toEqual({
    status: "todo",
    index: 0,
  });
  // A focused empty column stays the column.
  expect(focusTarget(columns, rails, { status: "testing", index: null }, "j")).toEqual({
    status: "testing",
    index: null,
  });
});

test("h and l step to the adjacent status, clamping the index into a shorter column", () => {
  expect(focusTarget(columns, rails, { status: "todo", index: 2 }, "l")).toEqual({
    status: "in_progress",
    index: 0,
  });
  expect(focusTarget(columns, rails, { status: "in_progress", index: 0 }, "h")).toEqual({
    status: "todo",
    index: 0,
  });
});

test("h and l land on an empty column or a collapsed rail itself, and stay at the board's ends", () => {
  expect(focusTarget(columns, rails, { status: "in_progress", index: 0 }, "l")).toEqual({
    status: "testing",
    index: null,
  });
  expect(focusTarget(columns, rails, { status: "testing", index: null }, "l")).toEqual({
    status: "needs_review",
    index: null,
  });
  // From a column into a non-empty one: index null lands on the first card.
  expect(focusTarget(columns, rails, { status: "needs_review", index: null }, "l")).toEqual({
    status: "retro",
    index: 0,
  });
  // Done is collapsed: the rail, never a card, even though it would hold cards when expanded.
  const withDone = groupIssuesByStatus([issue("Golf", "done"), issue("Hotel", "retro")]);
  expect(focusTarget(withDone, rails, { status: "retro", index: 0 }, "l")).toEqual({
    status: "done",
    index: null,
  });
  expect(focusTarget(withDone, none, { status: "retro", index: 0 }, "l")).toEqual({
    status: "done",
    index: 0,
  });
  expect(focusTarget(columns, rails, { status: "done", index: null }, "l")).toEqual({
    status: "done",
    index: null,
  });
  expect(focusTarget(columns, rails, { status: "triage", index: null }, "h")).toEqual({
    status: "triage",
    index: null,
  });
});

test("with nothing focused, j/l pick the first card and k/h the last, skipping collapsed rails", () => {
  expect(focusTarget(columns, rails, null, "j")).toEqual({ status: "todo", index: 0 });
  expect(focusTarget(columns, rails, null, "l")).toEqual({ status: "todo", index: 0 });
  expect(focusTarget(columns, rails, null, "k")).toEqual({ status: "retro", index: 1 });
  expect(focusTarget(columns, rails, null, "h")).toEqual({ status: "retro", index: 1 });
  // A card in a collapsed rail is not focusable; the edges expanded, it is the last card.
  const doneOnly = groupIssuesByStatus([issue("Golf", "done"), issue("India", "icebox")]);
  expect(focusTarget(doneOnly, rails, null, "j")).toEqual({ status: "triage", index: null });
  expect(focusTarget(doneOnly, rails, null, "k")).toEqual({ status: "done", index: null });
  expect(focusTarget(doneOnly, none, null, "j")).toEqual({ status: "icebox", index: 0 });
  expect(focusTarget(doneOnly, none, null, "k")).toEqual({ status: "done", index: 0 });
});

test("keyboardMove down/up use moveIssue's insertion index (the column without the card) and stop at the ends", () => {
  expect(keyboardMove(columns, "Alpha", "down")).toEqual({ status: "todo", insertionIndex: 1 });
  expect(keyboardMove(columns, "Bravo", "down")).toEqual({ status: "todo", insertionIndex: 2 });
  expect(keyboardMove(columns, "Charlie", "down")).toBeUndefined();
  expect(keyboardMove(columns, "Bravo", "up")).toEqual({ status: "todo", insertionIndex: 0 });
  expect(keyboardMove(columns, "Alpha", "up")).toBeUndefined();
  expect(keyboardMove(columns, "Zulu", "down")).toBeUndefined();
});

test("keyboardMove prev/next target the top of the adjacent status, Done and Triage included, and stop at the ends", () => {
  expect(keyboardMove(columns, "Bravo", "next")).toEqual({
    status: "in_progress",
    insertionIndex: 0,
  });
  expect(keyboardMove(columns, "Echo", "next")).toEqual({ status: "done", insertionIndex: 0 });
  expect(keyboardMove(columns, "Delta", "prev")).toEqual({ status: "todo", insertionIndex: 0 });
  const withTriage = groupIssuesByStatus([issue("Golf", "triage"), issue("Hotel", "done")]);
  expect(keyboardMove(withTriage, "Golf", "prev")).toBeUndefined();
  expect(keyboardMove(withTriage, "Hotel", "next")).toBeUndefined();
  expect(keyboardMove(withTriage, "Hotel", "prev")).toEqual({ status: "retro", insertionIndex: 0 });
});

test("announceMove names the card, the column and its position, or that Done closed it", () => {
  expect(announceMove("CORE-12", "todo", 2, 5)).toBe("CORE-12 → Todo, position 2 of 5");
  expect(announceMove("CORE-12", "in_progress", 1, 2)).toBe(
    "CORE-12 → In progress, position 1 of 2"
  );
  expect(announceMove("CORE-12", "done", 1, 1)).toBe("CORE-12 → Done, closed");
});
