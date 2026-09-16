import {
  ISSUE_STATUSES,
  type IssueKey,
  type IssueStatus,
  type LegionRole,
  roleToken,
} from "@legion/contracts";
import { type CheckRunRef, sortedCheckRunRefs } from "../state/types";
import type { DispatchIssueEvent } from "./dispatch-events";
import {
  assertKnownPhase,
  type DesignGate,
  designGateOpen,
  type IssueNode,
  isStaleQueuedStatus,
  type LegionState,
  liveAncestorTree,
  owningArchitect,
  type PrState,
  type TreeState,
  type UpdateSource,
} from "./legion-state";

export type { DispatchIssueEvent } from "./dispatch-events";

export interface LegionEventPayload {
  type: string;
  [key: string]: unknown;
}

/** An effect a reducer derives from one event. For a durable event (Dispatch issue events and GitHub check settlement alike), every effect dispatches (and a 404 no-holder is recorded) before the reducer's mutation is saved and the message acks; a failure anywhere in that sequence is fatal (see `events.ts`). `dequeue` removes a waiting issue's admission-queue entry and `queued` tree record (`ProcessManager.dequeue`) inside the same transaction. `probe-worker` probes one located, ready-confirmed worker claim's recorded process through the runtime (`ProcessManager.probeWorkerClaim`) and runs the worker death path on a dead verdict; emitted only by resync (LEGION-179). A `log` effect only writes one line to the daemon's log and cannot fail. */
export type Effect =
  | { kind: "publish"; role: string; payload: LegionEventPayload }
  | { kind: "controller"; payload: LegionEventPayload }
  | { kind: "probe"; tree: IssueKey }
  | { kind: "probe-worker"; token: string }
  | { kind: "linger"; tree: IssueKey }
  | { kind: "admit"; issue: IssueKey }
  | { kind: "dequeue"; issue: IssueKey }
  | { kind: "log"; message: string };

export interface EnvelopeJson {
  event_id: string;
  issued_at: number;
  payload?: string | Record<string, unknown>;
  payload_summary?: string;
  [key: string]: unknown;
}

export interface ReducerConfig {
  maxFixAttempts: number;
}

export type CiEmission =
  | { type: "ci-green"; sha: string }
  | { type: "ci-settled-red"; sha: string; failing: string[] };

/** The head's CI outcome: failing check runs and failing commit statuses (see the CI view contract). */
export interface CiOutcome {
  verdict: PrState["verdict"];
  failing: string[];
  failingStatuses: string[];
}

export interface CiSettlementInput extends CiOutcome {
  settledAt: number;
}
function sameStringMultiset(left: readonly string[], right: readonly string[]): boolean {
  if (left.length !== right.length) return false;
  const sortedLeft = [...left].sort();
  const sortedRight = [...right].sort();
  return sortedLeft.every((value, index) => value === sortedRight[index]);
}

/*
 * CI view contract. Two sources describe a head's checks:
 *
 * - A live settlement (Envoy listener) is a possibly incomplete view — a missed
 *   webhook, a record recreated after the KV TTL. It decides the outcome of
 *   every name it reports: the names in its attempt set plus the names it
 *   lists as failing (a legacy check or status context has no run id). Every
 *   other name keeps its last known outcome; the head is red while any failure
 *   remains (`effectiveOutcome`).
 * - A GitHub rollup read is a complete view: its failing check runs and failing
 *   commit statuses replace the stored ones wholesale. Statuses have no check
 *   run and are invisible to the listener, so they are kept apart
 *   (`failingStatuses`): a check run that shares a status's name cannot retire
 *   it — only GitHub does (resync).
 *
 * An accepted attempt set from either source merges into the stored fence:
 * the per-name maximum over the union of names, nothing pruned
 * (`writeCiFence`). The fence is the head's high-watermark, so a name an
 * incomplete view omitted cannot later reappear as new. Ordering compares only
 * the names the incoming view carries (`compareAttemptSets`).
 */

/** An attempt set: the latest GitHub check-run id per check name, sorted by name. */
export type AttemptSet = readonly CheckRunRef[];

export type AttemptSetOrder = "newer" | "equal" | "older" | "mixed";

/**
 * Orders an incoming attempt set against the stored one, per shared name:
 * `newer` when no shared id is lower and some id is higher or a name is new;
 * `equal` when every shared id matches and no name is new; `older` when no
 * shared id is higher and some is lower; `mixed` otherwise. Names only in the
 * stored set are ignored (a check can vanish from GitHub's view; a recreated
 * listener record starts sparse). Within one producer record per-name ids never
 * decrease, and the stored fence is the per-name maximum over every accepted
 * view (`mergeAttemptSets`), so the fence moves in one direction without
 * clocks: a superseded attempt is `newer` whatever its completion time; a
 * delayed older observation is `equal` or `older`.
 */
export function compareAttemptSets(stored: AttemptSet, incoming: AttemptSet): AttemptSetOrder {
  const known = new Map(stored.map((run) => [run.name, run.id]));
  let higher = false;
  let lower = false;
  for (const run of incoming) {
    const id = known.get(run.name);
    if (id === undefined || run.id > id) higher = true;
    else if (run.id < id) lower = true;
  }
  if (higher && lower) return "mixed";
  if (higher) return "newer";
  return lower ? "older" : "equal";
}

/** A live settlement offered to the per-head fence: its ordering identity and its outcome. */
export interface SettlementCandidate {
  readonly checkRuns: AttemptSet;
  readonly generation: number;
  readonly snapshot: string;
  /** Compared only when a terminal GitHub read holds the tie at an equal attempt set. */
  readonly verdict: PrState["verdict"];
  readonly failing: readonly string[];
}

export type SettlementClassification = "stale" | "duplicate" | "conflict" | "newer" | "refresh";

/**
 * Classifies a live settlement against the per-head fence. The attempt set
 * orders every source (`compareAttemptSets`); at an equal set the listener's
 * generation orders its own settlements (a GitHub-authored fence has no
 * generation and orders below every live one). When a terminal GitHub read
 * holds the tie at that set (`ciReconciled`), a higher generation is accepted
 * only when its effective outcome (`effectiveOutcome`) agrees — a `refresh`
 * of the listener identity that leaves GitHub's authority in place; a
 * disagreeing one is stale until GitHub reads the set again. Authority is
 * released when the set advances or a pending GitHub read clears it.
 */
