import { formatIssueKey, type IssueKey } from "@legion/contracts";
import { type CiFetchFailure, type CiFetchResult, isCiFetchFailure } from "../state/fetch";
import type { GitHubPRRef } from "../state/types";
import type { DaemonConfig } from "./config";
import type { LegionState } from "./legion-state";
import {
  advanceCiFence,
  type Effect,
  type EnvelopeJson,
  type ReducerConfig,
  reduceGithubEvent,
  resetPrHead,
  settleCiVerdict,
  uncertifyCiVerdict,
} from "./reducers";

export type ResyncAnomaly = {
  kind: "zero-owner-tree" | "erroring-issue" | "missed-open" | "untriaged-open" | "launch-failed";
  issue: IssueKey;
  detail: string;
};

export interface LegionEventPayload {
  type: "resync";
  anomalies: ResyncAnomaly[];
  healed: number;
  reconciledLabels: number;
  excludedNullContentItems: number;
  ciFetchFailures: number;
  ciFetchFailureDetails: CiFetchFailure[];
}

export interface RunResyncDeps {
  state: LegionState;
  config: Pick<DaemonConfig, "resyncIntervalMs"> & ReducerConfig;
  fetchGitHubProjectItems(): Promise<{
    items: Record<string, unknown>[];
    excludedNullContentItems?: number;
  }>;
  fetchCiStatusBatch(prRefs: Record<string, GitHubPRRef>): Promise<Record<string, CiFetchResult>>;
  applyEffects(effects: Effect[], envelope: EnvelopeJson): Promise<void>;
  now(): number;
}

const lastRunAt = new WeakMap<LegionState, number>();
const warnedMissingBoardProjectIds = new WeakSet<LegionState>();

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function boardIssue(item: Record<string, unknown>):
  | {
      issue: IssueKey;
      repository: string;
      number: number;
      open: boolean;
      erroring: boolean;
    }
  | undefined {
  const content = record(item.content);
  if (content?.type !== "Issue") return undefined;
  const repository = content.repository;
  const number = content.number;
  if (
    typeof repository !== "string" ||
    typeof number !== "number" ||
    !Number.isSafeInteger(number)
  ) {
    return undefined;
  }
  const [owner, repo, ...extra] = repository.split("/");
  if (!owner || !repo || extra.length > 0) return undefined;

  const projectStatus = item.status;
  const status = typeof projectStatus === "string" ? projectStatus.toLowerCase() : "";
  return {
    issue: formatIssueKey(owner, repo, number),
    repository,
    number,
    open: status !== "done" && status !== "closed",
    erroring: status.includes("error") || status.includes("failed"),
  };
}

function isBacklogged(state: LegionState, issue: IssueKey, item: Record<string, unknown>): boolean {
  if (state.issues[issue]?.backlogMarker) return true;
  const labels = item.labels;
  return Array.isArray(labels) && labels.includes("legion-backlog");
}

function boardLabels(item: Record<string, unknown>): Set<string> | undefined {
  if (!Array.isArray(item.labels)) return undefined;
  return new Set(item.labels.filter((label): label is string => typeof label === "string"));
}

function labeledBoardIssue(
  board: { issue: IssueKey; repository: string; number: number },
  action: "labeled" | "unlabeled",
  label: string,
  now: number
): EnvelopeJson {
  return {
    event_id: `resync:${board.issue}:${action}:${label}`,
    issued_at: now,
    payload: {
      action,
      issue: { number: board.number },
      label: { name: label },
      repository: { full_name: board.repository },
    },
  };
}

function openedBoardIssue(
  item: Record<string, unknown>,
  issue: IssueKey,
  projectId: string | undefined,
  now: number
): EnvelopeJson | undefined {
  const content = record(item.content);
  const repository = content?.repository;
  if (!content || typeof repository !== "string" || !projectId) return undefined;

  // This is reducer input reconstructed from the board GitHub just fetched, not an external webhook.
  return {
    event_id: `resync:${issue}`,
    issued_at: now,
    payload: {
      action: "opened",
      project: { id: projectId },
      projects_v2_item: { content },
      repository: { full_name: repository },
    },
  };
}

