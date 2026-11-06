import {
  type FixPair,
  type FixSpec,
  type FixState,
  type FixStates,
  type MergeKind,
  type MergeRecord,
  type PullRequestRecord,
  type RawRun,
  UsageError,
} from "./types.ts";

const VERSION_LOGS = [
  "packages/daemon/internal/api/version.go",
  "packages/contracts/fixtures/daemon-api/version.json",
];
const LOCKFILES = [
  "bun.lock",
  "bun.lockb",
  "package-lock.json",
  "go.sum",
  "uv.lock",
  "pnpm-lock.yaml",
];

/** The kind of file a conflicted path is: Python's categories, plus changie's `.changes/`. */
export function category(path: string): string {
  const base = path.slice(path.lastIndexOf("/") + 1);
  if (base === "CHANGELOG.md" || `/${path}`.includes("/.changes/")) return "changelog";
  if (base === "AGENTS.md" || base === "CLAUDE.md") return "agents-doc";
  if (
    path.startsWith("packages/claude-envoy/dist/") ||
    path.startsWith("packages/claude-envoy/legion/")
  ) {
    return "generated";
  }
  if (VERSION_LOGS.includes(path)) return "version-log";
  if (path.includes("/migrations/")) return "migration";
  if (LOCKFILES.includes(base)) return "lockfile";
  if (path.endsWith(".md") || path.startsWith("docs/")) return "doc";
  return "code";
}

/** A conflict "would not have happened without the shared files" when every path is in a tier. */
export const TIERS: ReadonlyArray<{ name: string; categories: readonly string[] }> = [
  { name: "changelog only", categories: ["changelog"] },
  {
    name: "changelog + AGENTS/CLAUDE.md + generated",
    categories: ["changelog", "agents-doc", "generated"],
  },
  {
    name: "... + daemon API version log",
    categories: ["changelog", "agents-doc", "generated", "version-log"],
  },
  {
    name: "... + any other doc (.md, docs/)",
    categories: ["changelog", "agents-doc", "generated", "version-log", "doc"],
  },
];

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const INSTANT =
  /^(\d{4}-\d{2}-\d{2})T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$/i;

function parseBound(flag: string, value: string, dayTime: string): number {
  const day = DATE.test(value) ? value : INSTANT.exec(value)?.[1];
  const instant = DATE.test(value) ? `${value}T${dayTime}Z` : value;
  const ms = day === undefined ? Number.NaN : Date.parse(instant);
  // Date.parse rolls 2026-02-30 over into March and gives NaN for month 13; a real calendar day
  // survives the round trip.
  const midnight = new Date(`${day}T00:00:00Z`);
  const real =
    day !== undefined &&
    !Number.isNaN(midnight.getTime()) &&
    midnight.toISOString().startsWith(day);
  if (Number.isNaN(ms) || !real) {
    throw new UsageError(`${flag} must be YYYY-MM-DD or an RFC 3339 instant, got "${value}"`);
  }
  return ms;
}

/** The inclusive window in epoch ms: a date since starts at 00:00:00Z, a date until ends at
 * 23:59:59Z, an instant is taken exactly. */
export function resolveWindow(since: string, until: string): { since: number; until: number } {
  const window = {
    since: parseBound("--since", since, "00:00:00"),
    until: parseBound("--until", until, "23:59:59"),
  };
  if (window.since > window.until) {
    throw new UsageError(`--since ${since} is after --until ${until}`);
  }
  return window;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Parses a run's raw records (the Python run's file included); refuses anything else. */
