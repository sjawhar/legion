import { expect, test } from "bun:test";

import type { InboxRow, MyReplyThreadRow } from "../../api/types";
import { agentRow, replyRow } from "../threads/replies-fixture";
import { dimensionValue, filterCounts, group, inFieldFilter, sortRows } from "./grouping";
import { issueAsk } from "./inbox-fixture";
import type { InboxListRow } from "./list-row";

const viewer = "alice";
const titles = { agent: "Waiting on agents", human: "Waiting on you", later: "Later" };
/** Every test here reads the clock once, so a snooze is either ahead of it or behind it. */
const now = Date.parse("2026-10-12T12:00:00Z");

function ask(overrides: Partial<InboxRow>): InboxListRow {
  return { ask: issueAsk(overrides), kind: "ask" };
}

function thread(overrides: Partial<MyReplyThreadRow>): InboxListRow {
  return { kind: "thread", thread: replyRow(overrides.thread_id ?? "t", overrides) };
}

function agentListRow(threadId: string): InboxListRow {
  return { kind: "thread", thread: agentRow(threadId) };
}

const keysOf = (rows: readonly InboxListRow[], by: Parameters<typeof group>[1]) =>
  group(rows, by, { now, titles, viewer }).map(({ key }) => key);

test("every dimension reads the same word off an ask and off a thread", () => {
  const p1 = ask({ id: "a", issue_key: "CORE-1", priority: 1, urgency: "high" });
  const agentThread = agentListRow("t1");

  expect(dimensionValue(p1, "priority", viewer)).toBe("P1");
  expect(dimensionValue(p1, "urgency", viewer)).toBe("high");
  expect(dimensionValue(p1, "type", viewer)).toBe("question");
  expect(dimensionValue(p1, "project", viewer)).toBe("CORE");
  expect(dimensionValue(p1, "assignee", viewer)).toBe("alice");

  expect(dimensionValue(agentThread, "priority", viewer)).toBe("none");
  expect(dimensionValue(agentThread, "urgency", viewer)).toBe("none");
  expect(dimensionValue(agentThread, "type", viewer)).toBe("agent");
  expect(dimensionValue(agentThread, "project", viewer)).toBe("none");
  // Every thread is the reader's own, so the Assignee field puts them all under the reader.
  expect(dimensionValue(agentThread, "assignee", viewer)).toBe("alice");
});

test("a sort puts its field first, none last, and orders a tie newest first", () => {
  const p0 = ask({ created_at: "2026-10-01T00:00:00Z", id: "p0", priority: 0 });
  const p2old = ask({ created_at: "2026-10-02T00:00:00Z", id: "p2-old", priority: 2 });
  const p2new = ask({ created_at: "2026-10-03T00:00:00Z", id: "p2-new", priority: 2 });
  const none = ask({ created_at: "2026-10-09T00:00:00Z", id: "none", priority: null });

  expect(sortRows([none, p2old, p0, p2new], "priority", viewer).map((row) => rowKey(row))).toEqual([
    "p0",
    "p2-new",
    "p2-old",
    "none",
  ]);
  // `newest` is the last write in each row, whatever its priority.
  expect(sortRows([p0, p2old, none, p2new], "newest", viewer).map((row) => rowKey(row))).toEqual([
    "none",
    "p2-new",
    "p2-old",
    "p0",
  ]);
});

function rowKey(row: InboxListRow): string {
  return row.kind === "ask" ? row.ask.id : row.thread.thread_id;
}

test("grouping bands by the chosen field, each field in its own order", () => {
  const rows = [
    ask({ id: "p2", priority: 2 }),
    ask({ id: "none", priority: null }),
    ask({ id: "p0", priority: 0 }),
  ];
  expect(keysOf(rows, "priority")).toEqual(["P0", "P2", "none"]);

  const typed = [
    agentListRow("agent"),
    ask({ id: "question" }),
    ask({ id: "approval", kind: "approval" }),
    thread({ thread_id: "reply" }),
  ];
  expect(keysOf(typed, "type")).toEqual(["question", "approval", "reply", "agent"]);

  const projects = [
    ask({
      id: "ops",
      issue: { assignee: "alice", key: "OPS-1", title: "Ops" },
      issue_key: "OPS-1",
    }),
    ask({ id: "core", issue_key: "CORE-1" }),
    ask({ id: "unowned", issue: undefined, issue_key: null }),
  ];
  expect(keysOf(projects, "project")).toEqual(["CORE", "OPS", "none"]);
});

test("Waiting on bands by whose turn it is, and a thread waits on the reader", () => {
  const rows = [
    ask({ id: "agent", waiting_on: "agent" }),
    thread({ thread_id: "t1" }),
    ask({ id: "human", waiting_on: "human" }),
  ];
  const bands = group(rows, "waiting-on", { now, titles, viewer });
  expect(bands.map(({ key, label }) => [key, label])).toEqual([
    ["human", "Waiting on you"],
    ["agent", "Waiting on agents"],
  ]);
  expect(bands[0]?.rows.map(rowKey)).toEqual(["t1", "human"]);
});

test("None is one unheaded band, and a snoozed ask folds into Later under every grouping", () => {
  const rows = [
    ask({ id: "open" }),
    ask({ id: "deferred", snoozed_until: "2026-10-13T00:00:00Z" }),
    thread({ thread_id: "t1" }),
  ];
  for (const by of ["none", "priority", "waiting-on", "type"] as const) {
    const bands = group(rows, by, { now, titles, viewer });
    const later = bands.at(-1);
    expect(later?.key).toBe("later");
    expect(later?.label).toBe("Later");
    expect(later?.rows.map(rowKey)).toEqual(["deferred"]);
  }
  const ungrouped = group(rows, "none", { now, titles, viewer });
  expect(ungrouped[0]?.key).toBe("all");
  expect(ungrouped[0]?.label).toBeUndefined();
  expect(ungrouped[0]?.rows.map(rowKey)).toEqual(["open", "t1"]);
  // A snooze whose moment has passed is an ordinary row again.
  const returned = group([ask({ id: "back", snoozed_until: "2026-10-11T00:00:00Z" })], "none", {
    now,
    titles,
    viewer,
  });
  expect(returned.map(({ key }) => key)).toEqual(["all"]);
});

test("a field's counts are taken under the other fields' filters, not its own", () => {
  const rows = [
    ask({ id: "a", priority: 0, urgency: "blocking" }),
    ask({ id: "b", priority: 1, urgency: "blocking" }),
    ask({ id: "c", priority: 1, urgency: "low" }),
  ];
  const chosen = { priority: ["P1"] };
  const counts = filterCounts(
    rows,
    "urgency",
    (row) => inFieldFilter(row, "priority", chosen.priority, viewer),
    viewer
  );
  // Only the P1 rows are counted, and both urgencies of them are, so neither reads 0.
  expect(counts.get("blocking")).toBe(1);
  expect(counts.get("low")).toBe(1);
});
