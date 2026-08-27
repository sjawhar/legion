import { formatIssueKey, type IssueKey, LegionDaemonApi, parseIssueKey } from "@legion/contracts";
import { getApprovalState } from "../../approval-check";
import type { LegionState } from "../../legion-state";
import { type RouteContext, treeContains } from "../context";
import { asRecord, HttpError, requiredNumber, validateContractResponse } from "../http";

interface MergeGatePrSnapshot {
  repo: `${string}/${string}`;
  raw: Record<string, unknown>;
  head: { ref: string; sha: string };
}

async function fetchMergeGatePr(
  ctx: RouteContext,
  tree: IssueKey,
  number: number
): Promise<MergeGatePrSnapshot> {
  const treeIssue = parseIssueKey(tree);
  if (!treeIssue) {
    throw new Error(`Invalid issue key in Legion state: ${tree}`);
  }
  const repo = `${treeIssue.owner}/${treeIssue.repo}` as `${string}/${string}`;
  const raw = asRecord(
    JSON.parse(await ctx.github.gh(tree, ["gh", "api", `repos/${repo}/pulls/${number}`]))
  );
  if (raw.number !== number) {
    throw new Error(`GitHub returned PR #${String(raw.number)} for requested PR #${number}`);
  }
  const head = asRecord(raw.head);
  if (typeof head.ref !== "string" || typeof head.sha !== "string") {
    throw new Error(`GitHub PR #${number} has an invalid head`);
  }
  return { repo, raw, head: { ref: head.ref, sha: head.sha } };
}

async function recoverPrForMergeGate(
  ctx: RouteContext,
  tree: IssueKey,
  number: number,
  snapshot: MergeGatePrSnapshot
): Promise<LegionState["prs"][string] | undefined> {
  const treeIssue = parseIssueKey(tree);
  if (!treeIssue) {
    throw new Error(`Invalid issue key in Legion state: ${tree}`);
  }
  const { repo, raw, head } = snapshot;
  const branchMatch = /^legion\/issue-(\d+)$/.exec(head.ref);
  let key = branchMatch
    ? formatIssueKey(treeIssue.owner, treeIssue.repo, Number(branchMatch[1]))
    : undefined;
  if (!key || !ctx.deps.state.issues[key] || !treeContains(ctx.deps.state, tree, key)) {
    const closingIssues = new Set<IssueKey>();
    if (typeof raw.body === "string") {
      for (const match of raw.body.matchAll(
        /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b/gi
      )) {
        const candidate = formatIssueKey(treeIssue.owner, treeIssue.repo, Number(match[1]));
        if (ctx.deps.state.issues[candidate] && treeContains(ctx.deps.state, tree, candidate)) {
          closingIssues.add(candidate);
        }
      }
    }
    [key] = closingIssues;
    if (closingIssues.size !== 1 || !key) return undefined;
  }
  const prKey = `${repo}#${number}`;
  if (ctx.deps.state.prs[prKey]) {
    return undefined;
  }
  const pr = {
    key,
    repo,
    number,
    headSha: head.sha,
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: ctx.now(),
    fixAttempts: 0,
  };
  ctx.deps.state.prs[prKey] = pr;
  ctx.deps.state.prByBranch[`${repo}@${head.ref}`] = prKey;
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
  const snapshot = await fetchMergeGatePr(ctx, tree, number);
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
  if (pr.repo !== snapshot.repo) {
    throw new Error(`GitHub returned PR #${number} from an unexpected repository`);
  }
  if (pr.headSha !== snapshot.head.sha) {
    if (
      Object.values(pr.checks).some(
        (check) =>
          check.status === "completed" &&
          check.conclusion !== "success" &&
          check.conclusion !== "neutral" &&
          check.conclusion !== "skipped"
      )
    ) {
      pr.fixAttempts += 1;
    }
    pr.headSha = snapshot.head.sha;
    pr.checks = {};
    pr.firstRedEmitted = false;
    pr.settledRedEmitted = false;
    pr.greenEmitted = false;
    delete pr.reviewDecision;
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