export function readRawRun(text: string): RawRun {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch (error) {
    throw new UsageError(`raw records are not JSON: ${(error as Error).message}`);
  }
  if (!isObject(data)) throw new UsageError("raw records are not a JSON object");
  for (const key of ["since", "until", "origin_main"]) {
    if (typeof data[key] !== "string") {
      throw new UsageError(`raw records: "${key}" is missing or not a string`);
    }
  }
  if (!Array.isArray(data.prs)) {
    throw new UsageError(`raw records: "prs" is missing or not an array`);
  }
  data.prs.forEach((pr: unknown, index) => {
    if (
      !isObject(pr) ||
      typeof pr.number !== "number" ||
      typeof pr.headRefName !== "string" ||
      !["OPEN", "MERGED", "CLOSED"].includes(pr.state as string) ||
      !Array.isArray(pr.merges) ||
      !Array.isArray(pr.force_pushes)
    ) {
      throw new UsageError(
        `raw records: prs[${index}] needs a number, headRefName, state, merges and force_pushes`
      );
    }
    if (typeof pr.createdAt !== "string") {
      throw new UsageError(`raw records: prs[${index}].createdAt is missing or not a string`);
    }
    if (pr.mergedAt !== null && typeof pr.mergedAt !== "string") {
      throw new UsageError(
        `raw records: prs[${index}].mergedAt is missing or not a string or null`
      );
    }
    if ("closedAt" in pr && pr.closedAt !== null && typeof pr.closedAt !== "string") {
      throw new UsageError(`raw records: prs[${index}].closedAt is not a string or null`);
    }
  });
  if ("measured_at" in data && typeof data.measured_at !== "string") {
    throw new UsageError(`raw records: "measured_at" is not a string`);
  }
  if ("selection" in data && data.selection !== "updated-since") {
    throw new UsageError(`raw records: unknown "selection" ${JSON.stringify(data.selection)}`);
  }
  return data as unknown as RawRun;
}

export interface Tier {
  name: string;
  hit: number;
  total: number;
  prs: number;
}

export interface CategoryRow {
  category: string;
  events: number;
  prs: number;
}

export type FixCounts = Record<FixState, number>;

export interface FileRow {
  path: string;
  category: string;
  events: number;
  prs: number[]; // sorted
  fix?: FixCounts; // when a --fixed spec matches the path
}

export interface Tables {
  events: number;
  categories: CategoryRow[]; // events descending, ties by first appearance
  files: FileRow[]; // events descending, then path
}

export interface MergeSection {
  prs: number;
  carryingForward: number;
  forward: number;
  conflicting: number;
  conflictingPrs: number;
  clean: number;
  cleanPrs: number;
  undeterminable: number;
  tiers: Tier[];
  otherBranch: number;
  internal: number;
  perPr: Array<{ pr: number; conflicting: number }>; // sorted by PR
  tables: Tables;
}

export interface ForcePushSection {
  count: number;
  prs: number;
  conflicting: number;
  conflictingPrs: number;
  undeterminable: number;
  errors: Array<{ error: string | null; count: number }>; // first seen first
  tiers: Tier[];
  tables: Tables;
}

export interface SnapshotSection {
  open: number;
  conflicting: number;
  tiers: Tier[];
  tables: Tables;
  rows: Array<{ pr: number; base: string; conflicts: string[] }>; // sorted by PR
}

/** A `--fixed` spec's totals over the combined section's conflicting forward merges with a
 * conflicted path the spec matches (first matching spec wins per path); a merge counts once. */
export interface FixTotal {
  glob: string;
  commit: string;
  merges: number;
  prs: number; // distinct PRs carrying those merges
  states: FixCounts | null; // each merge once, in its state; null without fix states
}

export interface Summary {
  window: { since: string; until: string; start: number; end: number; measuredAt: string | null };
  originMain: string;
  prefix: string;
  /** Every PR read, merges deduplicated across all of them; the section the comparison uses. */
  combined: MergeSection;
  /** Merged in the window (`state` MERGED for the Python run). */
  merged: MergeSection;
  /** Every other PR read, merges the merged section saw excluded. */
  other: MergeSection;
  forcePushes: ForcePushSection;
  snapshot: SnapshotSection;
  fixes: FixTotal[] | null;
  /** The (merge, glob) pairs the totals count; fix states are found for these. */
  fixPairs: FixPair[];
}

export interface SummaryOptions {
  prefix: string;
  /** The totals carry no states and the file rows no `fix`; `applyFixStates` adds them. */
  fixes?: FixSpec[];
}