function hasTree(state: LegionState, issue: IssueKey): boolean {
  const seen = new Set<IssueKey>();
  let current: IssueKey | undefined = issue;
  while (current && !seen.has(current)) {
    if (state.trees[current]) return true;
    seen.add(current);
    current = state.issues[current]?.parent;
  }
  return false;
}

function hasActiveTree(state: LegionState, issue: IssueKey): boolean {
  const seen = new Set<IssueKey>();
  let current: IssueKey | undefined = issue;
  while (current && !seen.has(current)) {
    if (state.trees[current]?.status === "active") return true;
    seen.add(current);
    current = state.issues[current]?.parent;
  }
  return false;
}

async function reconcilePrs(deps: RunResyncDeps, now: number): Promise<CiFetchFailure[]> {
  const refs: Record<string, GitHubPRRef> = {};
  const heads = new Map<string, string>();
  for (const [prKey, pr] of Object.entries(deps.state.prs)) {
    const [owner, repo] = pr.repo.split("/");
    refs[prKey] = { owner, repo, number: pr.number };
    heads.set(prKey, pr.headSha);
  }
  if (Object.keys(refs).length === 0) return [];

  const statuses = await deps.fetchCiStatusBatch(refs);
  const ciFetchFailures: CiFetchFailure[] = [];
  const reportedFailures = new Set<string>();
  for (const [prKey, ref] of Object.entries(refs)) {
    const pr = deps.state.prs[prKey];
    const status = statuses[prKey];
    if (!pr || !status || pr.headSha !== heads.get(prKey)) continue;
    if (isCiFetchFailure(status)) {
      const failureKey = `${status.owner}\u0000${status.error}`;
      if (!reportedFailures.has(failureKey)) {
        reportedFailures.add(failureKey);
        ciFetchFailures.push(status);
      }
      continue;
    }
    if (!status.isOpen || !status.headSha) continue;

    if (pr.headSha !== status.headSha) {
      if (!status.updatedAt) {
        throw new Error(`GitHub CI status is missing updatedAt for ${prKey}`);
      }
      const headUpdatedAt = Date.parse(status.updatedAt);
      if (Number.isNaN(headUpdatedAt)) {
        throw new Error(`GitHub CI status has an invalid updatedAt for ${prKey}`);
      }
      resetPrHead(pr, status.headSha);
      pr.headUpdatedAt = headUpdatedAt;
    }
    // The fetched max check-run id is ordering information whatever the
    // rollup state: a pending rerun already has newer runs, and a delayed
    // older live settlement must not pass the fence.
    advanceCiFence(pr, status.latestCheckRunId);
    const failing = status.failingChecks ?? [];
    // Mirror live intake: red only for actual failures; a cancelled-only
    // failing rollup, like a pending one, uncertifies a green head.
    const verdict =
      status.ciStatus === "passing"
        ? "green"
        : status.ciStatus === "failing" && failing.length > 0
          ? "red"
          : null;
    if (verdict === null) {
      if (status.ciStatus === "pending" || status.ciStatus === "failing") uncertifyCiVerdict(pr);
      continue;
    }
    const effects = settleCiVerdict(
      deps.state,
      pr,
      { verdict, failing, settledAt: now, latestCheckRunId: status.latestCheckRunId },
      deps.config
    );
    if (effects.length === 0) continue;
    await deps.applyEffects(effects, {
      event_id: `resync:${ref.owner}/${ref.repo}#${ref.number}:ci`,
      issued_at: now,
    });
  }
  return ciFetchFailures;
}

/**
 * Reads board artifacts and unsettled PR check rollups, mechanically converging
 * missed-open items, CI verdicts, and label drift through existing effects.
 */
