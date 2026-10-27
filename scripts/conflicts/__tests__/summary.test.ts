import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { compare, readRawRun, resolveWindow, summarise } from "../summary.ts";
import { type FixState, type RawRun, UsageError } from "../types.ts";

async function fixture(name: string): Promise<RawRun> {
  return readRawRun(await Bun.file(join(import.meta.dir, "..", "__fixtures__", name)).text());
}

const X_CHANGELOG = "packages/x/CHANGELOG.md";
const Y_CHANGELOG = "packages/y/CHANGELOG.md";

describe("summarise", () => {
  test("counts only merges dated inside the window, bounds included", async () => {
    const { merged, combined } = summarise(await fixture("window.json"), { prefix: "legion/" });
    // m2 sits at the since bound and m5 at the until bound; m3 is one second before the
    // window and m6 one second after.
    expect(merged.forward).toBe(4);
    const paths = combined.tables.files.map((row) => row.path);
    expect(paths).toContain("AGENTS.md");
    expect(paths).not.toContain("docs/guide.md");
    expect(combined.tables.files.find((row) => row.path === "src/a.ts")).toEqual({
      path: "src/a.ts",
      category: "code",
      events: 1,
      prs: [11, 12],
    });
  });

  test("a merge carried by two PRs counts once and is attributed to both", async () => {
    const { merged } = summarise(await fixture("window.json"), { prefix: "legion/" });
    expect(merged).toEqual({
      prs: 2,
      carryingForward: 2,
      forward: 4,
      conflicting: 3,
      conflictingPrs: 2,
      clean: 1,
      cleanPrs: 1,
      undeterminable: 0,
      tiers: [
        { name: "changelog only", hit: 1, total: 3, prs: 2 },
        { name: "changelog + AGENTS/CLAUDE.md + generated", hit: 2, total: 3, prs: 2 },
        { name: "... + daemon API version log", hit: 2, total: 3, prs: 2 },
        { name: "... + any other doc (.md, docs/)", hit: 2, total: 3, prs: 2 },
      ],
      otherBranch: 0,
      internal: 0,
      perPr: [
        { pr: 10, conflicting: 2 },
        { pr: 11, conflicting: 2 },
      ],
      tables: {
        events: 3,
        categories: [
          { category: "changelog", events: 3, prs: 2 },
          { category: "agents-doc", events: 1, prs: 1 },
          { category: "code", events: 1, prs: 1 },
        ],
        files: [
          { path: X_CHANGELOG, category: "changelog", events: 3, prs: [10, 11] },
          { path: "AGENTS.md", category: "agents-doc", events: 1, prs: [10] },
          { path: "src/a.ts", category: "code", events: 1, prs: [11] },
        ],
      },
    });
  });

  test("the other section excludes merges the merged section counted", async () => {
    const { other } = summarise(await fixture("window.json"), { prefix: "legion/" });
    expect(other.prs).toBe(1);
    expect(other.forward).toBe(1);
    expect(other.conflicting).toBe(1);
    expect(other.internal).toBe(1);
    expect(other.perPr).toEqual([{ pr: 12, conflicting: 1 }]);
    expect(other.tables.files.map((row) => row.path)).toEqual([Y_CHANGELOG]);
  });

  test("the combined section deduplicates a merge carried by a merged and an open PR", async () => {
    const { combined } = summarise(await fixture("window.json"), { prefix: "legion/" });
    expect(combined.prs).toBe(3);
    expect(combined.conflicting).toBe(4);
    expect(combined.conflictingPrs).toBe(3);
    expect(combined.clean).toBe(1);
    expect(combined.tiers.map(({ hit, total, prs }) => [hit, total, prs])).toEqual([
      [2, 4, 3],
      [3, 4, 3],
      [3, 4, 3],
      [3, 4, 3],
    ]);
    expect(combined.tables.categories).toEqual([
      { category: "changelog", events: 4, prs: 3 },
      { category: "agents-doc", events: 1, prs: 1 },
      { category: "code", events: 1, prs: 2 },
    ]);
    expect(combined.tables.files).toEqual([
      { path: X_CHANGELOG, category: "changelog", events: 3, prs: [10, 11, 12] },
      { path: "AGENTS.md", category: "agents-doc", events: 1, prs: [10] },
      { path: Y_CHANGELOG, category: "changelog", events: 1, prs: [12] },
      { path: "src/a.ts", category: "code", events: 1, prs: [11, 12] },
    ]);
  });

  test("the branch prefix drops other branches", async () => {
    const run = await fixture("window.json");
    const legion = summarise(run, { prefix: "legion/" });
    const every = summarise(run, { prefix: "" });
    expect(legion.combined.tables.files.map((row) => row.path)).not.toContain("src/b.ts");
    expect(every.combined.prs).toBe(4);
    expect(every.other.prs).toBe(2);
    expect(every.combined.conflicting).toBe(5);
    expect(every.combined.tables.files.find((row) => row.path === "src/b.ts")?.prs).toEqual([13]);
  });

  test("force pushes and the open snapshot", async () => {
    const { forcePushes, snapshot } = summarise(await fixture("window.json"), {
      prefix: "legion/",
    });
    expect(forcePushes.count).toBe(2);
    expect(forcePushes.prs).toBe(2);
    expect(forcePushes.conflicting).toBe(1);
    expect(forcePushes.undeterminable).toBe(1);
    expect(forcePushes.errors).toEqual([{ error: "base is not main", count: 1 }]);
    expect(snapshot.open).toBe(1);
    expect(snapshot.rows).toEqual([{ pr: 12, base: "legion/A-2", conflicts: [Y_CHANGELOG] }]);
  });

  test("a run with a selection splits merged from other by mergedAt in the window", async () => {
    const { merged, other, snapshot } = summarise(await fixture("after.json"), {
      prefix: "legion/",
    });
    expect(merged.prs).toBe(1);
    expect(other.prs).toBe(3);
    expect(snapshot.open).toBe(1);
  });

  test("fix states give per-file before/transition/after and zeros for an unmatched spec", async () => {
    const states = new Map<string, Map<string, FixState>>([
      ["m1", new Map([[X_CHANGELOG, "before"]])],
      ["m2", new Map([[X_CHANGELOG, "transition"]])],
      ["m5", new Map([[X_CHANGELOG, "after"]])],
      ["m7", new Map([[Y_CHANGELOG, "after"]])],
    ]);
    const specs = [
      { glob: "packages/*/CHANGELOG.md", commit: "c1" },
      { glob: "docs/**", commit: "c2" },
    ];
    const summary = summarise(await fixture("window.json"), {
      prefix: "legion/",
      fixes: { specs, states },
    });
    const fixOf = (path: string) =>
      summary.combined.tables.files.find((row) => row.path === path)?.fix;
    expect(fixOf(X_CHANGELOG)).toEqual({ before: 1, transition: 1, after: 1 });
    expect(fixOf(Y_CHANGELOG)).toEqual({ before: 0, transition: 0, after: 1 });
    expect(fixOf("AGENTS.md")).toBeUndefined();
    expect(summary.fixes).toEqual([
      { glob: "packages/*/CHANGELOG.md", commit: "c1", before: 1, transition: 1, after: 2 },
      { glob: "docs/**", commit: "c2", before: 0, transition: 0, after: 0 },
    ]);
  });
});

