import { describe, expect, test } from "bun:test";

import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssue,
} from "../../api/types";
import {
  componentPath,
  componentTone,
  modelDistrusted,
  placeChild,
  predictTree,
  sourceState,
  splitWork,
} from "./architecture-model";

function component(
  overrides: Partial<ArchitectureTreeComponent> & { id: string }
): ArchitectureTreeComponent {
  return {
    depends_on: [],
    done: 0,
    external: false,
    issues: [],
    own_done: 0,
    own_total: 0,
    parent: null,
    paths: [],
    prose: "",
    title: overrides.id,
    total: 0,
    ...overrides,
  };
}

function issue(overrides: Partial<ArchitectureTreeIssue> & { key: string }): ArchitectureTreeIssue {
  return {
    attached: "direct",
    external_links: [],
    parent: null,
    priority: null,
    status: "todo",
    title: overrides.key,
    updated_at: "2026-09-17T00:00:00Z",
    ...overrides,
  };
}

function tree(components: ArchitectureTreeComponent[]): ArchitectureTree {
  return {
    components,
    not_architectural: [],
    retired_links: [],
    source: {
      branch: "main",
      last_commit: "abc123",
      last_error: null,
      last_sync_at: "2026-09-17T00:00:00Z",
      repo: "legion/legion",
    },
    totals: {
      components_without_work: components.filter((c) => !c.external && c.total === 0).length,
      issues_done: 0,
      issues_total: 0,
      not_architectural: 0,
      retired_links: 0,
      unassigned: 0,
    },
    unassigned: [],
  };
}

describe("componentTone (spec item 4)", () => {
  test.each([
    ["0/0", { done: 0, total: 0 }, "no-work"],
    ["0/N", { done: 0, total: 4 }, "untouched"],
    ["D/N", { done: 2, total: 4 }, "partial"],
    ["N/N", { done: 4, total: 4 }, "done"],
  ] as const)("%s without descendants", (_label, counts, expected) => {
    const web = component({ id: "web", ...counts });
    expect(componentTone(web, [web])).toBe(expected);
  });

  test("N/N with a non-external descendant that has no work is done-with-gaps", () => {
    const legion = component({ done: 3, id: "legion", total: 3 });
    const daemon = component({ done: 3, id: "daemon", parent: "legion", total: 3 });
    const skills = component({ id: "skills", parent: "daemon" });
    expect(componentTone(legion, [legion, daemon, skills])).toBe("done-with-gaps");
    expect(componentTone(daemon, [legion, daemon, skills])).toBe("done-with-gaps");
    expect(componentTone(skills, [legion, daemon, skills])).toBe("no-work");
  });

  test("an external descendant without work does not block green, and external is its own tone", () => {
    const legion = component({ done: 3, id: "legion", total: 3 });
    const github = component({ external: true, id: "github", parent: "legion" });
    expect(componentTone(legion, [legion, github])).toBe("done");
    expect(componentTone(github, [legion, github])).toBe("external");
  });

  test("an external component with counts is still external, never a bar", () => {
    const github = component({ done: 1, external: true, id: "github", total: 1 });
    expect(componentTone(github, [github])).toBe("external");
  });
});

describe("source state (spec item 6)", () => {
  const now = Date.parse("2026-09-17T12:00:00Z");
  test("never synced until a commit was imported", () => {
    expect(
      sourceState(
        { branch: "main", last_commit: null, last_error: null, last_sync_at: null, repo: "r" },
        now
      )
    ).toBe("never-synced");
  });
  test("fresh within two ticker intervals, stale after", () => {
    const fresh = {
      branch: "main",
      last_commit: "abc",
      last_error: null,
      last_sync_at: "2026-09-17T11:51:00Z",
      repo: "r",
    };
    expect(sourceState(fresh, now)).toBe("fresh");
    expect(modelDistrusted(fresh, now)).toBe(false);
    const stale = { ...fresh, last_sync_at: "2026-09-17T11:49:59Z" };
    expect(sourceState(stale, now)).toBe("stale");
    expect(modelDistrusted(stale, now)).toBe(true);
  });
  test("a failed import distrusts the model even when fresh", () => {
    expect(
      modelDistrusted(
        {
          branch: "main",
          last_commit: "abc",
          last_error: "duplicate component id",
          last_sync_at: "2026-09-17T11:59:00Z",
          repo: "r",
        },
        now
      )
    ).toBe(true);
  });
});

test("splitWork orders unfinished issues by lifecycle status then newest activity, done apart", () => {
  const { done, open } = splitWork([
    issue({ key: "A", status: "done", updated_at: "2026-09-01T00:00:00Z" }),
    issue({ key: "B", status: "todo", updated_at: "2026-09-02T00:00:00Z" }),
    issue({ key: "C", status: "in_progress", updated_at: "2026-09-01T00:00:00Z" }),
    issue({ key: "D", status: "todo", updated_at: "2026-09-03T00:00:00Z" }),
    issue({ key: "E", status: "done", updated_at: "2026-09-05T00:00:00Z" }),
    issue({ key: "F", status: "triage", updated_at: "2026-09-01T00:00:00Z" }),
  ]);
  expect(open.map((row) => row.key)).toEqual(["F", "D", "B", "C"]);
  expect(done.map((row) => row.key)).toEqual(["E", "A"]);
});