export async function runResync(deps: RunResyncDeps): Promise<LegionEventPayload> {
  const now = deps.now();
  const last = lastRunAt.get(deps.state);
  if (last !== undefined && now - last < deps.config.resyncIntervalMs) {
    return {
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    };
  }

  lastRunAt.set(deps.state, now);
  const { items, excludedNullContentItems = 0 } = await deps.fetchGitHubProjectItems();
  const ciFetchFailureDetails = await reconcilePrs(deps, now);
  if (
    items.length > 0 &&
    deps.config.boardProjectIds.length === 0 &&
    !warnedMissingBoardProjectIds.has(deps.state)
  ) {
    warnedMissingBoardProjectIds.add(deps.state);
    console.error(
      "[legion] resync cannot heal board items because LEGION_BOARD_PROJECT_IDS is not configured"
    );
  }
  const anomalies: ResyncAnomaly[] = [];
  let healed = 0;
  let reconciledLabels = 0;
  const launchFailedTrees = new Set<IssueKey>();
  for (const [issue, tree] of Object.entries(deps.state.trees) as Array<
    [IssueKey, LegionState["trees"][IssueKey]]
  >) {
    if (tree.status !== "launch-failed" || deps.state.issues[issue]?.backlogMarker) continue;
    launchFailedTrees.add(issue);
    anomalies.push({
      kind: "launch-failed",
      issue,
      detail: `tree launch failed ${tree.launchFailures} times`,
    });
  }
  for (const item of items) {
    const board = boardIssue(item);
    if (!board?.open) continue;

    const issue = deps.state.issues[board.issue];
    if (issue) {
      const labels = boardLabels(item);
      if (labels) {
        for (const label of new Set([...labels, ...issue.labels])) {
          const present = issue.labels.includes(label);
          const expected = labels.has(label);
          if (present === expected) continue;
          const action = expected ? "labeled" : "unlabeled";
          const envelope = labeledBoardIssue(board, action, label, now);
          const effects = reduceGithubEvent(deps.state, "resync", envelope, deps.config);
          if (issue.labels.includes(label) !== expected) continue;
          await deps.applyEffects(effects, envelope);
          reconciledLabels += 1;
        }
      }
      if (launchFailedTrees.has(board.issue) || isBacklogged(deps.state, board.issue, item)) {
        continue;
      }
      if (board.erroring) {
        anomalies.push({
          kind: "erroring-issue",
          issue: board.issue,
          detail: "open board issue has an error project status",
        });
        continue;
      }
      if (
        issue.state === "open" &&
        !hasTree(deps.state, board.issue) &&
        !deps.state.admission.active.includes(board.issue) &&
        !deps.state.admission.queue.includes(board.issue)
      ) {
        const envelope: EnvelopeJson = {
          event_id: `resync:${board.issue}:triage`,
          issued_at: now,
        };
        await deps.applyEffects(
          [
            {
              kind: "controller",
              payload: {
                type: "triage",
                issue: board.issue,
                preexistingChildren: issue.children,
              },
            },
          ],
          envelope
        );
        anomalies.push(
          issue.released
            ? {
                kind: "zero-owner-tree",
                issue: board.issue,
                detail: "released open issue has no active Legion tree",
              }
            : {
                kind: "untriaged-open",
                issue: board.issue,
                detail: "tracked open issue has no Legion tree or admission entry",
              }
        );
      } else if (
        issue.state === "open" &&
        issue.released &&
        !hasActiveTree(deps.state, board.issue)
      ) {
        anomalies.push({
          kind: "zero-owner-tree",
          issue: board.issue,
          detail: "released open issue has no active Legion tree",
        });
      }
      continue;
    }

    if (launchFailedTrees.has(board.issue) || isBacklogged(deps.state, board.issue, item)) continue;
    const envelope = openedBoardIssue(item, board.issue, deps.config.boardProjectIds[0], now);
    if (envelope) {
      const effects = reduceGithubEvent(deps.state, "resync", envelope, deps.config);
      if (deps.state.issues[board.issue]) {
        await deps.applyEffects(effects, envelope);
        healed += 1;
        if (board.erroring) {
          anomalies.push({
            kind: "erroring-issue",
            issue: board.issue,
            detail: "open board issue has an error project status",
          });
        }
        continue;
      }
    }
    anomalies.push({
      kind: "missed-open",
      issue: board.issue,
      detail: "open board issue is absent from Legion state",
    });
  }
  return {
    type: "resync",
    anomalies,
    healed,
    reconciledLabels,
    excludedNullContentItems,
    ciFetchFailures: ciFetchFailureDetails.length,
    ciFetchFailureDetails,
  };
}