/** A counted thing with conflicted paths: a merge, a force push or an open head. */
interface Conflicted {
  conflicts: string[];
  prs: ReadonlySet<number>;
}

interface MergeEvent {
  oid: string;
  kind: MergeKind;
  conflicts: MergeRecord["conflicts"];
  record: MergeRecord; // the first PR's
  prs: Set<number>;
}

function prsOf(events: Iterable<{ prs: ReadonlySet<number> }>): Set<number> {
  const prs = new Set<number>();
  for (const event of events) for (const pr of event.prs) prs.add(pr);
  return prs;
}

function tiers(events: Conflicted[]): Tier[] {
  return TIERS.map(({ name, categories }) => {
    const hit = events.filter((event) =>
      event.conflicts.every((path) => categories.includes(category(path)))
    );
    return { name, hit: hit.length, total: events.length, prs: prsOf(hit).size };
  });
}

const byPath = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

function tables(events: Conflicted[]): Tables {
  const files = new Map<string, { events: number; prs: Set<number> }>();
  const categories = new Map<string, { events: number; prs: Set<number> }>();
  for (const event of events) {
    const touched = new Set<string>();
    for (const path of event.conflicts) {
      const file = files.get(path) ?? { events: 0, prs: new Set() };
      file.events++;
      for (const pr of event.prs) file.prs.add(pr);
      files.set(path, file);
      touched.add(category(path));
    }
    for (const name of touched) {
      const row = categories.get(name) ?? { events: 0, prs: new Set() };
      row.events++;
      for (const pr of event.prs) row.prs.add(pr);
      categories.set(name, row);
    }
  }
  return {
    events: events.length,
    categories: [...categories]
      .map(([name, row]) => ({ category: name, events: row.events, prs: row.prs.size }))
      .sort((a, b) => b.events - a.events),
    files: [...files]
      .map(([path, row]) => ({
        path,
        category: category(path),
        events: row.events,
        prs: [...row.prs].sort((a, b) => a - b),
      }))
      .sort((a, b) => b.events - a.events || byPath(a.path, b.path)),
  };
}

function uniqueMerges(
  prs: PullRequestRecord[],
  inWindow: (when: string) => boolean,
  exclude: ReadonlySet<string> = new Set()
): Map<string, MergeEvent> {
  const events = new Map<string, MergeEvent>();
  for (const pr of prs) {
    for (const record of pr.merges) {
      const { oid, kind, conflicts, date } = record;
      if (exclude.has(oid) || !inWindow(date)) continue;
      const event = events.get(oid) ?? { oid, kind, conflicts, record, prs: new Set<number>() };
      event.prs.add(pr.number);
      events.set(oid, event);
    }
  }
  return events;
}

function mergeSection(prCount: number, events: Map<string, MergeEvent>): MergeSection {
  const all = [...events.values()];
  const forward = all.filter((event) => event.kind === "forward");
  const conflicting = conflictingForward(all);
  const clean = forward.filter((event) => event.conflicts?.length === 0);
  const perPr = new Map<number, number>();
  for (const event of conflicting) {
    for (const pr of event.prs) perPr.set(pr, (perPr.get(pr) ?? 0) + 1);
  }
  return {
    prs: prCount,
    carryingForward: prsOf(forward).size,
    forward: forward.length,
    conflicting: conflicting.length,
    conflictingPrs: prsOf(conflicting).size,
    clean: clean.length,
    cleanPrs: prsOf(clean).size,
    undeterminable: forward.filter((event) => event.conflicts == null).length,
    tiers: tiers(conflicting),
    otherBranch: all.filter((event) => event.kind === "other-branch").length,
    internal: all.filter((event) => event.kind === "internal").length,
    perPr: [...perPr]
      .map(([pr, count]) => ({ pr, conflicting: count }))
      .sort((a, b) => a.pr - b.pr),
    tables: tables(conflicting),
  };
}