test("componentPath walks from the root to the component", () => {
  const legion = component({ id: "legion" });
  const dispatch = component({ id: "dispatch", parent: "legion" });
  const web = component({ id: "web", parent: "dispatch" });
  const index = new Map([legion, dispatch, web].map((c) => [c.id, c]));
  expect(componentPath(index, "web").map((c) => c.id)).toEqual(["legion", "dispatch", "web"]);
  expect(componentPath(index, "legion").map((c) => c.id)).toEqual(["legion"]);
  expect(componentPath(index, "missing")).toEqual([]);
});

test("placeChild finds a child in this component, another component, or the side lists", () => {
  const web = component({ id: "web", issues: [issue({ key: "CORE-2", parent: "CORE-1" })] });
  const server = component({ id: "server", issues: [issue({ key: "CORE-3" })] });
  const model = {
    ...tree([web, server]),
    not_architectural: [
      { inherited_from: null, key: "CORE-4", reason: "hiring", status: "todo", title: "Hire" },
    ],
    unassigned: [{ key: "CORE-5", status: "todo", title: "Loose" }],
  };
  expect(placeChild(model, "web", "CORE-2").kind).toBe("here");
  expect(placeChild(model, "web", "CORE-3")).toEqual({ component: "server", kind: "elsewhere" });
  expect(placeChild(model, "web", "CORE-4").kind).toBe("not-architectural");
  expect(placeChild(model, "web", "CORE-5").kind).toBe("unassigned");
  expect(placeChild(model, "web", "CORE-6").kind).toBe("uncounted");
});

describe("predictTree (the optimistic move)", () => {
  const legion = component({ id: "legion" });
  const dispatch = component({ id: "dispatch", parent: "legion" });
  const web = component({ id: "web", parent: "dispatch" });
  const server = component({ id: "server", parent: "dispatch" });
  const loose = { key: "CORE-9", status: "done", title: "Loose" };
  const base = {
    ...tree([legion, dispatch, web, server]),
    totals: {
      components_without_work: 4,
      issues_done: 1,
      issues_total: 1,
      not_architectural: 0,
      retired_links: 0,
      unassigned: 1,
    },
    unassigned: [loose],
  };

  test("an explicit attach moves the issue out of Unassigned and counts it once per component up the chain", () => {
    const next = predictTree(base, loose, { ids: ["web", "server"], mode: "explicit" }, "now");
    expect(next.unassigned).toEqual([]);
    expect(next.totals.unassigned).toBe(0);
    const counts = Object.fromEntries(
      next.components.map((c) => [c.id, `${c.done}/${c.total} own ${c.own_done}/${c.own_total}`])
    );
    expect(counts).toEqual({
      dispatch: "1/1 own 0/0",
      legion: "1/1 own 0/0",
      server: "1/1 own 1/1",
      web: "1/1 own 1/1",
    });
    const rows = Object.fromEntries(
      next.components.map((c) => [c.id, c.issues.map((row) => `${row.attached}:${row.via ?? ""}`)])
    );
    expect(rows).toEqual({
      dispatch: ["contained:web"],
      legion: ["contained:web"],
      server: ["direct:"],
      web: ["direct:"],
    });
    expect(next.totals.components_without_work).toBe(0);
  });

  test("a none write moves the issue into Not architectural with its reason", () => {
    const next = predictTree(base, loose, { mode: "none", reason: "hiring" }, "now");
    expect(next.unassigned).toEqual([]);
    expect(next.not_architectural).toEqual([
      { inherited_from: null, key: "CORE-9", reason: "hiring", status: "done", title: "Loose" },
    ]);
    expect(next.totals.not_architectural).toBe(1);
  });

  test("a reattach removes the issue's old rows and counts before adding the new ones", () => {
    const attached = predictTree(base, loose, { ids: ["web"], mode: "explicit" }, "now");
    const moved = predictTree(attached, loose, { ids: ["server"], mode: "explicit" }, "now");
    const counts = Object.fromEntries(moved.components.map((c) => [c.id, `${c.done}/${c.total}`]));
    expect(counts).toEqual({ dispatch: "1/1", legion: "1/1", server: "1/1", web: "0/0" });
    expect(moved.components.find((c) => c.id === "web")?.issues).toEqual([]);
  });

  test("an inherit write only removes the issue; where it lands is the refetch's to say", () => {
    const declared = predictTree(base, loose, { mode: "none", reason: "hiring" }, "now");
    const next = predictTree(declared, loose, { mode: "inherit" }, "now");
    expect(next.not_architectural).toEqual([]);
    expect(next.unassigned).toEqual([]);
  });
});
