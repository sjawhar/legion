import { expect, test } from "bun:test";
import after from "../../../../../../contracts/fixtures/delivery-live/after.json";
import delta from "../../../../../../contracts/fixtures/delivery-live/delta.json";
import snapshot from "../../../../../../contracts/fixtures/delivery-live/snapshot.json";
import type {
  DeliveryLiveDelta,
  DeliveryLiveRevs,
  DeliveryLiveSnapshot,
  DeliveryPR,
  DeliveryRun,
  DeliverySessionGroup,
  DeliveryTimelineResponse,
} from "../../../api/types";
import { applyDelta, applyRevs } from "./applyDelta";

function pr(number: number, mergedAt: string, title = `feat: change ${number}`): DeliveryPR {
  return {
    additions: null,
    author: "octocat",
    components: [],
    created_at: mergedAt,
    deletions: null,
    deploy_run: null,
    deployed_at: null,
    deployed_status: "waiting",
    first_commit_at: null,
    id: `acme/widgets#${number}`,
    issue: null,
    issue_title: null,
    merged_at: mergedAt,
    number,
    parent_agent: null,
    partial: false,
    priority: null,
    repo: "acme/widgets",
    rework: false,
    sessions: [],
    title,
    unfetchable_reason: null,
    url: `https://github.com/acme/widgets/pull/${number}`,
  };
}

function run(id: number, headAt: string): DeliveryRun {
  return {
    completed_at: null,
    conclusion: null,
    failed_jobs: [],
    head_at: headAt,
    head_sha: `${id}`.padStart(40, "0"),
    id,
    production: null,
    prs: [],
    root_failing_job: null,
    started_at: headAt,
    url: `https://github.com/acme/widgets/actions/runs/${id}`,
  };
}

function group(id: string, name: string): DeliverySessionGroup {
  const actor = { id: "alice@example.com", kind: "user" } as const;
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_by: actor,
    id,
    name,
    sessions: [],
    updated_at: "2026-10-01T00:00:00Z",
    updated_by: actor,
  };
}

const body = {
  window: { from: "2026-10-01T00:00:00Z", to: "2026-10-03T00:00:00Z" },
  prs: [pr(1, "2026-10-01T01:00:00Z"), pr(2, "2026-10-01T02:00:00Z")],
  waiting: [{ merged_at: "2026-09-30T21:00:00Z", deployed_at: null }],
  runs: [run(7, "2026-10-01T03:00:00Z")],
  facet_counts: { repo: { "acme/widgets": 2 }, author: { octocat: 2 } },
  color_counts: { repo: { "acme/widgets": 2 } },
  components: { "ACME/api": { title: "api", parent: null } },
  issue_titles: { "ACME-1": "First issue" },
  session_groups: [group("g2", "Reviewers"), group("g1", "planners")],
  session_titles: { "s-1": "Session s-1" },
  session_pr_counts: { "s-1": 2 },
} as unknown as DeliveryTimelineResponse;

const noChange: DeliveryLiveDelta = {
  type: "delta",
  sub: "timeline-1",
  seq: "2",
  changed_at: null,
  set: {},
  items: {},
  maps: {},
  counts: {},
  revs: {},
};

test("an upsert replaces the item with its id and keeps the server's order", () => {
  // #2 retitled, #3 merged between #1 and #2 (a fraction the server writes with fewer digits sorts
  // by its instant, not its text), and a run whose head is older than run 7's.
  const next = applyDelta(body, {
    ...noChange,
    items: {
      prs: {
        upsert: [pr(2, "2026-10-01T02:00:00Z", "feat: renamed"), pr(3, "2026-10-01T01:30:00.5Z")],
        remove: [],
      },
      runs: { upsert: [run(8, "2026-10-01T02:59:00Z")], remove: [] },
      session_groups: { upsert: [group("g0", "planners"), group("g3", "Agents")], remove: [] },
    },
  });
  expect(next.prs.map((item) => [item.id, item.title])).toEqual([
    ["acme/widgets#1", "feat: change 1"],
    ["acme/widgets#3", "feat: change 3"],
    ["acme/widgets#2", "feat: renamed"],
  ]);
  expect(next.runs.map((item) => item.id)).toEqual([8, 7]);
  // By name, case aside, then by id: the server's `order by lower(g.name), g.id`.
  expect(next.session_groups.map((item) => item.id)).toEqual(["g3", "g0", "g1", "g2"]);
  // An item no delta named is the same object it was.
  expect(next.prs[0]).toBe(body.prs[0]);
  expect(next.waiting).toBe(body.waiting);
});

test("a remove drops the item", () => {
  const next = applyDelta(body, {
    ...noChange,
    items: {
      prs: { upsert: [], remove: ["acme/widgets#1"] },
      runs: { upsert: [], remove: ["7"] },
    },
  });
  expect(next.prs.map((item) => item.id)).toEqual(["acme/widgets#2"]);
  expect(next.runs).toEqual([]);
});

