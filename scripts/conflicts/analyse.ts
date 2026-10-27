import { type GitRepo, ORIGIN_MAIN } from "./git.ts";
import {
  type FixSpec,
  type FixState,
  type FixStates,
  type ForcePushRecord,
  fixMatcher,
  type GraphPullRequest,
  type MergeRecord,
  type PullRequestRecord,
  UsageError,
} from "./types.ts";

/** The pull request's merges, force pushes and (when open) its conflicts with its base now. */
export async function analysePullRequest(
  git: GitRepo,
  pr: GraphPullRequest,
  upstreamRef: string
): Promise<PullRequestRecord> {
  const onPr = new Set(pr.commits.map((c) => c.oid));
  const merges: MergeRecord[] = [];
  for (const c of pr.commits) {
    const parents = c.parents.nodes.map((p) => p.oid);
    if (parents.length < 2) continue;
    const commit = { oid: c.oid, date: c.committedDate, headline: c.messageHeadline, parents };
    let branch = parents.filter((p) => onPr.has(p));
    let outside = parents.filter((p) => !onPr.has(p));
    if (branch.length === 0 || outside.length === 0) {
      // Both parents on the PR (merging two PR lines) or none (rare): not a forward merge.
      // When neither is on the PR, treat the parent not on upstream as the branch side.
      if (branch.length === 0 && outside.length >= 2) {
        const up: string[] = [];
        for (const p of outside) if (await git.isAncestor(p, upstreamRef)) up.push(p);
        const br = outside.filter((p) => !up.includes(p));
        if (up.length > 0 && br.length > 0) {
          branch = br;
          outside = up;
        }
      }
      if (branch.length === 0 || outside.length === 0) {
        merges.push({ ...commit, kind: "internal" });
        continue;
      }
    }
    const b = branch[0];
    const u = outside[0];
    const onMain = await git.isAncestor(u, ORIGIN_MAIN);
    const onBase = upstreamRef === ORIGIN_MAIN ? onMain : await git.isAncestor(u, upstreamRef);
    const rec: MergeRecord = {
      ...commit,
      branch_parent: b,
      upstream_parent: u,
      upstream_on_main: onMain,
      upstream_on_base: onBase,
      kind: onMain || onBase ? "forward" : "other-branch",
    };
    if (!((await git.has(b)) && (await git.has(u)))) {
      rec.conflicts = null;
      rec.error = "parent object missing";
    } else {
      rec.conflicts = await git.mergeTree(b, u);
    }
    merges.push(rec);
  }

  const pushes: ForcePushRecord[] = [];
  for (const e of pr.forcePushes) {
    const before = e.beforeCommit?.oid ?? null;
    const push = { date: e.createdAt, before, after: e.afterCommit?.oid ?? null };
    if (pr.baseRefName !== "main") {
      pushes.push({ ...push, conflicts: null, error: "base is not main" });
    } else if (!before || !(await git.fetchOid(before))) {
      pushes.push({ ...push, conflicts: null, error: "pre-push commit unavailable" });
    } else {
      const m = await git.mainAt(e.createdAt);
      pushes.push({
        ...push,
        main_at_push: m,
        already_contains_main: m !== null && (await git.isAncestor(m, before)),
        conflicts: m ? await git.mergeTree(before, m) : null,
      });
    }
  }

  const out: PullRequestRecord = {
    number: pr.number,
    title: pr.title,
    state: pr.state,
    baseRefName: pr.baseRefName,
    headRefName: pr.headRefName,
    headRefOid: pr.headRefOid,
    mergedAt: pr.mergedAt,
    createdAt: pr.createdAt,
    commit_count: pr.commits.length,
    merges,
    force_pushes: pushes,
  };
  if (pr.state === "OPEN") {
    const head = pr.headRefOid;
    if (await git.fetchOid(head)) {
      out.open_conflicts_now = await git.mergeTree(head, upstreamRef);
      out.open_behind_base = !(await git.isAncestor(upstreamRef, head));
    } else {
      out.open_conflicts_now = null;
    }
  }
  return out;
}

/** The pull request's base branch on origin, or origin/main when the base was deleted since. */
export async function upstreamRefFor(git: GitRepo, baseRefName: string): Promise<string> {
  const ref = `refs/remotes/origin/${baseRefName}`;
  return (await git.resolveCommit(ref)) !== null ? ref : ORIGIN_MAIN;
}

/** Resolves each `--fixed` commit to a full oid; refuses one git cannot resolve or not on main. */
export async function resolveFixes(
  git: GitRepo,
  specs: Array<{ glob: string; commit: string }>
): Promise<FixSpec[]> {
  const fixes: FixSpec[] = [];
  for (const { glob, commit } of specs) {
    const oid = await git.resolveCommit(commit);
    if (oid === null) throw new UsageError(`--fixed commit ${commit} does not resolve to a commit`);
    if (!(await git.isAncestor(oid, ORIGIN_MAIN))) {
      throw new UsageError(`--fixed commit ${commit} is not on origin/main`);
    }
    fixes.push({ glob, commit: oid });
  }
  return fixes;
}

/**
 * Where each merge stood against each fix whose glob matches one of its conflicted paths (first
 * matching spec wins per path), keyed by the spec's glob: `before` when the upstream parent lacks
 * the fix, `transition` when the upstream parent has it and the branch parent does not, `after`
 * when both have it.
 */
export async function classifyFixes(
  git: GitRepo,
  events: Array<Pick<MergeRecord, "oid" | "branch_parent" | "upstream_parent" | "conflicts">>,
  fixes: FixSpec[]
): Promise<FixStates> {
  const match = fixMatcher(fixes);
  const states: FixStates = new Map();
  for (const { oid, branch_parent, upstream_parent, conflicts } of events) {
    if (!branch_parent || !upstream_parent || !conflicts) continue;
    const byGlob = new Map<string, FixState>();
    for (const path of conflicts) {
      const fix = match(path);
      if (!fix || byGlob.has(fix.glob)) continue;
      let state: FixState;
      if (!(await git.isAncestor(fix.commit, upstream_parent))) state = "before";
      else if (!(await git.isAncestor(fix.commit, branch_parent))) state = "transition";
      else state = "after";
      byGlob.set(fix.glob, state);
    }
    if (byGlob.size > 0) states.set(oid, byGlob);
  }
  return states;
}

/** `fn` over `items` with at most `limit` calls in flight; results in input order. */
export async function mapLimit<T, R>(
  items: T[],
  limit: number,
  fn: (item: T) => Promise<R>
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      results[i] = await fn(items[i]);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return results;
}
