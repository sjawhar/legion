import { type GitRepo, ORIGIN_MAIN } from "./git.ts";
import {
  type FixPair,
  type FixSpec,
  type FixState,
  type FixStates,
  type ForcePushRecord,
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
    if (outside.length === 0) {
      // Every parent on the PR (merging two PR lines): not a forward merge, and no git needed.
      merges.push({ ...commit, kind: "internal" });
      continue;
    }
    let missing = false;
    for (const p of parents) {
      if (!(await git.fetchOid(p))) {
        missing = true;
        break;
      }
    }
    if (missing) {
      // Undeterminable: no ancestry call or merge-tree can run without every parent.
      const sides =
        branch.length === 1 && outside.length === 1
          ? { branch_parent: branch[0], upstream_parent: outside[0] }
          : {};
      merges.push({
        ...commit,
        ...sides,
        kind: "forward",
        conflicts: null,
        error: "parent object missing",
      });
      continue;
    }
    // No parent on the PR (rare): treat the parent not on upstream as the branch side; when that
    // split fails too, it is not a forward merge.
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
    const b = branch[0];
    const u = outside[0];
    const onMain = await git.isAncestor(u, ORIGIN_MAIN);
    const onBase = upstreamRef === ORIGIN_MAIN ? onMain : await git.isAncestor(u, upstreamRef);
    merges.push({
      ...commit,
      branch_parent: b,
      upstream_parent: u,
      upstream_on_main: onMain,
      upstream_on_base: onBase,
      kind: onMain || onBase ? "forward" : "other-branch",
      conflicts: await git.mergeTree(b, u),
    });
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
    closedAt: pr.closedAt,
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
 * Where each pair's merge stood against its glob's fix: `before` when the upstream parent lacks
 * the fix, `transition` when the upstream parent has it and the branch parent does not, `after`
 * when both have it. Throws when a merge's parent is absent or cannot be fetched.
 */
export async function classifyFixes(git: GitRepo, pairs: FixPair[]): Promise<FixStates> {
  const states: FixStates = new Map();
  for (const { oid, glob, commit, branch_parent, upstream_parent } of pairs) {
    if (!branch_parent || !upstream_parent) {
      throw new Error(`merge ${oid}: no branch or upstream parent to classify against ${glob}`);
    }
    let byGlob = states.get(oid);
    if (byGlob === undefined) {
      for (const parent of [upstream_parent, branch_parent]) {
        if (!(await git.fetchOid(parent))) {
          throw new Error(`merge ${oid}: parent ${parent} is missing and could not be fetched`);
        }
      }
      byGlob = new Map<string, FixState>();
      states.set(oid, byGlob);
    }
    let state: FixState;
    if (!(await git.isAncestor(commit, upstream_parent))) state = "before";
    else if (!(await git.isAncestor(commit, branch_parent))) state = "transition";
    else state = "after";
    byGlob.set(glob, state);
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