test("removing an id the page never held changes nothing", () => {
  const next = applyDelta(body, {
    ...noChange,
    items: { prs: { upsert: [], remove: ["acme/widgets#999"] } },
  });
  expect(next.prs).toEqual(body.prs);
  expect(next.prs[0]).toBe(body.prs[0]);
});

test("a list with no order is refused by name", () => {
  const unknownList = {
    ...noChange,
    items: { unknown_list: { upsert: [], remove: [] } },
  } as unknown as DeliveryLiveDelta;
  expect(() => applyDelta(body, unknownList)).toThrow("unknown_list");
});

test("a map entry is set, and a null entry is deleted", () => {
  const next = applyDelta(body, {
    ...noChange,
    maps: {
      issue_titles: { "ACME-1": null, "ACME-2": "Second issue" },
      session_pr_counts: { "s-1": 3 },
      components: { "ACME/web": { title: "web", parent: "ACME/api" } },
    },
  });
  expect(next.issue_titles).toEqual({ "ACME-2": "Second issue" });
  expect(next.session_pr_counts).toEqual({ "s-1": 3 });
  expect(next.components).toEqual({
    "ACME/api": { title: "api", parent: null },
    "ACME/web": { title: "web", parent: "ACME/api" },
  });
  expect(next.session_titles).toBe(body.session_titles);
  expect(body.issue_titles).toEqual({ "ACME-1": "First issue" });
});

test("a count is set inside its facet, and a null count is deleted", () => {
  const next = applyDelta(body, {
    ...noChange,
    counts: {
      facet_counts: {
        repo: { "acme/widgets": 3 },
        author: { octocat: null, hubot: 1 },
        issue: { "ACME-2": 1 },
      },
    },
  });
  expect(next.facet_counts as unknown).toEqual({
    repo: { "acme/widgets": 3 },
    author: { hubot: 1 },
    issue: { "ACME-2": 1 },
  });
  expect(next.color_counts).toBe(body.color_counts);
  expect(body.facet_counts.author).toEqual({ octocat: 2 });
});

test("a whole field is replaced, waiting included", () => {
  const waiting = [{ merged_at: "2026-09-30T21:00:00Z", deployed_at: "2026-10-01T04:00:00Z" }];
  const window = { from: "2026-10-01T00:05:00Z", to: "2026-10-03T00:05:00Z" };
  const next = applyDelta(body, { ...noChange, set: { waiting, window } });
  expect(next.waiting).toEqual(waiting);
  expect(next.window).toEqual(window);
  expect(next.prs).toBe(body.prs);
});

test("an empty delta returns the same object", () => {
  expect(applyDelta(body, noChange)).toBe(body);
});

test("revision changes apply section by section; null deletes the key", () => {
  const revs: DeliveryLiveRevs = {
    items: { prs: { "acme/widgets#1": "aaaaaaaaaaaaaaaa", "acme/widgets#2": "bbbbbbbbbbbbbbbb" } },
    maps: { issue_titles: { "ACME-1": "cccccccccccccccc" } },
    counts: { facet_counts: { author: { octocat: "dddddddddddddddd" } } },
    fields: { waiting: "eeeeeeeeeeeeeeee", window: "ffffffffffffffff" },
  };
  expect(
    applyRevs(revs, {
      items: { prs: { "acme/widgets#1": null, "acme/widgets#3": "1111111111111111" } },
      maps: { issue_titles: { "ACME-1": null, "ACME-2": "2222222222222222" } },
      counts: {
        facet_counts: { author: { octocat: null, hubot: "3333333333333333" } },
        color_counts: { repo: { "acme/widgets": "4444444444444444" } },
      },
      fields: { waiting: "5555555555555555" },
    })
  ).toEqual({
    items: { prs: { "acme/widgets#2": "bbbbbbbbbbbbbbbb", "acme/widgets#3": "1111111111111111" } },
    maps: { issue_titles: { "ACME-2": "2222222222222222" } },
    counts: {
      facet_counts: { author: { hubot: "3333333333333333" } },
      color_counts: { repo: { "acme/widgets": "4444444444444444" } },
    },
    fields: { waiting: "5555555555555555", window: "ffffffffffffffff" },
  });
  expect(applyRevs(revs, {})).toEqual(revs);
});

test("the server's delta applied to its snapshot is the snapshot a page subscribing afterwards gets", () => {
  // The frames Dispatch's own diff writes (packages/contracts/fixtures/delivery-live): a deploy, a
  // merge and two pull requests leaving the window. A delta never carries freshness, which has its
  // own frame, so it is the one field the two bodies may differ in.
  const held = snapshot as unknown as DeliveryLiveSnapshot;
  const next = after as unknown as DeliveryLiveSnapshot;
  const change = delta as unknown as DeliveryLiveDelta;
  const { freshness: _applied, ...applied } = applyDelta(
    held.body,
    change
  ) as DeliveryTimelineResponse;
  const { freshness: _next, ...expected } = next.body as DeliveryTimelineResponse;
  expect(applied).toEqual(expected);
  expect(applyRevs(held.revs, change.revs)).toEqual(next.revs);
});