function conflictingForward(events: MergeEvent[]): Array<MergeEvent & Conflicted> {
  return events.filter(
    (event): event is MergeEvent & Conflicted =>
      event.kind === "forward" && !!event.conflicts?.length
  );
}

const noFixes = (): FixCounts => ({ before: 0, transition: 0, after: 0 });

/** The spec a path falls under: the first whose glob matches it, or undefined. */
function fixMatcher(specs: FixSpec[]): (path: string) => FixSpec | undefined {
  const globs = specs.map((spec) => ({ spec, glob: new Bun.Glob(spec.glob) }));
  return (path) => globs.find(({ glob }) => glob.match(path))?.spec;
}

/** Totals each spec's matching merges, without states, and gives the (merge, glob) pairs it
 * counted; a path counts for the first spec matching it, a merge once per spec. */
function countFixes(
  events: Map<string, MergeEvent>,
  specs: FixSpec[]
): { totals: FixTotal[]; pairs: FixPair[] } {
  const match = fixMatcher(specs);
  const merges = new Map(specs.map((spec) => [spec, [] as MergeEvent[]]));
  const pairs: FixPair[] = [];
  for (const event of conflictingForward([...events.values()])) {
    const matched = new Map<FixSpec, string[]>();
    for (const path of event.conflicts) {
      const spec = match(path);
      if (!spec) continue;
      const paths = matched.get(spec);
      if (paths) paths.push(path);
      else matched.set(spec, [path]);
    }
    const { branch_parent, upstream_parent } = event.record;
    for (const [spec, paths] of matched) {
      (merges.get(spec) as MergeEvent[]).push(event);
      pairs.push({
        oid: event.oid,
        glob: spec.glob,
        commit: spec.commit,
        branch_parent,
        upstream_parent,
        paths,
      });
    }
  }
  const totals = [...merges].map(([{ glob, commit }, matching]) => ({
    glob,
    commit,
    merges: matching.length,
    prs: prsOf(matching).size,
    states: null,
  }));
  return { totals, pairs };
}

/**
 * The summary with the fix states of its pairs: each spec's totals count each of its merges once,
 * in its state, and each by-file row a spec takes counts each merge on it, in its state.
 */
export function applyFixStates(summary: Summary, states: FixStates): Summary {
  const totals = new Map<string, FixCounts>();
  const byFile = new Map<string, FixCounts>();
  for (const { oid, glob, paths } of summary.fixPairs) {
    const state = states.get(oid)?.get(glob);
    if (state === undefined) throw new Error(`no fix state for ${glob} in merge ${oid}`);
    const total = totals.get(glob) ?? noFixes();
    total[state]++;
    totals.set(glob, total);
    for (const path of paths) {
      const fix = byFile.get(path) ?? noFixes();
      fix[state]++;
      byFile.set(path, fix);
    }
  }
  const { combined } = summary;
  const files = combined.tables.files.map((row) => {
    const fix = byFile.get(row.path);
    return fix ? { ...row, fix } : row;
  });
  return {
    ...summary,
    combined: { ...combined, tables: { ...combined.tables, files } },
    // A repeated glob's later spec matches nothing: the first takes every path it matches.
    fixes:
      summary.fixes?.map((fix) => {
        const counts = totals.get(fix.glob) ?? noFixes();
        totals.delete(fix.glob);
        return { ...fix, states: counts };
      }) ?? null,
  };
}

function forcePushSection(
  prs: PullRequestRecord[],
  inWindow: (when: string) => boolean
): ForcePushSection {
  const pushes = prs.flatMap((pr) =>
    pr.force_pushes
      .filter((push) => inWindow(push.date))
      .map(({ conflicts, error }) => ({ conflicts, error, prs: new Set([pr.number]) }))
  );
  const conflicting = pushes.filter(
    (push): push is typeof push & Conflicted => !!push.conflicts?.length
  );
  const unknown = pushes.filter((push) => push.conflicts == null);
  const errors = new Map<string | null, number>();
  for (const { error = null } of unknown) errors.set(error, (errors.get(error) ?? 0) + 1);
  return {
    count: pushes.length,
    prs: prsOf(pushes).size,
    conflicting: conflicting.length,
    conflictingPrs: prsOf(conflicting).size,
    undeterminable: unknown.length,
    errors: [...errors].map(([error, count]) => ({ error, count })),
    tiers: tiers(conflicting),
    tables: tables(conflicting),
  };
}

