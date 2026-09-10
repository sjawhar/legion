import type { IssueKey } from "@legion/contracts";
import { type CiFetchFailure, type CiFetchResult, isCiFetchFailure } from "../state/fetch";
import type { GitHubPRRef } from "../state/types";
import type { DaemonConfig } from "./config";
import type { DispatchClient } from "./dispatch-client";
import { writeStatus } from "./dispatch-client";
import type { IssueStatus, LegionState } from "./legion-state";
import {
  acceptGitHubFence,
  type CiSnapshot,
  ciSnapshot,
  ciSnapshotEquals,
  type DispatchIssueEvent,
  type Effect,
  type EnvelopeJson,
  type ReducerConfig,
  reduceDispatchEvent,
  resetPrHead,
  settleCiVerdict,
  supersededBy,
  uncertifyCiVerdict,
  writeCiFence,
} from "./reducers";

export type ResyncAnomaly = {
  kind: "zero-owner-tree" | "untriaged-open" | "launch-failed";
  issue: IssueKey;
  detail: string;
};

export interface LegionEventPayload {
  type: "resync";
  anomalies: ResyncAnomaly[];
  healed: number;
  ciFetchFailures: number;
  ciFetchFailureDetails: CiFetchFailure[];
}

export interface RunResyncDeps {
  state: LegionState;
  config: Pick<DaemonConfig, "resyncIntervalMs" | "dispatchProject"> & ReducerConfig;
  dispatchClient: DispatchClient;
  fetchCiStatusBatch(prRefs: Record<string, GitHubPRRef>): Promise<Record<string, CiFetchResult>>;
  applyEffects(effects: Effect[], envelope: EnvelopeJson): Promise<void>;
  now(): number;
}

