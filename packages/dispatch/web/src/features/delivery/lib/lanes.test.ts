import { expect, test } from "bun:test";

import type { DeliveryPR } from "../../../api/types";
import type { Filters } from "./facets";
import {
  brushWindow,
  foldLanes,
  LEFT_AXIS_WIDTH,
  MIN_LANE_WIDTH_PX,
  OTHER_LANE,
  RIGHT_SLIDER_WIDTH,
} from "./lanes";

const noFilters: Filters = {
  repo: [],
  parentAgent: [],
  session: [],
  issue: [],
  priority: [],
  component: [],
  author: [],
  rework: [],
  deployed: [],
  search: "",
};

const base: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: a widget",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

/** `count` pull requests by `author`, numbered from `from`. */
function byAuthor(author: string, count: number, from: number): DeliveryPR[] {
  return Array.from({ length: count }, (_, i) => ({
    ...base,
    author,
    id: `acme/widgets#${from + i}`,
    number: from + i,
  }));
}

/** The chart width that fits `lanes` lane columns beside the Deploys/fails column. */
function widthFor(lanes: number): number {
  return LEFT_AXIS_WIDTH + RIGHT_SLIDER_WIDTH + MIN_LANE_WIDTH_PX * (lanes + 1);
}

// Six authors, busiest first: a (6), b (5), c (4), d (3), e (2), f (1), plus two pull requests
// with no issue, which colour by priority under the No issue placeholder.
const prs = [
  ...byAuthor("a", 6, 1),
  ...byAuthor("b", 5, 100),
  ...byAuthor("c", 4, 200),
  ...byAuthor("d", 3, 300),
  ...byAuthor("e", 2, 400),
  ...byAuthor("f", 1, 500),
];

test("swimlanes off: one Merges column holding every merge", () => {
  expect(
    foldLanes(prs, {
      colorBy: "author",
      lanes: false,
      filters: noFilters,
      width: widthFor(1),
      components: {},
      sessionGroups: [],
    })
  ).toEqual([{ key: "__merges__", label: "Merges", count: 21 }]);
});

test("the busiest values get lanes, the rest fold into Other, and the lanes never exceed the budget", () => {
  const lanes = foldLanes(prs, {
    colorBy: "author",
    lanes: true,
    filters: noFilters,
    width: widthFor(3),
    components: {},
    sessionGroups: [],
  });
  expect(lanes).toEqual([
    { key: "a", label: "a", count: 6 },
    { key: "b", label: "b", count: 5 },
    { key: OTHER_LANE, label: "Other", count: 10 },
  ]);
});

test("a one-lane budget folds every value into Other, never one value beside it", () => {
  const lanes = foldLanes(prs, {
    colorBy: "author",
    lanes: true,
    filters: noFilters,
    width: widthFor(1),
    components: {},
    sessionGroups: [],
  });
  expect(lanes).toEqual([{ key: OTHER_LANE, label: "Other", count: 21 }]);
});

test("placeholder lanes come after Other and always show", () => {
  const withPriority = [
    ...prs.slice(0, 3).map((pr) => ({ ...pr, issue: "ACME-1", priority: "P0" as const })),
    ...prs.slice(3, 5).map((pr) => ({ ...pr, issue: "ACME-2", priority: "P1" as const })),
    ...prs.slice(5, 6).map((pr) => ({ ...pr, issue: "ACME-3", priority: "P2" as const })),
    ...prs.slice(6, 8),
  ];
  const lanes = foldLanes(withPriority, {
    colorBy: "priority",
    lanes: true,
    filters: noFilters,
    width: widthFor(3),
    components: {},
    sessionGroups: [],
  });
  expect(lanes.map((lane) => [lane.label, lane.count])).toEqual([
    ["P0", 3],
    ["Other", 3],
    ["No issue", 2],
  ]);
});

test("a facet with values picked in its own filter shows exactly those, never folded", () => {
  const lanes = foldLanes(prs, {
    colorBy: "author",
    lanes: true,
    filters: { ...noFilters, author: ["a", "b", "c", "d", "e", "f"] },
    width: widthFor(1),
    components: {},
    sessionGroups: [],
  });
  expect(lanes.map((lane) => lane.key)).toEqual(["a", "b", "c", "d", "e", "f"]);
});

test("laned by Parent agent, a group's lane reads as the group's name", () => {
  const grouped = [
    ...byAuthor("a", 2, 1).map((pr) => ({ ...pr, parent_agent: "group:g1" })),
    ...byAuthor("b", 1, 100).map((pr) => ({ ...pr, parent_agent: "Reviewer" })),
  ];
  const lanes = foldLanes(grouped, {
    colorBy: "parentAgent",
    lanes: true,
    filters: noFilters,
    width: widthFor(3),
    components: {},
    sessionGroups: [
      {
        created_at: "2026-10-11T00:00:00Z",
        created_by: { id: "alice@example.com", kind: "user" },
        id: "g1",
        name: "Release crew",
        sessions: ["s1", "s2"],
        updated_at: "2026-10-11T00:00:00Z",
        updated_by: { id: "alice@example.com", kind: "user" },
      },
    ],
  });
  expect(lanes.map((lane) => [lane.key, lane.label])).toEqual([
    ["group:g1", "Release crew"],
    ["Reviewer", "Reviewer"],
  ]);
});

test("laned by Parent agent with values chosen, each chosen value has its lane, in the chosen order, and the rest share Other at any width", () => {
  const agents = [
    ...byAuthor("x", 3, 1).map((pr) => ({ ...pr, parent_agent: "a" })),
    ...byAuthor("x", 2, 100).map((pr) => ({ ...pr, parent_agent: "b" })),
    ...byAuthor("x", 1, 200).map((pr) => ({ ...pr, parent_agent: "c" })),
    ...byAuthor("x", 1, 300),
  ];
  for (const width of [widthFor(1), widthFor(8)]) {
    const lanes = foldLanes(agents, {
      colorBy: "parentAgent",
      lanes: true,
      filters: noFilters,
      width,
      components: {},
      sessionGroups: [],
      chosen: ["b", "b"],
    });
    expect(lanes.map((lane) => [lane.key, lane.count])).toEqual([
      ["b", 2],
      [OTHER_LANE, 4],
      ["__no_session__", 1],
    ]);
  }
});

test("a drag becomes the brush window earlier end first, and an unplaced end sets none", () => {
  const early = Date.parse("2024-06-01T02:00:00Z");
  const late = Date.parse("2024-06-01T05:00:00Z");
  expect(brushWindow(late, early)).toEqual({
    start: "2024-06-01T02:00:00.000Z",
    end: "2024-06-01T05:00:00.000Z",
  });
  expect(brushWindow(early, Number.NaN)).toBeNull();
  expect(brushWindow([early], late)).toBeNull();
});