function snapshotSection(open: PullRequestRecord[]): SnapshotSection {
  const conflicting = open
    .filter((pr) => !!pr.open_conflicts_now?.length)
    .map((pr) => ({
      pr: pr.number,
      base: pr.baseRefName,
      conflicts: pr.open_conflicts_now as string[],
      prs: new Set([pr.number]),
    }));
  return {
    open: open.length,
    conflicting: conflicting.length,
    tiers: tiers(conflicting),
    tables: tables(conflicting),
    rows: conflicting
      .map(({ pr, base, conflicts }) => ({ pr, base, conflicts }))
      .sort((a, b) => a.pr - b.pr),
  };
}

/** Whether an RFC 3339 time falls inside the run's window, bounds included. */
function windowTest(window: { since: number; until: number }): (when: string) => boolean {
  return (when) => {
    const at = Date.parse(when);
    return window.since <= at && at <= window.until;
  };
}

/**
 * Whether a PR existed in the window: opened by its end and neither merged nor closed before its
 * start, or carrying a merge or force push dated inside it (a PR opened later from a branch that
 * merged main inside the window).
 */
function existedInWindow(
  pr: PullRequestRecord,
  window: { since: number; until: number },
  inWindow: (when: string) => boolean
): boolean {
  const { mergedAt, closedAt } = pr;
  return (
    (Date.parse(pr.createdAt) <= window.until &&
      !(mergedAt !== null && Date.parse(mergedAt) < window.since) &&
      !(pr.state === "CLOSED" && closedAt != null && Date.parse(closedAt) < window.since)) ||
    pr.merges.some((merge) => inWindow(merge.date)) ||
    pr.force_pushes.some((push) => inWindow(push.date))
  );
}

/**
 * The PRs a run counts: those whose head branch starts with `prefix`, narrowed for a run of this
 * script (one with a `selection`) to those that existed in the window, since its list runs to
 * whenever it was made. The Python run's records are taken as they are.
 */
function prsRead(
  run: RawRun,
  prefix: string,
  window: { since: number; until: number },
  inWindow: (when: string) => boolean
): PullRequestRecord[] {
  const prs = run.prs.filter((pr) => pr.headRefName.startsWith(prefix));
  return run.selection === undefined
    ? prs
    : prs.filter((pr) => existedInWindow(pr, window, inWindow));
}

/**
 * Python's `summarise` as numbers: events are unique merge commits, a merge carried by several
 * PRs counts once and is attributed to each; only merges and force pushes dated inside the
 * window count, and only the PRs `prsRead` gives.
 */
export function summarise(run: RawRun, options: SummaryOptions): Summary {
  const window = resolveWindow(run.since, run.until);
  const inWindow = windowTest(window);
  const prs = prsRead(run, options.prefix, window, inWindow);
  const isMerged =
    run.selection === undefined
      ? (pr: PullRequestRecord) => pr.state === "MERGED"
      : (pr: PullRequestRecord) => pr.mergedAt !== null && inWindow(pr.mergedAt);
  const merged = prs.filter(isMerged);
  const other = prs.filter((pr) => !isMerged(pr));

  const allEvents = uniqueMerges(prs, inWindow);
  const mergedEvents = uniqueMerges(merged, inWindow);
  const fixes = options.fixes ? countFixes(allEvents, options.fixes) : null;
  return {
    window: {
      since: run.since,
      until: run.until,
      start: window.since,
      end: window.until,
      measuredAt: run.measured_at ?? null,
    },
    originMain: run.origin_main,
    prefix: options.prefix,
    combined: mergeSection(prs.length, allEvents),
    merged: mergeSection(merged.length, mergedEvents),
    other: mergeSection(other.length, uniqueMerges(other, inWindow, new Set(mergedEvents.keys()))),
    forcePushes: forcePushSection([...merged, ...other], inWindow),
    snapshot: snapshotSection(prs.filter((pr) => pr.state === "OPEN")),
    fixes: fixes?.totals ?? null,
    fixPairs: fixes?.pairs ?? [],
  };
}