export function classifySettlement(
  pr: PrState,
  incoming: SettlementCandidate
): SettlementClassification {
  if (pr.ciCheckRuns === null) return "newer";
  switch (compareAttemptSets(pr.ciCheckRuns, incoming.checkRuns)) {
    case "newer":
      return "newer";
    case "older":
      return "stale";
    case "mixed":
      return "conflict";
    case "equal":
      break;
  }
  if (pr.ciSettlementGeneration !== null) {
    if (incoming.generation < pr.ciSettlementGeneration) return "stale";
    if (incoming.generation === pr.ciSettlementGeneration) {
      return incoming.snapshot === pr.ciSnapshot ? "duplicate" : "conflict";
    }
  }
  if (!pr.ciReconciled) return "newer";
  const effective = effectiveOutcome(pr, incoming);
  return effective.verdict === pr.verdict && sameStringMultiset(effective.failing, pr.failing)
    ? "refresh"
    : "stale";
}

/**
 * The outcome a live settlement establishes for the head (see the CI view
 * contract above): the names it reports — its attempt set plus its failing
 * names — take its outcome; every other name keeps its last known outcome, and
 * the stored commit-status failures stand, keeping the head red.
 */
export function effectiveOutcome(
  pr: PrState,
  incoming: Pick<SettlementCandidate, "checkRuns" | "verdict" | "failing">
): CiOutcome {
  const reported = new Set([...incoming.checkRuns.map((run) => run.name), ...incoming.failing]);
  const failing = [...incoming.failing, ...pr.failing.filter((name) => !reported.has(name))];
  // Commit statuses are invisible to the listener: the stored ones stand as they are.
  const failingStatuses = [...pr.failingStatuses];
  if (failing.length > 0 || failingStatuses.length > 0)
    return { verdict: "red", failing, failingStatuses };
  return { verdict: incoming.verdict, failing: [], failingStatuses };
}

export interface CiFence {
  readonly checkRuns: AttemptSet;
  /** null for a fence GitHub's rollup authored (resync); the listener's state version otherwise. */
  readonly generation: number | null;
  readonly snapshot: string | null;
}

/**
 * What a rollup read from GitHub may do to the stored fence: `advance` it (a
 * newer attempt set, or nothing fenced yet — the set merges in and the
 * identity becomes GitHub's, with no listener generation); `apply` its verdict
 * at an equal set, keeping the listener identity for duplicate detection;
 * apply it `unfenced` when neither the rollup nor the stored fence has a check
 * run; or be skipped as `stale` (an older set; or no check runs where some are
 * fenced) or a `conflict` (a mixed set). The live counterpart is
 * `classifySettlement`.
 */
export type GitHubFenceEffect = "advance" | "apply" | "unfenced" | "stale" | "conflict";

export function acceptGitHubFence(pr: PrState, checkRuns: AttemptSet): GitHubFenceEffect {
  const fenced = pr.ciCheckRuns !== null && pr.ciCheckRuns.length > 0;
  if (checkRuns.length === 0) return fenced ? "stale" : "unfenced";
  if (pr.ciCheckRuns === null) return "advance";
  switch (compareAttemptSets(pr.ciCheckRuns, checkRuns)) {
    case "newer":
      return "advance";
    case "equal":
      return "apply";
    case "older":
      return "stale";
    case "mixed":
      return "conflict";
  }
}

/**
 * Writes a fence its caller already accepted (`classifySettlement` or
 * `acceptGitHubFence`): the attempt set merges into the stored one (see the
 * CI view contract above); the identity is the caller's.
 */
export function writeCiFence(pr: PrState, fence: CiFence): void {
  pr.ciCheckRuns = mergeAttemptSets(pr.ciCheckRuns ?? [], fence.checkRuns);
  pr.ciSettlementGeneration = fence.generation;
  pr.ciSnapshot = fence.snapshot;
}

/** Per-name maximum over the union of two attempt sets, in canonical name order. */
function mergeAttemptSets(stored: AttemptSet, incoming: AttemptSet): CheckRunRef[] {
  const merged = new Map(stored.map((run) => [run.name, run.id]));
  for (const run of incoming) {
    const known = merged.get(run.name);
    if (known === undefined || run.id > known) merged.set(run.name, run.id);
  }
  return sortedCheckRunRefs(merged);
}

/** Refreshes the listener identity at an unchanged attempt set (`classifySettlement` returned "refresh"). */
export function refreshCiIdentity(
  pr: PrState,
  generation: number,
  snapshot: string,
  settledAt: number
): void {
  pr.ciSettlementGeneration = generation;
  pr.ciSnapshot = snapshot;
  pr.ciSettledAt = settledAt;
}

/** The head and CI fields a reconciliation must find unchanged before it may apply — every field the read may write, plus the head's lifecycle clock: one definition for capture and comparison. */
export type CiSnapshot = Pick<
  PrState,
  | "headSha"
  | "headUpdatedAt"
  | "verdict"
  | "failing"
  | "failingStatuses"
  | "ciSettledAt"
  | "ciCheckRuns"
  | "ciSettlementGeneration"
  | "ciSnapshot"
  | "ciReconciled"
>;

export function ciSnapshot(pr: PrState): CiSnapshot {
  return {
    headSha: pr.headSha,
    headUpdatedAt: pr.headUpdatedAt,
    verdict: pr.verdict,
    failing: [...pr.failing],
    failingStatuses: [...pr.failingStatuses],
    ciSettledAt: pr.ciSettledAt,
    ciCheckRuns: pr.ciCheckRuns === null ? null : pr.ciCheckRuns.map((run) => ({ ...run })),
    ciSettlementGeneration: pr.ciSettlementGeneration,
    ciSnapshot: pr.ciSnapshot,
    ciReconciled: pr.ciReconciled,
  };
}

function sameAttemptSet(left: AttemptSet | null, right: AttemptSet | null): boolean {
  if (left === null || right === null) return left === right;
  return (
    left.length === right.length &&
    left.every((run, index) => run.name === right[index]?.name && run.id === right[index]?.id)
  );
}

