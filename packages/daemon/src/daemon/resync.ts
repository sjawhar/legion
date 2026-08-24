import { formatIssueKey, type IssueKey } from "@legion/contracts";
import type { DaemonConfig } from "./config";
import type { LegionState } from "./legion-state";
import { type Effect, type EnvelopeJson, type ReducerConfig, reduceGithubEvent } from "./reducers";

export type ResyncAnomaly = {
  kind: "zero-owner-tree" | "erroring-issue" | "missed-open" | "launch-failed";
  issue: IssueKey;
  detail: string;
};

export interface LegionEventPayload {
  type: "resync";
  anomalies: ResyncAnomaly[];
  healed: number;
  excludedNullContentItems: number;
}

export interface RunResyncDeps {
  state: LegionState;
  config: Pick<DaemonConfig, "resyncIntervalMs"> & ReducerConfig;
  fetchGitHubProjectItems(): Promise<{
    items: Record<string, unknown>[];
    excludedNullContentItems?: number;
  }>;
  applyEffects(effects: Effect[], envelope: EnvelopeJson): Promise<void>;
  now(): number;
}

const lastRunAt = new WeakMap<LegionState, number>();

function record(value: unknown): Record<string, unknown> | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

function boardIssue(
  item: Record<string, unknown>
): { issue: IssueKey; open: boolean; erroring: boolean } | undefined {
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
    open: status !== "done" && status !== "closed",
    erroring: status.includes("error") || status.includes("failed"),
  };
}

function isBacklogged(state: LegionState, issue: IssueKey, item: Record<string, unknown>): boolean {
  if (state.issues[issue]?.backlogMarker) return true;
  const labels = item.labels;
  return Array.isArray(labels) && labels.includes("legion-backlog");
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

/**
 * Reads board artifacts and mechanically converges missed-open items through
 * the same reducer and effect executor that processes live board events.
 */
export async function runResync(deps: RunResyncDeps): Promise<LegionEventPayload> {
  const now = deps.now();
  const last = lastRunAt.get(deps.state);
  if (last !== undefined && now - last < deps.config.resyncIntervalMs) {
    return { type: "resync", anomalies: [], healed: 0, excludedNullContentItems: 0 };
  }

  lastRunAt.set(deps.state, now);
  const { items, excludedNullContentItems = 0 } = await deps.fetchGitHubProjectItems();
  const anomalies: ResyncAnomaly[] = [];
  let healed = 0;
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
    if (
      !board?.open ||
      launchFailedTrees.has(board.issue) ||
      isBacklogged(deps.state, board.issue, item)
    ) {
      continue;
    }

    const issue = deps.state.issues[board.issue];
    if (!issue) {
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
    if (issue.state === "open" && issue.released && !hasActiveTree(deps.state, board.issue)) {
      anomalies.push({
        kind: "zero-owner-tree",
        issue: board.issue,
        detail: "released open issue has no active Legion tree",
      });
    }
  }
  return { type: "resync", anomalies, healed, excludedNullContentItems };
}
