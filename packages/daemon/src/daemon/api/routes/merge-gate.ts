import { type IssueKey, LegionDaemonApi } from "@legion/contracts";
import { getApprovalState } from "../../approval-check";
import type { LegionState } from "../../legion-state";
import { issueForBranch, issueForPrBody, resetPrHead, supersededBy } from "../../reducers";
import { type RouteContext, treeContains } from "../context";
import { asRecord, HttpError, requiredNumber, validateContractResponse } from "../http";

interface MergeGatePrSnapshot {
  raw: Record<string, unknown>;
  head: { ref: string; sha: string };
  /** The PR's lifecycle clock (GitHub `updated_at`), so a delayed older synchronize cannot rewind this head. */
  updatedAt: number;
}

async function fetchMergeGatePr(ctx: RouteContext, number: number): Promise<MergeGatePrSnapshot> {
  const raw = asRecord(
    JSON.parse(await ctx.github.gh(["gh", "api", `repos/${ctx.config.repo}/pulls/${number}`]))
  );
  if (raw.number !== number) {
    throw new Error(`GitHub returned PR #${String(raw.number)} for requested PR #${number}`);
  }
  const head = asRecord(raw.head);
  if (typeof head.ref !== "string" || typeof head.sha !== "string") {
    throw new Error(`GitHub PR #${number} has an invalid head`);
  }
  const updatedAt = typeof raw.updated_at === "string" ? Date.parse(raw.updated_at) : Number.NaN;
  if (Number.isNaN(updatedAt)) {
    throw new Error(`GitHub PR #${number} has an invalid updated_at`);
  }
  return { raw, head: { ref: head.ref, sha: head.sha }, updatedAt };
}

async function recoverPrForMergeGate(
  ctx: RouteContext,
  tree: IssueKey,
  number: number,
  snapshot: MergeGatePrSnapshot
): Promise<LegionState["prs"][string] | undefined> {
  const { raw, head } = snapshot;
  const key =
    issueForBranch(head.ref) ??
    (typeof raw.body === "string" ? issueForPrBody(raw.body) : undefined);
  if (!key || !ctx.deps.state.issues[key] || !treeContains(ctx.deps.state, tree, key)) {
    return undefined;
  }
  const prKey = `${ctx.config.repo}#${number}`;
  if (ctx.deps.state.prs[prKey]) {
    return undefined;
  }
  const pr = {
    key,
    repo: ctx.config.repo,
    number,
    headSha: head.sha,
    headUpdatedAt: snapshot.updatedAt,
    headUpdatedAtSource: "resync" as const,
    verdict: null,
    failing: [],
    failingStatuses: [],
    ciSettledAt: null,
    ciCheckRuns: null,
    ciSettlementGeneration: null,
    ciSnapshot: null,
    ciReconciled: false,
    fixAttempts: 0,
  };
  ctx.deps.state.prs[prKey] = pr;
  ctx.deps.state.prByBranch[`${ctx.config.repo}@${head.ref}`] = prKey;
  await ctx.save();
  return pr;
}

export async function handleMergeGate(
  ctx: RouteContext,
  body: Record<string, unknown>
): Promise<Response> {
  const tree = ctx.requireTree(body);
  ctx.auth.requireArchitectCapability(body, tree);
  if (ctx.config.gates.merge !== "human") {
    throw new HttpError(409, "Human merge gate is disabled");
  }
  const number = requiredNumber(body, "pr");
  const snapshot = await fetchMergeGatePr(ctx, number);
  let matchingPrs = Object.values(ctx.deps.state.prs).filter(
    (candidate) => candidate.number === number && treeContains(ctx.deps.state, tree, candidate.key)
  );
  if (matchingPrs.length === 0) {
    const recovered = await recoverPrForMergeGate(ctx, tree, number, snapshot);
    if (!recovered) {
      throw new HttpError(404, `No PR #${number} belongs to tree ${tree}`);
    }
    matchingPrs = [recovered];
  }
  if (matchingPrs.length > 1) {
    throw new HttpError(409, `PR #${number} is ambiguous within tree ${tree}`);
  }
  const pr = matchingPrs[0];
  if (!pr) throw new Error("Matching merge-gate PR disappeared");
  if (pr.repo !== ctx.config.repo) {
    throw new Error(`GitHub returned PR #${number} from an unexpected repository`);
  }
  // GitHub's read orders against the PR's lifecycle clock as resync does: a
  // different head replaces the stored one unless the read is strictly older
  // than a lifecycle update already observed (one that landed before or while
  // the read was in flight); the same head still advances the clock, so a
  // delayed synchronize for an intervening head is rejected as older.
  if (pr.headSha !== snapshot.head.sha) {
    if (
      supersededBy(
        { updatedAt: snapshot.updatedAt, source: "resync" },
        { updatedAt: pr.headUpdatedAt, source: pr.headUpdatedAtSource ?? "webhook" }
      )
    ) {
      console.debug(
        `[legion] ignored stale merge-gate head for ${pr.repo}#${pr.number} fetched=${snapshot.head.sha}@${new Date(snapshot.updatedAt).toISOString()} known=${pr.headSha}@${new Date(pr.headUpdatedAt ?? 0).toISOString()}`
      );
    } else {
      resetPrHead(pr, snapshot.head.sha);
      pr.headUpdatedAt = snapshot.updatedAt;
      pr.headUpdatedAtSource = "resync";
      await ctx.save();
    }
  } else if (pr.headUpdatedAt === undefined || snapshot.updatedAt > pr.headUpdatedAt) {
    pr.headUpdatedAt = snapshot.updatedAt;
    pr.headUpdatedAtSource = "resync";
    await ctx.save();
  }
  const approval = await getApprovalState(
    { repo: pr.repo, pr: pr.number, sha: pr.headSha },
    {
      runner: ctx.runner,
      tokenManager: ctx.deps.tokenManager,
      appLogins: ctx.config.appLogins ?? [],
      gatesMerge: ctx.config.gates.merge,
    }
  );
  return Response.json(
    validateContractResponse(LegionDaemonApi.MergeGate.response, {
      approved: approval === "success",
      pr: pr.number,
      headSha: pr.headSha,
    })
  );
}