export function ciSnapshotEquals(pr: PrState, snapshot: CiSnapshot): boolean {
  return (
    pr.headSha === snapshot.headSha &&
    pr.headUpdatedAt === snapshot.headUpdatedAt &&
    pr.verdict === snapshot.verdict &&
    pr.ciSettledAt === snapshot.ciSettledAt &&
    sameAttemptSet(pr.ciCheckRuns, snapshot.ciCheckRuns) &&
    pr.ciSettlementGeneration === snapshot.ciSettlementGeneration &&
    pr.ciSnapshot === snapshot.ciSnapshot &&
    pr.ciReconciled === snapshot.ciReconciled &&
    pr.failing.length === snapshot.failing.length &&
    pr.failing.every((name, index) => name === snapshot.failing[index]) &&
    pr.failingStatuses.length === snapshot.failingStatuses.length &&
    pr.failingStatuses.every((name, index) => name === snapshot.failingStatuses[index])
  );
}

/** A rerun in flight, or a cancelled-only settlement: a green verdict is no longer certified. Red is preserved. */
export function uncertifyCiVerdict(pr: PrState): void {
  if (pr.verdict !== "green") return;
  pr.verdict = null;
  pr.failing = [];
  pr.failingStatuses = [];
}

function ciVerdictEmissions(
  pr: PrState,
  verdict: PrState["verdict"],
  failing: string[],
  failingStatuses: string[]
): CiEmission[] {
  if (verdict === null) {
    uncertifyCiVerdict(pr);
    return [];
  }

  const priorVerdict = pr.verdict;
  const priorFailing = [...pr.failing, ...pr.failingStatuses];
  pr.verdict = verdict;
  pr.failing = verdict === "red" ? failing : [];
  pr.failingStatuses = verdict === "red" ? failingStatuses : [];
  const nowFailing = [...pr.failing, ...pr.failingStatuses];
  if (priorVerdict === verdict && sameStringMultiset(priorFailing, nowFailing)) return [];
  return verdict === "red"
    ? [{ type: "ci-settled-red", failing: nowFailing, sha: pr.headSha }]
    : [{ type: "ci-green", sha: pr.headSha }];
}

export function settleCiVerdict(
  state: LegionState,
  pr: PrState,
  input: CiSettlementInput,
  config: ReducerConfig
): Effect[] {
  pr.ciSettledAt = input.settledAt;
  return collapseClosedTreeWakes(
    ciVerdictEmissions(pr, input.verdict, input.failing, input.failingStatuses).flatMap(
      (emission) => [
        ...routeActive(state, pr.key, emission),
        ...reduceCiEmission(state, pr.repo, pr.number, emission, config),
      ]
    )
  );
}

type JsonRecord = Record<string, unknown>;

/** The controller wake payload for GitHub activity on a tree that has already closed. */
export interface ClosedTreeActivityPayload extends LegionEventPayload {
  type: "closed-tree-activity";
  issue: IssueKey;
  root: IssueKey;
  event: LegionEventPayload;
}

function asRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as JsonRecord)
    : undefined;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function numberValue(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isSafeInteger(value)) return value;
  if (typeof value === "string" && /^\d+$/.test(value)) {
    const parsed = Number(value);
    return Number.isSafeInteger(parsed) ? parsed : undefined;
  }
  return undefined;
}

function payloadFrom(envelope: EnvelopeJson): JsonRecord | undefined {
  if (typeof envelope.payload !== "string") return asRecord(envelope.payload);
  try {
    return asRecord(JSON.parse(envelope.payload));
  } catch {
    return undefined;
  }
}

function treeFor(state: LegionState, key: IssueKey): TreeState | undefined {
  let current = key;
  const visited = new Set<IssueKey>();
  while (!visited.has(current)) {
    visited.add(current);
    const tree = state.trees[current];
    if (tree) return tree;
    const parent = state.issues[current]?.parent;
    if (!parent) return undefined;
    current = parent;
  }
  return undefined;
}

/** The controller wake for activity on a tree that has already closed, shared by `routeActive` and
 * `routeArchitect`. */
function closedTreeWake(issue: IssueKey, tree: TreeState, payload: LegionEventPayload): Effect {
  return {
    kind: "controller",
    payload: { type: "closed-tree-activity", issue, root: tree.root, event: payload },
  };
}

/** `issue`'s active phase role, or undefined when no phase is active or it has completed. */
function activePhaseRole(state: LegionState, issue: IssueKey): LegionRole | undefined {
  const phase = state.phases[issue];
  if (!phase) return undefined;
  const role = assertKnownPhase(issue, phase);
  return phase.completed ? undefined : role;
}

/** The token of the architect that owns `issue`: its own sub-architect when one is claimed, else
 * the nearest claimed ancestor, then the tree root. */
function architectToken(state: LegionState, issue: IssueKey): string {
  return roleToken(state.project, owningArchitect(state, issue), "architect");
}

/**
 * Routes an event about `issue` to the role that can act on it: the issue's active phase worker
 * (`activePhaseRole` -- the one place a new active phase is written is `promptExistingWorker`
 * delivering an architect assignment, `kind: "assignment"`: a task prompted into a live worker,
 * or delivered from `pendingAssignment` at `/worker/ready`; a bare registration, a reconnect, or
 * a daemon catch-up never writes it, and `handlePhaseComplete` only deletes, restores, or marks
 * it completed), else the architect that owns the issue (`architectToken`: its own sub-architect
 * when one is claimed, else the nearest claimed ancestor, else the tree root) -- which includes a
 * phase whose worker already reported completion (`phase.completed`, set by
 * `handlePhaseComplete` when no architect was live to receive it) as well as no phase at all. A
 * closed tree instead wakes the controller so a human can decide whether to resume it. For a wake
 * only an architect can act on, use `routeArchitect`.
 */
export function routeActive(
  state: LegionState,
  issue: IssueKey,
  payload: LegionEventPayload
): Effect[] {
  const tree = treeFor(state, issue);
  if (!tree) return [];
  if (tree.status === "closed") return [closedTreeWake(issue, tree, payload)];
  const active = activePhaseRole(state, issue);
  const token = active ? roleToken(state.project, issue, active) : architectToken(state, issue);
  return [{ kind: "publish", role: token, payload }];
}

/**
 * Routes a wake only an architect can act on -- a child's lifecycle (`child-adopted`,
 * `child-status`, `child-closed`, `children-complete`) and the design gate's verdicts
 * (`design-approved`, `design-changes-requested`) -- to the architect that owns `issue`
 * (`architectToken`), never to its active phase worker: a planner cannot spawn a sub-architect or
 * release a wave. Shares `routeActive`'s closed-tree controller wake and its unknown-phase throw
 * (LEGION-86).
 */