const lastRunAt = new WeakMap<LegionState, number>();

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
  const snapshots = new Map<string, CiSnapshot>();
  for (const [prKey, pr] of Object.entries(deps.state.prs)) {
    const [owner, repo] = pr.repo.split("/");
    refs[prKey] = { owner, repo, number: pr.number };
    snapshots.set(prKey, ciSnapshot(pr));
  }
  if (Object.keys(refs).length === 0) return [];

  const statuses = await deps.fetchCiStatusBatch(refs);
  const ciFetchFailures: CiFetchFailure[] = [];
  const reportedFailures = new Set<string>();
  for (const [prKey, ref] of Object.entries(refs)) {
    const pr = deps.state.prs[prKey];
    const status = statuses[prKey];
    const snapshot = snapshots.get(prKey);
    if (!pr || !status || !snapshot) continue;
    if (isCiFetchFailure(status)) {
      const failureKey = `${status.owner}\u0000${status.error}`;
      if (!reportedFailures.has(failureKey)) {
        reportedFailures.add(failureKey);
        ciFetchFailures.push(status);
      }
      continue;
    }
    if (!ciSnapshotEquals(pr, snapshot)) {
      console.debug(`[legion] skipped stale resync CI result ${prKey}`);
      continue;
    }
    if (!status.isOpen || !status.headSha) continue;

    // The PR's lifecycle clock (GitHub's updatedAt) orders head observations.
    // A same-head read advances it, so a delayed synchronize for an intervening
    // head is later rejected as older. A different fetched head is skipped only
    // when its clock is strictly older than a lifecycle update already observed:
    // the read is GitHub's current head, and at an equal clock (second
    // resolution) it outranks a webhook — deliveries may arrive out of order, and
    // a stale equal-clock synchronize must not block the read that corrects it.
    // A head that moved during the read is caught by the snapshot guard above.
    const headUpdatedAt = status.updatedAt === null ? Number.NaN : Date.parse(status.updatedAt);
    if (pr.headSha === status.headSha) {
      if (
        !Number.isNaN(headUpdatedAt) &&
        (pr.headUpdatedAt === undefined || headUpdatedAt > pr.headUpdatedAt)
      ) {
        pr.headUpdatedAt = headUpdatedAt;
        pr.headUpdatedAtSource = "resync";
      }
    } else {
      if (!status.updatedAt) {
        throw new Error(`GitHub CI status is missing updatedAt for ${prKey}`);
      }
      if (Number.isNaN(headUpdatedAt)) {
        throw new Error(`GitHub CI status has an invalid updatedAt for ${prKey}`);
      }
      if (
        supersededBy(
          { updatedAt: headUpdatedAt, source: "resync" },
          { updatedAt: pr.headUpdatedAt, source: pr.headUpdatedAtSource ?? "webhook" }
        )
      ) {
        console.debug(
          `[legion] ignored stale resync head for ${prKey} fetched=${status.headSha}@${status.updatedAt} known=${pr.headSha}@${new Date(pr.headUpdatedAt ?? 0).toISOString()}`
        );
        continue;
      }
      resetPrHead(pr, status.headSha);
      pr.headUpdatedAt = headUpdatedAt;
      pr.headUpdatedAtSource = "resync";
    }
    // GitHub's rollup carries an attempt set with no listener identity. It
    // advances the stored fence, applies at an equal set, applies unfenced, or
    // is an older or inconsistent view and is skipped — acceptGitHubFence decides.
    const fenceEffect = acceptGitHubFence(pr, status.checkRuns);
    if (fenceEffect === "stale" || fenceEffect === "conflict") {
      console.debug(
        `[legion] ignored ${fenceEffect} rollup for ${prKey} check_runs=${JSON.stringify(status.checkRuns)} fence=${JSON.stringify(pr.ciCheckRuns)}`
      );
      continue;
    }
    if (fenceEffect === "advance") {
      writeCiFence(pr, { checkRuns: status.checkRuns, generation: null, snapshot: null });
    }
    // GitHub's read is a complete view: its failing check runs and failing
    // statuses replace the stored ones wholesale (only GitHub can retire a
    // failure the listener cannot see — a status context, a deleted check).
    // Red only for actual failures; a cancelled-only failing rollup, like a
    // pending one, uncertifies a green head and leaves a red one, failing
    // names included, untouched.
    const failing = status.failingChecks ?? [];
    const failingStatuses = status.failingStatuses ?? [];
    const verdict =
      status.ciStatus === "passing"
        ? "green"
        : status.ciStatus === "failing" && failing.length + failingStatuses.length > 0
          ? "red"
          : null;
    // A terminal read holds the tie at this attempt set until the set advances;
    // a pending or cancelled-only read carries no verdict and holds nothing.
    pr.ciReconciled = fenceEffect !== "unfenced" && verdict !== null;
    if (verdict === null) {
      if (status.ciStatus === "pending" || status.ciStatus === "failing") uncertifyCiVerdict(pr);
      continue;
    }
    const envelope = {
      event_id: `resync:${ref.owner}/${ref.repo}#${ref.number}:ci`,
      issued_at: now,
    };
    const effects = settleCiVerdict(
      deps.state,
      pr,
      { verdict, failing, failingStatuses, settledAt: now },
      deps.config,
      envelope
    );
    if (effects.length === 0) continue;
    await deps.applyEffects(effects, envelope);
  }
  return ciFetchFailures;
}

/** Retries every daemon-owned Dispatch status write recorded by `writeStatus` after an earlier
 * PATCH failure (`spawnTree`/`closeTree`/`phase/complete`/the `/issues/status` and
 * `/waves/release` routes) — the only retry mechanism for those writes; the client itself never
 * retries (see `dispatch-client.ts`). Runs before the drift scan below so a successful retry is
 * reflected in the same cycle's `listIssues` read instead of being reported as drift. A pending
 * `done` write whose issue is already locally `done` is dropped instead of retried: Dispatch
 * permanently refuses a further status PATCH on an issue it already considers closed, so
 * retrying would never succeed and would log the same failure every cycle forever (this can
 * only happen for a pending write recorded before `closeTreeLocked` learned to skip that same
 * redundant write itself — kept here as a self-healing backstop for any such write already on
 * disk). */
async function retryPendingStatusWrites(deps: RunResyncDeps): Promise<void> {
  for (const [issue, status] of Object.entries(deps.state.pendingStatusWrites)) {
    if (status === "done" && deps.state.issues[issue]?.status === "done") {
      delete deps.state.pendingStatusWrites[issue];
      continue;
    }
    await writeStatus(deps.state, deps.dispatchClient, issue, status);
  }
}

/**
 * Replays every Dispatch issue whose status differs from this daemon's last-applied one as a
 * synthetic `issue.updated` event, fenced by that issue's own `last_seq` from Dispatch — the same
 * at-most-once fence `reduceDispatchEvent` applies to a live event, so an issue this daemon is
 * already caught up on (its `lastAppliedSeq` already at or past Dispatch's `last_seq`) is a no-op
 * here too. `listIssues` omits `last_seq` (only `getIssue` carries it), so a drifted issue costs
 * one extra read — bounded by how many issues actually drifted, not the whole project.
 */