export interface ComparisonWindow {
  since: string;
  until: string;
  start: number;
  end: number;
  /** From start to `min(end, measured_at)`. */
  days: number;
}

export interface ComparisonSide {
  window: ComparisonWindow;
  prsRead: number;
  carrying: number;
  carryingShare: number; // of PRs read
  conflicting: number;
  conflictingPerPr: number; // per PR read
  tiers: Array<{ name: string; events: number; share: number }>; // share of conflicting merges
}

export interface ComparisonFile {
  path: string;
  category: string;
  before: { events: number; prs: number };
  after: { events: number; prs: number; fix?: FixCounts };
}

/** A `--fixed` glob's matching merges and the PRs carrying them. */
export interface ComparisonGlob {
  glob: string;
  before: { merges: number; prs: number; prsShare: number }; // prsShare: of PRs read
  after: { merges: number; prs: number; prsShare: number; states: FixCounts | null };
}

export interface Comparison {
  before: ComparisonSide;
  after: ComparisonSide;
  /** True when the after summary carries fix states. */
  fixes: boolean;
  files: ComparisonFile[]; // events before + after descending, then path
  globs: ComparisonGlob[]; // the after summary's specs, in order; none without them
}

const share = (count: number, of: number) => (of === 0 ? 0 : count / of);
const DAY_MS = 86_400_000;

function side(summary: Summary): ComparisonSide {
  const { window, combined } = summary;
  const end =
    window.measuredAt === null ? window.end : Math.min(window.end, Date.parse(window.measuredAt));
  return {
    window: {
      since: window.since,
      until: window.until,
      start: window.start,
      end: window.end,
      days: (end - window.start) / DAY_MS,
    },
    prsRead: combined.prs,
    carrying: combined.conflictingPrs,
    carryingShare: share(combined.conflictingPrs, combined.prs),
    conflicting: combined.conflicting,
    conflictingPerPr: share(combined.conflicting, combined.prs),
    tiers: combined.tiers.map((tier) => ({
      name: tier.name,
      events: tier.hit,
      share: share(tier.hit, tier.total),
    })),
  };
}

/** Before → after over each summary's combined section. */
export function compare(before: Summary, after: Summary): Comparison {
  const beforeRows = new Map(before.combined.tables.files.map((row) => [row.path, row]));
  const afterRows = new Map(after.combined.tables.files.map((row) => [row.path, row]));
  const paths = new Set([...beforeRows.keys(), ...afterRows.keys()]);
  const files = [...paths].map((path): ComparisonFile => {
    const was = beforeRows.get(path);
    const now = afterRows.get(path);
    return {
      path,
      category: ((now ?? was) as FileRow).category,
      before: { events: was?.events ?? 0, prs: was?.prs.length ?? 0 },
      after: { events: now?.events ?? 0, prs: now?.prs.length ?? 0, fix: now?.fix },
    };
  });
  files.sort(
    (a, b) =>
      b.before.events + b.after.events - (a.before.events + a.after.events) ||
      byPath(a.path, b.path)
  );
  const globs = (after.fixes ?? []).map((now): ComparisonGlob => {
    const was = before.fixes?.find((fix) => fix.glob === now.glob);
    const merges = was?.merges ?? 0;
    const prs = was?.prs ?? 0;
    return {
      glob: now.glob,
      before: { merges, prs, prsShare: share(prs, before.combined.prs) },
      after: {
        merges: now.merges,
        prs: now.prs,
        prsShare: share(now.prs, after.combined.prs),
        states: now.states,
      },
    };
  });
  return {
    before: side(before),
    after: side(after),
    fixes: after.fixes?.some((fix) => fix.states !== null) ?? false,
    files,
    globs,
  };
}