export function routeArchitect(
  state: LegionState,
  issue: IssueKey,
  payload: LegionEventPayload
): Effect[] {
  const tree = treeFor(state, issue);
  if (!tree) return [];
  if (tree.status === "closed") return [closedTreeWake(issue, tree, payload)];
  const phase = state.phases[issue];
  if (phase) assertKnownPhase(issue, phase);
  return [{ kind: "publish", role: architectToken(state, issue), payload }];
}

/**
 * One incoming GitHub or CI event can fan through multiple `routeActive`
 * calls in a single reducer pass — a primary route plus a derived one (a
 * review and its `pr-ready` follow-on, a child closing and the
 * `children-complete` it triggers, a CI settlement and the ready/blocked
 * signal it derives). On a closed tree each call independently wakes the
 * controller, so one event can produce several `closed-tree-activity`
 * effects for the same tree. Collapses them to the first (the primary
 * event) per tree; every other effect kind passes through unchanged.
 */
function collapseClosedTreeWakes(effects: Effect[]): Effect[] {
  const wokenRoots = new Set<IssueKey>();
  return effects.filter((effect) => {
    if (effect.kind !== "controller" || effect.payload.type !== "closed-tree-activity") return true;
    const root = effect.payload.root as IssueKey;
    if (wokenRoots.has(root)) return false;
    wokenRoots.add(root);
    return true;
  });
}

function openChildren(state: LegionState, parent: IssueNode): number {
  return parent.children.filter((key) => state.issues[key]?.status !== "done").length;
}

/** `payload.legion_footer` is Envoy's flag for the substring on the uncapped raw body
 * (`<!-- legion:`, Legion's own worker footer) — capping the body at 2048 runes can cut the
 * marker off, so the flag is checked first and the capped `body` is only a fallback for an
 * older listener that never set it. */
function filtered(payload: JsonRecord): boolean {
  return (
    payload.legion_footer === "true" || (stringValue(payload.body) ?? "").includes("<!-- legion:")
  );
}

export function issueForBranch(branch: string): IssueKey | undefined {
  const match = /^legion\/([A-Z][A-Z0-9]*-[0-9]+)$/.exec(branch);
  return match?.[1];
}

export function issueForPrBody(body: string): IssueKey | undefined {
  const match = /^Dispatch: ([A-Z][A-Z0-9]*-[0-9]+)$/m.exec(body);
  return match?.[1];
}

function updatedAt(raw: JsonRecord): number | undefined {
  const value = stringValue(raw.updated_at);
  if (!value) return undefined;
  const timestamp = Date.parse(value);
  return Number.isNaN(timestamp) ? undefined : timestamp;
}

/**
 * True when `incoming` must not overwrite `applied`, the fence's
 * last-recorded observation: an out-of-order redelivery (an incoming clock
 * strictly older than the applied one), or — at an identical clock — a
 * webhook observation that disagrees with a resync-sourced value (a board
 * GraphQL, CI-status, or merge-gate read). GitHub's authoritative resync
 * read wins that tie regardless of arrival order; two observations from the
 * same source at the same clock are a legitimate same-second sequence and
 * still apply. Undefined on either side means there is no fence to apply,
 * so nothing is ever superseded.
 */
export function supersededBy(
  incoming: { updatedAt: number | undefined; source: UpdateSource },
  applied: { updatedAt: number | undefined; source: UpdateSource }
): boolean {
  if (incoming.updatedAt === undefined || applied.updatedAt === undefined) return false;
  if (incoming.updatedAt < applied.updatedAt) return true;
  if (incoming.updatedAt > applied.updatedAt) return false;
  return applied.source === "resync" && incoming.source === "webhook";
}

