import { expect, test } from "bun:test";

import type { InboxRow } from "../../api/types";
import { issueAsk } from "./inbox-fixture";
import { crossBandCounts, groupRows, type InboxBand } from "./grouping";

function onIssue(id: string, key: string): InboxRow {
  return issueAsk({
    id,
    issue: { assignee: "alice", key, title: `Title for ${key}` },
    issue_key: key,
  });
}

function onDocument(id: string, project: string, slug: string): InboxRow {
  return issueAsk({
    document: { name: `Document ${slug}`, project, slug },
    id,
    issue: undefined,
    issue_key: null,
  });
}

function withoutOwner(id: string): InboxRow {
  return issueAsk({ id, issue: undefined, issue_key: null });
}

test("groupRows puts an owner's group at its first row and preserves server order", () => {
  const first = onIssue("first", "CORE-1");
  const other = onIssue("other", "CORE-2");
  const second = onIssue("second", "CORE-1");
  const third = onIssue("third", "CORE-1");

  const groups = groupRows([first, other, second, third]);

  expect(groups.map((group) => group.owner.key)).toEqual(["issue:CORE-1", "issue:CORE-2"]);
  expect(groups[0]?.rows.map((row) => row.id)).toEqual(["first", "second", "third"]);
  expect(groups[1]?.rows.map((row) => row.id)).toEqual(["other"]);
});

test("groupRows groups document asks by project and slug", () => {
  const first = onDocument("first", "CORE", "roadmap");
  const other = onDocument("other", "CORE", "runbook");
  const second = onDocument("second", "CORE", "roadmap");

  const groups = groupRows([first, other, second]);

  expect(groups.map((group) => group.owner.key)).toEqual([
    "document:CORE/roadmap",
    "document:CORE/runbook",
  ]);
  expect(groups[0]?.rows.map((row) => row.id)).toEqual(["first", "second"]);
});

test("groupRows leaves one ask as a one-row group and never merges unowned rows", () => {
  const alone = onIssue("alone", "CORE-1");
  const first = withoutOwner("first");
  const second = withoutOwner("second");

  const groups = groupRows([alone, first, second]);

  expect(groups).toHaveLength(3);
  expect(groups[0]?.rows).toEqual([alone]);
  expect(groups[1]?.rows).toEqual([first]);
  expect(groups[2]?.rows).toEqual([second]);
});

test("crossBandCounts gives each displayed owner the counts in every other band", () => {
  const bands: InboxBand[] = [
    {
      section: "human",
      rows: [onIssue("a-human", "CORE-1"), onIssue("b-human", "CORE-1"), onIssue("c-human", "CORE-2")],
    },
    {
      section: "agent",
      rows: [onIssue("a-agent", "CORE-1"), onIssue("b-agent", "CORE-2"), onIssue("c-agent", "CORE-2")],
    },
    { section: "later", rows: [onIssue("a-later", "CORE-1")] },
  ];

  const notes = crossBandCounts(bands);

  expect(notes.get("human")?.get("issue:CORE-1")).toEqual([
    { count: 1, section: "agent" },
    { count: 1, section: "later" },
  ]);
  expect(notes.get("human")?.get("issue:CORE-2")).toEqual([{ count: 2, section: "agent" }]);
  expect(notes.get("agent")?.get("issue:CORE-1")).toEqual([
    { count: 2, section: "human" },
    { count: 1, section: "later" },
  ]);
});