async function healStatusDrift(deps: RunResyncDeps, now: number): Promise<number> {
  const summaries = await deps.dispatchClient.listIssues(deps.config.dispatchProject);
  let healed = 0;
  for (const summary of summaries) {
    const node = deps.state.issues[summary.key];
    if (!node || node.status === summary.status) continue;
    const details = await deps.dispatchClient.getIssue(summary.key);
    const event: DispatchIssueEvent = {
      type: "issue.updated",
      key: summary.key,
      seq: details.last_seq,
      notify: false,
      payload: details,
      eventId: `resync:${summary.key}:${details.last_seq}`,
    };
    const effects = reduceDispatchEvent(deps.state, event, deps.config);
    const envelope: EnvelopeJson = { event_id: event.eventId, issued_at: now };
    await deps.applyEffects(effects, envelope);
    if (deps.state.issues[summary.key]?.status === summary.status) healed += 1;
  }
  return healed;
}

/** Root-issue consistency anomalies a healed drift scan cannot itself explain: an issue Dispatch
 * still considers alive but whose tree/admission bookkeeping has fallen out of step with its own
 * status. Never self-heals `zero-owner-tree` (an architect or human must decide whether to
 * re-admit); `untriaged-open` self-heals by re-emitting the `issue.created` controller wake the
 * daemon apparently lost. */
function reportRootAnomalies(deps: RunResyncDeps, now: number): Promise<ResyncAnomaly[]> {
  const anomalies: ResyncAnomaly[] = [];
  const acks: Promise<void>[] = [];
  for (const node of Object.values(deps.state.issues)) {
    if (node.parent || node.status === undefined) continue;
    if (node.status === "done" || node.status === "backlog" || node.status === "icebox") continue;
    if (node.status === "triage") {
      if (
        hasTree(deps.state, node.key) ||
        deps.state.admission.active.includes(node.key) ||
        deps.state.admission.queue.includes(node.key)
      ) {
        continue;
      }
      anomalies.push({
        kind: "untriaged-open",
        issue: node.key,
        detail: "tracked triage issue has no Legion tree or admission entry",
      });
      acks.push(
        deps.applyEffects(
          [
            {
              kind: "controller",
              payload: { type: "triage", issue: node.key, preexistingChildren: node.children },
            },
          ],
          { event_id: `resync:${node.key}:triage`, issued_at: now }
        )
      );
      continue;
    }
    if (!hasActiveTree(deps.state, node.key)) {
      anomalies.push({
        kind: "zero-owner-tree",
        issue: node.key,
        detail: `Dispatch status "${node.status}" has no active Legion tree`,
      });
    }
  }
  const launchFailedTrees = new Set<IssueKey>();
  for (const [issue, tree] of Object.entries(deps.state.trees) as Array<
    [IssueKey, LegionState["trees"][IssueKey]]
  >) {
    if (tree.status !== "launch-failed") continue;
    const status: IssueStatus | undefined = deps.state.issues[issue]?.status;
    if (status === "backlog" || status === "icebox") continue;
    launchFailedTrees.add(issue);
    anomalies.push({
      kind: "launch-failed",
      issue,
      detail: `tree launch failed ${tree.launchFailures} times`,
    });
  }
  return Promise.all(acks).then(() => anomalies);
}

/**
 * Retries failed daemon-owned status writes, heals Dispatch status drift this daemon's own
 * durable consumer missed, reconciles unsettled PR check rollups, and reports root-issue
 * anomalies a status replay alone cannot self-heal.
 */
export async function runResync(deps: RunResyncDeps): Promise<LegionEventPayload> {
  const now = deps.now();
  const last = lastRunAt.get(deps.state);
  if (last !== undefined && now - last < deps.config.resyncIntervalMs) {
    return {
      type: "resync",
      anomalies: [],
      healed: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    };
  }

  lastRunAt.set(deps.state, now);
  await retryPendingStatusWrites(deps);
  const healed = await healStatusDrift(deps, now);
  const ciFetchFailureDetails = await reconcilePrs(deps, now);
  const anomalies = await reportRootAnomalies(deps, now);
  return {
    type: "resync",
    anomalies,
    healed,
    ciFetchFailures: ciFetchFailureDetails.length,
    ciFetchFailureDetails,
  };
}