function registerPr(
  state: LegionState,
  repo: string,
  number: number,
  branch: string | undefined,
  body: string | undefined,
  sha: string | undefined,
  headUpdatedAt: number | undefined,
  source: UpdateSource
): PrState | undefined {
  const key =
    (branch ? issueForBranch(branch) : undefined) ?? (body ? issueForPrBody(body) : undefined);
  if (!key || !state.issues[key] || !sha) return undefined;
  const prKey = `${repo}#${number}`;
  const pr: PrState = {
    key,
    repo: repo as `${string}/${string}`,
    number,
    headSha: sha,
    ...(headUpdatedAt === undefined ? {} : { headUpdatedAt, headUpdatedAtSource: source }),
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
  state.prs[prKey] = pr;
  if (branch) state.prByBranch[`${repo}@${branch}`] = prKey;
  return pr;
}

/**
 * Registers a new `PrState` for an `opened` event or a `synchronize` that
 * finds no existing record — or returns `undefined` without registering
 * anything for a redelivery or stale observation: an existing record at
 * the same or an older clock (`registerPr` would wipe the CI/review state
 * a settlement already established on it — `opened` fires exactly once
 * per PR, so a later delivery at the same clock is that same event
 * redelivered, not a new observation), or a clock at or before this PR's
 * close tombstone (an `opened`/`synchronize` at or older than the close it
 * resurrects a PR this state already recorded as closed-unmerged — unlike
 * the fence above, an equal clock here is still the close winning, not a
 * legitimate same-second reopen). Clears the tombstone on success.
 */
function registerPrFenced(
  state: LegionState,
  repo: string,
  number: number,
  branch: string | undefined,
  body: string | undefined,
  sha: string | undefined,
  headUpdatedAt: number | undefined,
  source: UpdateSource
): PrState | undefined {
  const prKey = `${repo}#${number}`;
  const existing = state.prs[prKey];
  if (
    headUpdatedAt !== undefined &&
    existing?.headUpdatedAt !== undefined &&
    headUpdatedAt <= existing.headUpdatedAt
  ) {
    return undefined;
  }
  const tombstonedAt = state.prTombstones[prKey];
  if (headUpdatedAt !== undefined && tombstonedAt !== undefined && headUpdatedAt <= tombstonedAt) {
    return undefined;
  }
  const pr = registerPr(state, repo, number, branch, body, sha, headUpdatedAt, source);
  if (pr) delete state.prTombstones[prKey];
  return pr;
}

/** A new head arrived (the synchronize webhook, or resync's GitHub read). It counts as a fix
 * attempt when the prior verdict was red — unless the push webhook already classified this exact
 * sha handoff-only (`pendingPush`, consumed here whatever it says); `headCounted` records the
 * decision so a handoff-only push webhook arriving later can take the attempt back (see `push`). A
 * pending slot naming a different sha describes a newer push whose synchronize has not arrived and
 * is left in place. */
export function resetPrHead(pr: PrState, headSha: string): void {
  const pending = pr.pendingPush;
  const handoffOnly = pending?.sha === headSha && pending.handoffOnly;
  if (pending?.sha === headSha) delete pr.pendingPush;
  if (pr.verdict === "red" && !handoffOnly) {
    pr.fixAttempts += 1;
    pr.headCounted = true;
  } else {
    delete pr.headCounted;
  }
  pr.headSha = headSha;
  pr.verdict = null;
  pr.failing = [];
  pr.failingStatuses = [];
  pr.ciSettledAt = null;
  pr.ciCheckRuns = null;
  pr.ciSettlementGeneration = null;
  pr.ciSnapshot = null;
  pr.ciReconciled = false;
  delete pr.reviewDecision;
}

const HANDOFF_PATH_PREFIX = ".legion/";

type PushClassification = { handoffOnly: true } | { handoffOnly: false; unknown?: string };

/** Classifies a normalized push from the file lists the listener forwards (`changed_paths`,
 * `changed_paths_truncated` — the push case of `githubPayload` in normalize.go). Handoff-only
 * means every path is under `.legion/`; a push mixing in any other path is a real change.
 * `unknown` names why the push could not be classified at all — such a push counts as a fix
 * attempt exactly as before these fields existed. `changed_paths_truncated` is read first: the
 * listener emits it on every push, whereas `payloadJSON` drops an empty `changed_paths`, so that
 * key's absence alone cannot tell an old listener from a push listing no commits. */
function classifyPush(payload: JsonRecord): PushClassification {
  const truncated = stringValue(payload.changed_paths_truncated);
  if (truncated === undefined) {
    return { handoffOnly: false, unknown: "changed_paths absent (listener predates LEGION-33)" };
  }
  if (truncated === "true") {
    return { handoffOnly: false, unknown: "changed_paths truncated at 100" };
  }
  if (truncated !== "false") {
    return { handoffOnly: false, unknown: `changed_paths_truncated=${truncated} unrecognised` };
  }
  const changedPaths = stringValue(payload.changed_paths);
  if (!changedPaths) return { handoffOnly: false, unknown: "no commits listed" };
  return changedPaths.split("\n").every((path) => path.startsWith(HANDOFF_PATH_PREFIX))
    ? { handoffOnly: true }
    : { handoffOnly: false };
}

/** A push webhook on a branch with a registered PR (`prByBranch` is the whole filter: a push on
 * `main`, a tag, or a legion branch with no PR maps to nothing). Its classification either takes
 * back the attempt the current head was counted for (its synchronize arrived first — the take-back
 * also forgets a `pr-blocked` published for that count, so the next real fix publishes it again)
 * or is remembered for the head that has not arrived yet (`pendingPush`, latest push wins). A push
 * that cannot be classified counts as before and, when that count is real, says so through a `log`
 * effect. */
function push(state: LegionState, payload: JsonRecord): Effect[] | undefined {
  if (payload.kind !== "push") return undefined;
  const repo = stringValue(payload.repo);
  const ref = stringValue(payload.ref);
  const after = stringValue(payload.after);
  if (!repo || !ref?.startsWith("refs/heads/") || !after) return [];
  const branch = ref.slice("refs/heads/".length);
  const prKey = state.prByBranch[`${repo}@${branch}`];
  const pr = prKey === undefined ? undefined : state.prs[prKey];
  if (!pr) return [];
  const classification = classifyPush(payload);
  // Whether this push's count is real — judged before any mutation below: the current head's
  // recorded decision, or, for a head still to arrive, the verdict `resetPrHead` will see.
  const counted = pr.headSha === after ? pr.headCounted === true : pr.verdict === "red";
  if (pr.headSha === after) {
    if (classification.handoffOnly && pr.headCounted) {
      if (pr.blockedAttempts === pr.fixAttempts) delete pr.blockedAttempts;
      pr.fixAttempts -= 1;
      delete pr.headCounted;
    }
  } else {
    pr.pendingPush = { sha: after, handoffOnly: classification.handoffOnly };
  }
  if (classification.handoffOnly || classification.unknown === undefined || !counted) return [];
  return [
    {
      kind: "log",
      message: `fix attempt: ${pr.repo}#${pr.number} (${pr.key}) push ${after} to ${branch} could not be classified as handoff-only (${classification.unknown}); it counts against max_fix_attempts`,
    },
  ];
}

function removeBranchMappings(state: LegionState, prKey: string): void {
  for (const [branch, mapped] of Object.entries(state.prByBranch)) {
    if (mapped === prKey) delete state.prByBranch[branch];
  }
}

/** A `pull_request_review_comment` (`path` present) or `issue_comment` on a PR (`path` absent) —
 * both arrive from Envoy as `kind: "comment"`. A comment on a plain GitHub issue
 * (`parent_kind !== "pr"`) is never acted on: the daemon does not read or write GitHub issues. */
function prComment(state: LegionState, payload: JsonRecord): Effect[] | undefined {
  if (payload.kind !== "comment") return undefined;
  const repo = stringValue(payload.repo);
  const number = numberValue(payload.number);
  if (!repo || number === undefined) return [];
  if (payload.action !== "created" || payload.parent_kind !== "pr" || filtered(payload)) return [];
  const pr = state.prs[`${repo}#${number}`];
  if (!pr) return [];
  const author = stringValue(payload.author) ?? "";
  const body = stringValue(payload.body) ?? "";
  const url = stringValue(payload.url) ?? "";
  const path = stringValue(payload.path);
  return routeActive(
    state,
    pr.key,
    path !== undefined
      ? { type: "pr-review-comment", author, body, path, url }
      : { type: "pr-comment", author, body, url }
  );
}

function review(state: LegionState, payload: JsonRecord): Effect[] | undefined {
  if (payload.kind !== "review") return undefined;
  const repo = stringValue(payload.repo);
  const number = numberValue(payload.number);
  if (!repo || number === undefined || payload.action !== "submitted") return [];
  const pr = state.prs[`${repo}#${number}`];
  if (!pr) return [];
  const decision = (stringValue(payload.state) ?? "").toLowerCase();
  const commitId = stringValue(payload.commit_id);
  // Absent `commit_id` (an older listener) means the delivery cannot be pinned to a head at
  // all: the approval is never recorded, but the phase worker still hears about the review.
  const isCurrentHead = commitId !== undefined && commitId === pr.headSha;
  const prior = pr.reviewDecision;
  // Approval is head-gated: it feeds `pr-ready`, which must only ever fire for an approval of
  // the exact commit that would merge. Changes requested is not — a reviewer
  // legitimately pins its review to the implementation commit it read rather than to a later
  // handoff commit, and any such verdict still means the PR is not reviewer-clean. Safe to
  // record from any commit because `resetPrHead` drops the decision on every new head, so a
  // verdict never outlives the round it was given for.
  if (decision === "changes_requested" || (isCurrentHead && decision === "approved")) {
    pr.reviewDecision = decision;
  }
  const result = routeActive(state, pr.key, {
    type: "pr-review",
    state: decision,
    author: stringValue(payload.author) ?? "",
    body: stringValue(payload.body) ?? "",
  });
  if (isCurrentHead && decision === "approved" && prior !== "approved" && pr.verdict === "green") {
    result.push(...routeActive(state, pr.key, { type: "pr-ready", pr: number }));
  }
  return result;
}

function pullRequest(
  state: LegionState,
  payload: JsonRecord,
  source: UpdateSource
): Effect[] | undefined {
  if (payload.kind !== "pr") return undefined;
  const repo = stringValue(payload.repo);
  const number = numberValue(payload.number);
  if (!repo || number === undefined) return [];
  const prKey = `${repo}#${number}`;
  const branch = stringValue(payload.head_ref);
  const body = stringValue(payload.body);
  const sha = stringValue(payload.head_sha);
  const headUpdatedAt = updatedAt(payload);

  if (payload.action === "opened") {
    const pr = registerPrFenced(state, repo, number, branch, body, sha, headUpdatedAt, source);
    if (!pr) return [];
    return routeActive(state, pr.key, {
      type: "pr-opened",
      pr: number,
      url: stringValue(payload.url) ?? "",
    });
  }

  let pr: PrState | undefined = state.prs[prKey];
  if (!pr && payload.action === "synchronize") {
    pr = registerPrFenced(state, repo, number, branch, body, sha, headUpdatedAt, source);
  }
  if (!pr) return [];
  if (payload.action === "synchronize") {
    if (
      supersededBy(
        { updatedAt: headUpdatedAt, source },
        { updatedAt: pr.headUpdatedAt, source: pr.headUpdatedAtSource ?? "webhook" }
      )
    ) {
      return [];
    }
    if (!sha) return [];
    if (pr.headSha === sha) {
      if (
        headUpdatedAt !== undefined &&
        (pr.headUpdatedAt === undefined || headUpdatedAt > pr.headUpdatedAt)
      ) {
        pr.headUpdatedAt = headUpdatedAt;
        pr.headUpdatedAtSource = source;
      }
      return [];
    }
    resetPrHead(pr, sha);
    if (headUpdatedAt === undefined) {
      delete pr.headUpdatedAt;
      delete pr.headUpdatedAtSource;
    } else {
      pr.headUpdatedAt = headUpdatedAt;
      pr.headUpdatedAtSource = source;
    }
    return [];
  }
  if (payload.action === "closed") {
    // Either way the PR is finished: its record and branch mapping go, and a tombstone keeps
    // a stale "opened" redelivery from recreating it. The wake differs -- a merge is the
    // architect's cue to send the implementer to check the change in production and to sign off
    // once that record exists, an unmerged close its cue to decide whether the work is reopened,
    // reassigned, or cancelled.
    delete state.prs[prKey];
    removeBranchMappings(state, prKey);
    if (headUpdatedAt !== undefined) state.prTombstones[prKey] = headUpdatedAt;
    if (payload.merged === "true") {
      return routeActive(state, pr.key, {
        type: "pr-merged",
        pr: number,
        mergeCommitSha: stringValue(payload.merge_commit_sha) ?? "",
      });
    }
    return routeActive(state, pr.key, { type: "pr-closed-unmerged", pr: number });
  }
  return [];
}

export function reduceGithubEvent(
  state: LegionState,
  topic: string,
  envelope: EnvelopeJson,
  _config: ReducerConfig
): Effect[] {
  if (/^notifications\.github\.[^.]+\.[^.]+\.pr\.\d+\.checks$/.test(topic)) return [];
  const payload = payloadFrom(envelope);
  if (!payload) return [];
  // "resync" is the daemon's own sentinel topic for reducer input (board GraphQL
  // reads, not an external webhook) — GitHub's authoritative read wins a
  // same-clock tie against a webhook (see `supersededBy`).
  const source: UpdateSource = topic === "resync" ? "resync" : "webhook";
  // GitHub carries PRs, checks, reviews, and branch pushes only (D1/D2): the daemon never reads
  // or writes a GitHub issue, so an `issues`/`projects_v2_item`/`sub_issue` webhook produces no
  // effect.
  return collapseClosedTreeWakes(
    prComment(state, payload) ??
      review(state, payload) ??
      pullRequest(state, payload, source) ??
      push(state, payload) ??
      []
  );
}

export function reduceCiEmission(
  state: LegionState,
  repo: string,
  number: number,
  emission: CiEmission,
  config: ReducerConfig
): Effect[] {
  const pr = state.prs[`${repo}#${number}`];
  if (!pr || pr.headSha !== emission.sha || pr.ciSettledAt === null) return [];
  if (emission.type === "ci-green") {
    return pr.reviewDecision === "approved"
      ? routeActive(state, pr.key, { type: "pr-ready", pr: number })
      : [];
  }
  if (pr.fixAttempts < config.maxFixAttempts || pr.blockedAttempts === pr.fixAttempts) return [];
  // Published once per exhausted count, never again on a later red verdict for the same count;
  // recorded whether or not a tree was found to route to (the publish is this reducer's decision).
  pr.blockedAttempts = pr.fixAttempts;
  return routeActive(state, pr.key, { type: "pr-blocked", pr: number, attempts: pr.fixAttempts });
}

function isIssueStatus(value: string): value is IssueStatus {
  return (ISSUE_STATUSES as readonly string[]).includes(value);
}

/** The fields every `issue.*` Dispatch event's payload carries (the full issue, not a diff). */
interface DispatchIssuePayload {
  key: IssueKey;
  title: string;
  status: IssueStatus;
  parent: IssueKey | null;
}

function dispatchIssuePayload(payload: unknown, key: IssueKey): DispatchIssuePayload | undefined {
  const raw = asRecord(payload);
  const title = stringValue(raw?.title);
  const status = stringValue(raw?.status);
  if (!raw || !title || !status || !isIssueStatus(status)) return undefined;
  const parent = typeof raw.parent === "string" ? raw.parent : null;
  return { key, title, status, parent };
}

function applyDispatchIssueFields(node: IssueNode, issue: DispatchIssuePayload): void {
  node.title = issue.title;
  node.status = issue.status;
}

/** Creates a `issue.created` node. Idempotent against a redelivered or reordered duplicate that
 * slips past the outer seq fence (e.g. an older create landing after a resync-derived seq bump
 * for the same key): once a node exists for this key, this is a no-op — no field is overwritten,
 * no controller/child-adopted effect is re-emitted — never a replace, since a later event (a
 * status update, another child's adoption) may have already advanced fields a blind replace would
 * roll back. */
function reduceIssueCreated(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const issue = dispatchIssuePayload(event.payload, event.key);
  if (!issue) return [];
  if (state.issues[issue.key]) return [];
  const node: IssueNode = {
    key: issue.key,
    title: issue.title,
    children: [],
    status: issue.status,
  };
  if (issue.parent) node.parent = issue.parent;
  state.issues[issue.key] = node;
  if (!issue.parent) {
    return [{ kind: "controller", payload: { type: "triage", issue: issue.key } }];
  }
  return childAdopted(state, issue.parent, issue.key);
}

/** The wake for a child that has (re)entered `parent`'s tree: emitted by `issue.created` with a
 * parent, and by the boot repair that removes a pre-LEGION-57 root tree for a child
 * (`ProcessManager.adoptOwnerlessChildTrees`, published from `index.ts`). Records the child on the
 * parent's `children` when it is not there yet and wakes the parent's owning architect
 * (`routeArchitect`); nothing when the parent has no node. */
export function childAdopted(state: LegionState, parent: IssueKey, child: IssueKey): Effect[] {
  const parentNode = state.issues[parent];
  if (!parentNode) return [];
  if (!parentNode.children.includes(child)) parentNode.children.push(child);
  return routeArchitect(state, parent, {
    type: "child-adopted",
    child,
    remaining: openChildren(state, parentNode),
  });
}

/** The `dequeue` effect for an issue whose new `status` has taken it out of the waiting line
 * (`isStaleQueuedStatus`) while it still holds an `admission.queue` entry or a `queued` tree
 * record -- `ProcessManager.dequeue` removes both in the same durable step as the status change.
 * Nothing for any other status, and nothing for an issue that is not waiting: a redelivery finds
 * nothing to do, and an active or lingering tree belongs to the linger and close paths. */
function dequeueIfWaiting(state: LegionState, issue: IssueKey, status: IssueStatus): Effect[] {
  if (!isStaleQueuedStatus(status)) return [];
  const waiting = state.admission.queue.includes(issue) || state.trees[issue]?.status === "queued";
  return waiting ? [{ kind: "dequeue", issue }] : [];
}

function reduceIssueUpdated(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const issue = dispatchIssuePayload(event.payload, event.key);
  const node = issue ? state.issues[issue.key] : undefined;
  if (!issue || !node) return [];
  const statusChanged = node.status !== issue.status;
  applyDispatchIssueFields(node, issue);
  if (!statusChanged) return [];
  if (issue.status === "todo") return admitOnTodo(state, node);
  if (issue.status === "backlog" || issue.status === "icebox") {
    const tree = state.trees[issue.key];
    if (tree && tree.status === "active") return [{ kind: "linger", tree: issue.key }];
  }
  return dequeueIfWaiting(state, issue.key, issue.status);
}

/** A `todo` transition admits a root. A child under a live tree is owned by that tree's architect,
 * which runs it as a sub-architect phase worker (`spawn_worker` with `role: "architect"` -- the
 * spawn that writes its `in_progress`), so its `todo` is inert here: the parent's own
 * `child.status` event already wakes the parent's owning architect (`reduceChildStatus`). A child
 * with no live ancestor tree (never admitted, or lingering/closed) is an orphan and admits as a
 * root exactly like a parentless issue, with one log line naming the parent. */
function admitOnTodo(state: LegionState, node: IssueNode): Effect[] {
  if (!node.parent) return [{ kind: "admit", issue: node.key }];
  if (liveAncestorTree(state, node.key)) return [];
  const nearest = treeFor(state, node.parent);
  return [
    {
      kind: "log",
      message: `admitting ${node.key} as a root of its own: its parent ${node.parent} has no live tree${nearest ? ` (${nearest.root} is ${nearest.status})` : ""}`,
    },
    { kind: "admit", issue: node.key },
  ];
}

/** Lingers the closed issue's own active tree whether or not it has a parent -- a child admitted
 * as a root by a pre-LEGION-57 daemon releases its admission slot on close exactly like a root --
 * or, when that tree is still waiting for a slot instead, dequeues it (`dequeueIfWaiting`); then
 * wakes the parent's owning architect (`routeArchitect`: `child-closed`, and `children-complete`
 * on the last one). */
function reduceIssueClosed(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const issue = dispatchIssuePayload(event.payload, event.key);
  const node = issue ? state.issues[issue.key] : undefined;
  if (!issue || !node) return [];
  const wasOpen = node.status !== "done";
  applyDispatchIssueFields(node, issue);
  const own = state.trees[issue.key];
  const result: Effect[] =
    own?.status === "active"
      ? [{ kind: "linger", tree: issue.key }]
      : dequeueIfWaiting(state, issue.key, issue.status);
  if (!node.parent || !wasOpen) return result;
  const parent = state.issues[node.parent];
  if (!parent) return result;
  result.push(
    ...routeArchitect(state, node.parent, {
      type: "child-closed",
      child: issue.key,
      remaining: openChildren(state, parent),
    })
  );
  if (openChildren(state, parent) === 0) {
    result.push(...routeArchitect(state, node.parent, { type: "children-complete" }));
  }
  return result;
}

function reduceChildStatus(state: LegionState, event: DispatchIssueEvent): Effect[] {
  if (!state.issues[event.key]) return [];
  const raw = asRecord(event.payload);
  const child = stringValue(raw?.child_key);
  const from = stringValue(raw?.from);
  const to = stringValue(raw?.to);
  if (!child || !from || !to) return [];
  return routeArchitect(state, event.key, { type: "child-status", child, from, to });
}

/** The design gate an artifact event addresses: `state.gates[event.key]` only when the issue node
 * exists (a schema-valid but dangling gate record is never mutated or routed) and the gate names
 * the event's document — a review of any other document on the issue changes no gate. */
function gateFor(
  state: LegionState,
  event: DispatchIssueEvent,
  artifactId: string | undefined
): DesignGate | undefined {
  if (!state.issues[event.key] || artifactId === undefined) return undefined;
  const gate = state.gates[event.key];
  return gate && gate.artifactId === artifactId ? gate : undefined;
}

/** A human approved the root's spec document at `payload.version` (the contract's
 * `ArtifactReviewEventPayload`). Records that version and, only when it is the document's current
 * version (`designGateOpen`), wakes the architect with `design-approved` in the shape it has
 * always had. An approval below `latestVersion` is recorded and emits nothing: the gate stays
 * closed until the current version is approved. Idempotent independent of the outer seq fence:
 * an approval already recorded at this same version is a no-op, so a redelivered
 * `artifact.approved` at a newer seq can never re-emit the wake. */
function reduceArtifactApproved(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const raw = asRecord(event.payload);
  const gate = gateFor(state, event, stringValue(raw?.artifact_id));
  const version = raw?.version;
  if (!gate || typeof version !== "number" || gate.approvedVersion === version) return [];
  gate.approvedVersion = version;
  gate.latestVersion = Math.max(gate.latestVersion, version);
  if (!designGateOpen(gate)) return [];
  return routeArchitect(state, event.key, { type: "design-approved" });
}

/** A human requested changes on the root's spec document (`ArtifactReviewEventPayload` with
 * `reason` set — the decoder rejects one without it). Retracts any recorded approval — a
 * changes-requested review is the contract's only way to retract one — and wakes the architect
 * with the reviewer's reason (and login, when the payload's `actor` carries one). */
function reduceArtifactChangesRequested(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const raw = asRecord(event.payload);
  const gate = gateFor(state, event, stringValue(raw?.artifact_id));
  const version = raw?.version;
  const reason = stringValue(raw?.reason);
  if (!gate || typeof version !== "number" || reason === undefined) return [];
  delete gate.approvedVersion;
  gate.latestVersion = Math.max(gate.latestVersion, version);
  const author = stringValue(asRecord(raw?.actor)?.id);
  return routeArchitect(state, event.key, {
    type: "design-changes-requested",
    version,
    reason,
    ...(author === undefined ? {} : { author }),
  });
}

/** The root's spec document gained a version (`ArtifactVersionEventPayload`; named or unnamed —
 * the contract's staleness compares version numbers only). Raises `latestVersion` and emits
 * nothing: a spec edited after approval closes the gate silently (the architect made the edit, or
 * Dispatch already delivered this event to its session), and the next approval at the new version
 * reopens it. */
function reduceArtifactVersion(state: LegionState, event: DispatchIssueEvent): Effect[] {
  const raw = asRecord(event.payload);
  const gate = gateFor(state, event, stringValue(raw?.artifact_id));
  const number = asRecord(raw?.version)?.number;
  if (!gate || typeof number !== "number") return [];
  gate.latestVersion = Math.max(gate.latestVersion, number);
  return [];
}

/**
 * The Dispatch counterpart to `reduceGithubEvent`: derives Legion's issue lifecycle (triage
 * through done), child-tree wakes, and the design gate from native Dispatch issue events —
 * `issue.created`/`issue.updated`/`issue.closed`/`child.status` and, for the gate,
 * `artifact.approved`/`artifact.changes_requested`/`artifact.version`. Every other event type
 * (comments, asks, messages) is already delivered to the right role/session by Dispatch's own
 * routing (see the design's "Intake and events" section) and produces no effect here; in
 * particular `ask.answered` no longer touches the gate. `config` is accepted for signature parity
 * with `reduceGithubEvent`; no Dispatch event currently needs it.
 *
 * At-most-once per (issue, seq): before dispatching to a sub-reducer, an event whose `seq` is not
 * strictly newer than `state.issues[event.key].lastAppliedSeq` is dropped outright — no mutation,
 * no effect, not even a re-derived one — since Dispatch's own `Issue.last_seq` is monotonic per
 * issue and a redelivery or reorder can only repeat or regress it, never legitimately reuse it.
 * After a recognized sub-reducer runs (whether or not it produced an effect — a no-op redelivered
 * again must stay a no-op), `lastAppliedSeq` is stamped to `event.seq` on `state.issues[event.key]`
 * if that node exists. Unknown additive event types return before this stamp: they have no Legion
 * state meaning and are acknowledged without mutation. `child.status` and the artifact events
 * against a key this daemon has no node for skip the stamp because their reducers begin with an
 * explicit node existence guard, so a schema-valid but dangling gate or tree record cannot be
 * mutated or routed.
 */
export function reduceDispatchEvent(
  state: LegionState,
  event: DispatchIssueEvent,
  _config: ReducerConfig
): Effect[] {
  const lastAppliedSeq = state.issues[event.key]?.lastAppliedSeq;
  if (lastAppliedSeq !== undefined && event.seq <= lastAppliedSeq) return [];

  let effects: Effect[];
  switch (event.type) {
    case "issue.created":
      effects = reduceIssueCreated(state, event);
      break;
    case "issue.updated":
      effects = reduceIssueUpdated(state, event);
      break;
    case "issue.closed":
      effects = reduceIssueClosed(state, event);
      break;
    case "child.status":
      effects = reduceChildStatus(state, event);
      break;
    case "artifact.approved":
      effects = reduceArtifactApproved(state, event);
      break;
    case "artifact.changes_requested":
      effects = reduceArtifactChangesRequested(state, event);
      break;
    case "artifact.version":
      effects = reduceArtifactVersion(state, event);
      break;
    default:
      return [];
  }

  const node = state.issues[event.key];
  if (node) node.lastAppliedSeq = event.seq;

  return collapseClosedTreeWakes(effects);
}
