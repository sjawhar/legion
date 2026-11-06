import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import {
  applyFixStates,
  category,
  compare,
  readRawRun,
  resolveWindow,
  summarise,
} from "../summary.ts";
import {
  type FixState,
  type MergeRecord,
  type PullRequestRecord,
  type RawRun,
  UsageError,
} from "../types.ts";

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

  describe("a run with a selection reads only the PRs that existed in the window", () => {
    // The window is 2026-10-20T00:00:00Z to 2026-10-26T23:59:59Z.
    const forward = (oid: string, date: string): MergeRecord => ({
      oid,
      date,
      headline: "Merge main",
      parents: [`${oid}-branch`, `${oid}-main`],
      kind: "forward",
      branch_parent: `${oid}-branch`,
      upstream_parent: `${oid}-main`,
      conflicts: [X_CHANGELOG],
    });
    const pr = (number: number, fields: Partial<PullRequestRecord>): PullRequestRecord => ({
      number,
      title: `PR ${number}`,
      state: "OPEN",
      baseRefName: "main",
      headRefName: `legion/D-${number}`,
      headRefOid: `h${number}`,
      mergedAt: null,
      createdAt: "2026-10-21T00:00:00Z",
      commit_count: 0,
      merges: [],
      force_pushes: [],
      ...fields,
    });
    const late = "2026-10-27T09:00:00Z";
    const early = "2026-10-10T00:00:00Z";
    const cases: Array<{ pr: PullRequestRecord; read: boolean }> = [
      // Opened after the window with no merge in it, conflicting with main now.
      {
        pr: pr(40, {
          createdAt: late,
          merges: [forward("p40", "2026-10-27T10:00:00Z")],
          open_conflicts_now: [X_CHANGELOG],
        }),
        read: false,
      },
      // Opened after the window from a branch that merged main inside it.
      {
        pr: pr(41, { createdAt: late, merges: [forward("p41", "2026-10-25T08:16:00Z")] }),
        read: true,
      },
      // Opened after the window, force-pushed inside it.
      {
        pr: pr(42, {
          createdAt: late,
          force_pushes: [{ date: "2026-10-26T00:00:00Z", before: "a", after: "b", conflicts: [] }],
        }),
        read: true,
      },
      // Merged before the window.
      {
        pr: pr(43, { state: "MERGED", createdAt: early, mergedAt: "2026-10-19T23:59:59Z" }),
        read: false,
      },
      // Closed unmerged before the window.
      {
        pr: pr(44, { state: "CLOSED", createdAt: early, closedAt: "2026-10-19T23:59:59Z" }),
        read: false,
      },
      // Closed unmerged, no closedAt (an earlier run's record).
      { pr: pr(45, { state: "CLOSED", createdAt: early }), read: true },
      // Closed unmerged inside the window.
      {
        pr: pr(46, { state: "CLOSED", createdAt: early, closedAt: "2026-10-22T00:00:00Z" }),
        read: true,
      },
      // Opened at the window's last second, open now.
      { pr: pr(47, { createdAt: "2026-10-26T23:59:59Z" }), read: true },
      // Merged inside the window.
      {
        pr: pr(48, { state: "MERGED", createdAt: early, mergedAt: "2026-10-22T00:00:00Z" }),
        read: true,
      },
    ];
    const runOf = (prs: PullRequestRecord[], selection?: "updated-since"): RawRun => ({
      since: "2026-10-20",
      until: "2026-10-26",
      origin_main: "o",
      prs,
      ...(selection ? { measured_at: "2026-10-30T00:00:00Z", selection } : {}),
    });

    test("each PR is read or not by when it was opened, merged, closed and pushed", () => {
      const read = cases.map(
        ({ pr }) =>
          summarise(runOf([pr], "updated-since"), { prefix: "legion/" }).combined.prs === 1
      );
      expect(read).toEqual(cases.map((c) => c.read));
    });

    test("every section counts only the PRs read", () => {
      const summary = summarise(
        runOf(
          cases.map((c) => c.pr),
          "updated-since"
        ),
        {
          prefix: "legion/",
        }
      );
      expect(summary.combined.prs).toBe(6);
      expect(summary.merged.prs).toBe(1);
      expect(summary.other.prs).toBe(5);
      expect(summary.combined.perPr).toEqual([{ pr: 41, conflicting: 1 }]);
      expect(summary.forcePushes.count).toBe(1);
      expect(summary.snapshot.open).toBe(3);
      expect(summary.snapshot.rows).toEqual([]);
    });

    test("a PR opened before the window, open now and untouched in it, is read", () => {
      const summary = summarise(runOf([pr(49, { createdAt: early })], "updated-since"), {
        prefix: "legion/",
      });
      expect(summary.combined.prs).toBe(1);
      expect(summary.merged.prs).toBe(0);
      expect(summary.other.prs).toBe(1);
      expect(summary.snapshot.open).toBe(1);
    });

    test("a run without a selection (the Python run) reads every PR", () => {
      const summary = summarise(runOf(cases.map((c) => c.pr)), { prefix: "legion/" });
      expect(summary.combined.prs).toBe(cases.length);
      expect(summary.merged.prs).toBe(2);
      expect(summary.other.prs).toBe(cases.length - 2);
      expect(summary.snapshot.open).toBe(4);
      expect(summary.snapshot.rows).toEqual([{ pr: 40, base: "main", conflicts: [X_CHANGELOG] }]);
    });
  });

  test("fix states give per-file before/transition/after and zeros for an unmatched spec", async () => {
    const states = new Map<string, Map<string, FixState>>([
      ["m1", new Map([["packages/*/CHANGELOG.md", "before"]])],
      ["m2", new Map([["packages/*/CHANGELOG.md", "transition"]])],
      ["m5", new Map([["packages/*/CHANGELOG.md", "after"]])],
      ["m7", new Map([["packages/*/CHANGELOG.md", "after"]])],
    ]);
    const specs = [
      { glob: "packages/*/CHANGELOG.md", commit: "c1" },
      { glob: "docs/**", commit: "c2" },
    ];
    const summary = applyFixStates(
      summarise(await fixture("window.json"), { prefix: "legion/", fixes: specs }),
      states
    );
    const fixOf = (path: string) =>
      summary.combined.tables.files.find((row) => row.path === path)?.fix;
    expect(fixOf(X_CHANGELOG)).toEqual({ before: 1, transition: 1, after: 1 });
    expect(fixOf(Y_CHANGELOG)).toEqual({ before: 0, transition: 0, after: 1 });
    expect(fixOf("AGENTS.md")).toBeUndefined();
    expect(summary.fixes).toEqual([
      {
        glob: "packages/*/CHANGELOG.md",
        commit: "c1",
        merges: 4,
        prs: 3,
        states: { before: 1, transition: 1, after: 2 },
      },
      {
        glob: "docs/**",
        commit: "c2",
        merges: 0,
        prs: 0,
        states: { before: 0, transition: 0, after: 0 },
      },
    ]);
  });

  describe("a directory glob", () => {
    // k1 conflicts on two files under gen/ and is carried by both PRs; k2 on one of them.
    const merge = (oid: string, date: string, conflicts: string[]): MergeRecord => ({
      oid,
      date,
      headline: "Merge main",
      parents: [`${oid}-branch`, `${oid}-main`],
      kind: "forward",
      branch_parent: `${oid}-branch`,
      upstream_parent: `${oid}-main`,
      conflicts,
    });
    const pr = (number: number, merges: MergeRecord[]): PullRequestRecord => ({
      number,
      title: `PR ${number}`,
      state: "OPEN",
      baseRefName: "main",
      headRefName: `legion/C-${number}`,
      headRefOid: `h${number}`,
      mergedAt: null,
      createdAt: "2026-10-20T00:00:00Z",
      commit_count: merges.length,
      merges,
      force_pushes: [],
    });
    const k1 = merge("k1", "2026-10-21T00:00:00Z", ["gen/a.js", "gen/b.js"]);
    const k2 = merge("k2", "2026-10-22T00:00:00Z", ["gen/a.js", "src/c.ts"]);
    const k3 = merge("k3", "2026-10-23T00:00:00Z", ["src/c.ts"]);
    const run: RawRun = {
      since: "2026-10-20",
      until: "2026-10-26",
      origin_main: "o",
      prs: [pr(30, [k1, k2]), pr(31, [k1, k3])],
    };
    const specs = [{ glob: "gen/**", commit: "c" }];

    test("counts a merge on two matched files once, with its state, and PRs distinct", () => {
      const states = new Map<string, Map<string, FixState>>([
        ["k1", new Map<string, FixState>([["gen/**", "after"]])],
        ["k2", new Map<string, FixState>([["gen/**", "before"]])],
      ]);
      const summary = applyFixStates(summarise(run, { prefix: "legion/", fixes: specs }), states);
      expect(summary.fixes).toEqual([
        {
          glob: "gen/**",
          commit: "c",
          merges: 2,
          prs: 2,
          states: { before: 1, transition: 0, after: 1 },
        },
      ]);
      const fixOf = (path: string) =>
        summary.combined.tables.files.find((row) => row.path === path)?.fix;
      expect(fixOf("gen/a.js")).toEqual({ before: 1, transition: 0, after: 1 });
      expect(fixOf("gen/b.js")).toEqual({ before: 0, transition: 0, after: 1 });
      expect(fixOf("src/c.ts")).toBeUndefined();
    });

    test("without states the totals have no states and the file rows no fix", () => {
      const summary = summarise(run, { prefix: "legion/", fixes: specs });
      expect(summary.fixes).toEqual([
        { glob: "gen/**", commit: "c", merges: 2, prs: 2, states: null },
      ]);
      expect(summary.combined.tables.files.every((row) => row.fix === undefined)).toBe(true);
      expect(summary.fixPairs).toEqual([
        {
          oid: "k1",
          glob: "gen/**",
          commit: "c",
          branch_parent: "k1-branch",
          upstream_parent: "k1-main",
          paths: ["gen/a.js", "gen/b.js"],
        },
        {
          oid: "k2",
          glob: "gen/**",
          commit: "c",
          branch_parent: "k2-branch",
          upstream_parent: "k2-main",
          paths: ["gen/a.js"],
        },
      ]);
    });

    test("takes every path a narrower later glob matches, leaving that glob at zero", () => {
      const narrower = [...specs, { glob: "gen/a.js", commit: "c2" }];
      const states = new Map<string, Map<string, FixState>>([
        ["k1", new Map<string, FixState>([["gen/**", "after"]])],
        ["k2", new Map<string, FixState>([["gen/**", "before"]])],
      ]);
      const counted = summarise(run, { prefix: "legion/", fixes: narrower });
      expect(counted.fixPairs.map(({ oid, glob }) => [oid, glob])).toEqual([
        ["k1", "gen/**"],
        ["k2", "gen/**"],
      ]);
      expect(applyFixStates(counted, states).fixes).toEqual([
        {
          glob: "gen/**",
          commit: "c",
          merges: 2,
          prs: 2,
          states: { before: 1, transition: 0, after: 1 },
        },
        {
          glob: "gen/a.js",
          commit: "c2",
          merges: 0,
          prs: 0,
          states: { before: 0, transition: 0, after: 0 },
        },
      ]);
    });
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
    expect(comparison.globs).toEqual([]);
  });

  test("glob rows give before and after totals, zeros where a glob matches nothing", async () => {
    const specs = [
      { glob: "packages/*/CHANGELOG.md", commit: "c1" },
      { glob: "src/**", commit: "c2" },
      { glob: "README.md", commit: "c3" },
    ];
    const before = summarise(await fixture("window.json"), { prefix: "legion/", fixes: specs });
    const states = new Map<string, Map<string, FixState>>([
      ["n1", new Map<string, FixState>([["packages/*/CHANGELOG.md", "after"]])],
      ["n2", new Map<string, FixState>([["README.md", "before"]])],
      ["n4", new Map<string, FixState>([["packages/*/CHANGELOG.md", "transition"]])],
    ]);
    const after = applyFixStates(
      summarise(await fixture("after.json"), { prefix: "legion/", fixes: specs }),
      states
    );
    const comparison = compare(before, after);
    expect(comparison.fixes).toBe(true);
    expect(comparison.globs).toEqual([
      {
        glob: "packages/*/CHANGELOG.md",
        before: { merges: 4, prs: 3, prsShare: 3 / 3 },
        after: {
          merges: 2,
          prs: 2,
          prsShare: 2 / 4,
          states: { before: 0, transition: 1, after: 1 },
        },
      },
      {
        glob: "src/**",
        before: { merges: 1, prs: 2, prsShare: 2 / 3 },
        after: {
          merges: 0,
          prs: 0,
          prsShare: 0,
          states: { before: 0, transition: 0, after: 0 },
        },
      },
      {
        glob: "README.md",
        before: { merges: 0, prs: 0, prsShare: 0 },
        after: {
          merges: 1,
          prs: 1,
          prsShare: 1 / 4,
          states: { before: 1, transition: 0, after: 0 },
        },
      },
    ]);
  });

  test("a glob the before summary has no totals for is zero before", async () => {
    const before = summarise(await fixture("window.json"), { prefix: "legion/" });
    const after = summarise(await fixture("after.json"), {
      prefix: "legion/",
      fixes: [{ glob: "packages/*/CHANGELOG.md", commit: "c1" }],
    });
    expect(compare(before, after).globs).toEqual([
      {
        glob: "packages/*/CHANGELOG.md",
        before: { merges: 0, prs: 0, prsShare: 0 },
        after: { merges: 2, prs: 2, prsShare: 2 / 4, states: null },
      },
    ]);
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
      ["2026-13-01", "2026-10-27"],
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

  describe("a PR record", () => {
    const pr = {
      number: 1,
      headRefName: "legion/A",
      state: "CLOSED",
      createdAt: "2026-10-19T00:00:00Z",
      mergedAt: null,
      merges: [],
      force_pushes: [],
    };
    const read = (fields: object) =>
      readRawRun(
        JSON.stringify({
          since: "2026-10-20",
          until: "2026-10-27",
          origin_main: "abc",
          prs: [{ ...pr, ...fields }],
        })
      ).prs[0];

    test("takes closedAt as a string or null, or absent, and refuses anything else", () => {
      expect(read({ closedAt: "2026-10-21T00:00:00Z" }).closedAt).toBe("2026-10-21T00:00:00Z");
      expect(read({ closedAt: null }).closedAt).toBeNull();
      expect(read({}).closedAt).toBeUndefined();
      expect(() => read({ closedAt: 5 })).toThrow(UsageError);
    });

    test("needs createdAt as a string", () => {
      expect(read({}).createdAt).toBe("2026-10-19T00:00:00Z");
      expect(() => read({ createdAt: undefined })).toThrow(UsageError);
      expect(() => read({ createdAt: null })).toThrow(UsageError);
    });

    test("needs mergedAt as a string or null", () => {
      expect(read({ mergedAt: "2026-10-21T00:00:00Z" }).mergedAt).toBe("2026-10-21T00:00:00Z");
      expect(read({}).mergedAt).toBeNull();
      expect(() => read({ mergedAt: 5 })).toThrow(UsageError);
      expect(() => read({ mergedAt: undefined })).toThrow(UsageError);
    });
  });
});

describe("category", () => {
  test("a file under a .changes/ directory or named CHANGELOG.md is a changelog", () => {
    expect(
      [
        ".changes/unreleased/x.yaml",
        "packages/envoy/.changes/unreleased/x.yaml",
        "packages/x/CHANGELOG.md",
        "packages/x/changes.md",
        "packages/x/changes/y.go",
      ].map(category)
    ).toEqual(["changelog", "changelog", "changelog", "doc", "code"]);
  });
});
