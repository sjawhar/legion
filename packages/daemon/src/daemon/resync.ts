import { formatIssueKey, type IssueKey } from "@legion/contracts";
import type { DaemonConfig } from "./config";
import type { LegionState } from "./legion-state";
import { type Effect, type EnvelopeJson, type ReducerConfig, reduceGithubEvent } from "./reducers";

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

/**
 * Reads board artifacts and mechanically converges missed-open items and
 * label drift through the same reducer and effect executor that processes
 * live board events.
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
    };
  }

  lastRunAt.set(deps.state, now);
  const { items, excludedNullContentItems = 0 } = await deps.fetchGitHubProjectItems();
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
  };
}