describe("compare", () => {
  test("per-file counts before and after, zero where a file is absent, shares over PRs read", async () => {
    const before = summarise(await fixture("window.json"), { prefix: "legion/" });
    const after = summarise(await fixture("after.json"), { prefix: "legion/" });
    const comparison = compare(before, after);

    expect(comparison.files).toEqual([
      {
        path: X_CHANGELOG,
        category: "changelog",
        before: { events: 3, prs: 3 },
        after: { events: 2, prs: 2, fix: undefined },
      },
      {
        path: "AGENTS.md",
        category: "agents-doc",
        before: { events: 1, prs: 1 },
        after: { events: 0, prs: 0, fix: undefined },
      },
      {
        path: "README.md",
        category: "doc",
        before: { events: 0, prs: 0 },
        after: { events: 1, prs: 1, fix: undefined },
      },
      {
        path: Y_CHANGELOG,
        category: "changelog",
        before: { events: 1, prs: 1 },
        after: { events: 0, prs: 0, fix: undefined },
      },
      {
        path: "src/a.ts",
        category: "code",
        before: { events: 1, prs: 2 },
        after: { events: 0, prs: 0, fix: undefined },
      },
    ]);

    expect(comparison.after.prsRead).toBe(4);
    expect(comparison.after.carrying).toBe(3);
    expect(comparison.after.carryingShare).toBe(3 / 4);
    expect(comparison.after.conflictingPerPr).toBe(3 / 4);
    expect(comparison.before.carryingShare).toBe(3 / 3);
    expect(comparison.before.conflictingPerPr).toBe(4 / 3);
    expect(comparison.after.tiers.map(({ events, share }) => [events, share])).toEqual([
      [2, 2 / 3],
      [2, 2 / 3],
      [2, 2 / 3],
      [3, 1],
    ]);
    expect(comparison.fixes).toBe(false);
  });

  test("a window's length runs to measured_at when it is before until", async () => {
    const before = summarise(await fixture("window.json"), { prefix: "legion/" });
    const after = summarise(await fixture("after.json"), { prefix: "legion/" });
    const { before: was, after: now } = compare(before, after);
    expect(was.window.days).toBe((7 * 86_400 - 1) / 86_400);
    expect(now.window.days).toBe(7);
    expect(now.window.end).toBe(Date.parse("2026-11-03T23:59:59Z"));
  });
});

describe("resolveWindow", () => {
  test("a date until runs through 23:59:59Z and an instant is exact", () => {
    expect(resolveWindow("2026-10-20", "2026-10-27")).toEqual({
      since: Date.parse("2026-10-20T00:00:00Z"),
      until: Date.parse("2026-10-27T23:59:59Z"),
    });
    expect(resolveWindow("2026-10-27T11:21:00Z", "2026-11-03T11:21:00+02:00")).toEqual({
      since: Date.parse("2026-10-27T11:21:00Z"),
      until: Date.parse("2026-11-03T09:21:00Z"),
    });
  });

  test("a malformed bound or since after until is refused", () => {
    for (const [since, until] of [
      ["2026-10-2", "2026-10-27"],
      ["2026-10-20", "2026-02-30"],
      ["yesterday", "2026-10-27"],
      ["2026-10-20", "2026-10-27T25:00:00Z"],
      ["2026-10-28", "2026-10-27"],
      ["2026-10-27T12:00:00Z", "2026-10-27T11:59:59Z"],
    ]) {
      expect(() => resolveWindow(since, until)).toThrow(UsageError);
    }
  });
});

describe("readRawRun", () => {
  test("refuses JSON without prs and text that is not JSON", () => {
    const run = { since: "2026-10-20", until: "2026-10-27", origin_main: "abc" };
    expect(() => readRawRun(JSON.stringify(run))).toThrow(UsageError);
    expect(() => readRawRun(JSON.stringify({ ...run, prs: [{ number: 1 }] }))).toThrow(UsageError);
    expect(() => readRawRun("not json")).toThrow(UsageError);
    expect(readRawRun(JSON.stringify({ ...run, prs: [] })).prs).toEqual([]);
  });
});
