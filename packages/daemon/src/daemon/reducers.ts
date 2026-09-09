import {
  formatIssueKey,
  type IssueKey,
  isLegionRole,
  type LegionRole,
  roleToken,
} from "@legion/contracts";
import { type CheckRunRef, sortedCheckRunRefs } from "../state/types";
import type { IssueNode, LegionState, PrState, TreeState, UpdateSource } from "./legion-state";

export interface LegionEventPayload {
  type: string;
  [key: string]: unknown;
}

/** An effect a reducer derives from one event. For a durable GitHub event, every effect dispatches (and a 404 no-holder is recorded) before the reducer's mutation is saved and the message acks; a failure anywhere in that sequence is fatal (see `events.ts`). */
export type Effect =
  | { kind: "publish"; role: string; payload: LegionEventPayload }
  | { kind: "controller"; payload: LegionEventPayload }
  | { kind: "probe"; tree: IssueKey }
  | { kind: "linger"; tree: IssueKey }
  | { kind: "approval-status"; repo: string; pr: number; sha: string };

export interface EnvelopeJson {
  event_id: string;
  issued_at: number;
  payload?: string | Record<string, unknown>;
  payload_summary?: string;
  [key: string]: unknown;
}

export interface ReducerConfig {
  boardProjectIds: readonly string[];
  appLogins: readonly string[];
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
  config: ReducerConfig,
  envelope: EnvelopeJson
): Effect[] {
  pr.ciSettledAt = input.settledAt;
  return collapseClosedTreeWakes(
    ciVerdictEmissions(pr, input.verdict, input.failing, input.failingStatuses).flatMap(
      (emission) => [
        ...routeActive(state, pr.key, emission, envelope),
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

const SURVIVING_LABELS: Record<string, true> = {
  "needs-approval": true,
  "human-approved": true,
  "legion-child": true,
  "legion-backlog": true,
};

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

function repository(payload: JsonRecord): string | undefined {
  return stringValue(asRecord(payload.repository)?.full_name) ?? stringValue(payload.repo);
}

function keyFor(repo: string, number: number): IssueKey | undefined {
  const [owner, name, ...extra] = repo.split("/");
  if (!owner || !name || extra.length > 0) return undefined;
  return formatIssueKey(owner, name, number);
}

function labels(value: unknown): string[] {
  const source = Array.isArray(value) ? value : asRecord(value)?.nodes;
  if (!Array.isArray(source)) return [];
  const result: string[] = [];
  for (const label of source) {
    const name = typeof label === "string" ? label : stringValue(asRecord(label)?.name);
    if (name && SURVIVING_LABELS[name] && !result.includes(name)) result.push(name);
  }
  return result;
}

// Dispatch threads are human Q&A artifacts (GitHub sub-issues labeled by
// dispatchLabel in packages/envoy/internal/dispatch/core/thread.go), never
// Legion work items. Checked against the raw payload — deliberately not via
// labels()/SURVIVING_LABELS, whose output persists into IssueNode.labels and
// must stay within the GateLabelSchema enum in legion-state.ts.
function isDispatchThread(rawLabels: unknown): boolean {
  const source = Array.isArray(rawLabels) ? rawLabels : asRecord(rawLabels)?.nodes;
  if (!Array.isArray(source)) return false;
  return source.some((label) => {
    const name = typeof label === "string" ? label : stringValue(asRecord(label)?.name);
    return name === "dispatch-thread";
  });
}

function childKeys(repo: string, raw: JsonRecord): IssueKey[] {
  const subIssues = raw.sub_issues;
  const values = Array.isArray(subIssues) ? subIssues : asRecord(subIssues)?.nodes;
  if (!Array.isArray(values)) return [];
  const result: IssueKey[] = [];
  for (const value of values) {
    const record = asRecord(value);
    if (isDispatchThread(record?.labels)) continue;
    const number = numberValue(record?.number);
    const key = number === undefined ? undefined : keyFor(repo, number);
    if (key && !result.includes(key)) result.push(key);
  }
  return result;
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

/**
 * Routes an event to an explicit role token. The daemon does not hold events
 * at the reducer level: release and liveness gating happen where the event
 * is actually delivered (the event pump's per-publish active check, and the
 * delivery-exception path when no subscriber is holding the role).
 */
function routeToken(
  state: LegionState,
  issue: IssueKey,
  token: string,
  payload: LegionEventPayload,
  _envelope: EnvelopeJson
): Effect[] {
  if (!treeFor(state, issue)) return [];
  return [{ kind: "publish", role: token, payload }];
}

/**
 * Routes an event about `issue` to whichever role the shared routing table
 * names: the issue's active phase worker (`state.phases[issue]`, written by
 * `handlePhase`), or the tree's architect when no phase is active. A closed
 * tree instead wakes the controller so a human can decide whether to resume
 * it.
 */
export function routeActive(
  state: LegionState,
  issue: IssueKey,
  payload: LegionEventPayload,
  _envelope: EnvelopeJson
): Effect[] {
  const tree = treeFor(state, issue);
  if (!tree) return [];
  if (tree.status === "closed") {
    return [
      {
        kind: "controller",
        payload: { type: "closed-tree-activity", issue, root: tree.root, event: payload },
      },
    ];
  }
  const phase = state.phases[issue];
  if (phase && !isLegionRole(phase.phase)) {
    throw new Error(
      `state.phases[${issue}] has an unrecognized phase: ${JSON.stringify(phase.phase)}`
    );
  }
  const token = phase
    ? roleToken(state.project, issue, phase.phase as LegionRole)
    : roleToken(state.project, tree.root, "architect");
  return [{ kind: "publish", role: token, payload }];
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
  return parent.children.filter((key) => state.issues[key]?.state === "open").length;
}

function boardEvent(payload: JsonRecord, config: ReducerConfig): boolean {
  const project = asRecord(payload.project);
  const item = asRecord(payload.projects_v2_item);
  const id =
    stringValue(project?.id) ??
    stringValue(payload.project_node_id) ??
    stringValue(item?.project_node_id) ??
    stringValue(asRecord(item?.project)?.id);
  return id !== undefined && config.boardProjectIds.includes(id);
}

function addNode(
  state: LegionState,
  key: IssueKey,
  raw: JsonRecord,
  released: boolean,
  parent?: IssueKey
): IssueNode {
  const prior = state.issues[key];
  const node: IssueNode = {
    key,
    title: stringValue(raw.title) ?? prior?.title ?? key,
    state: stringValue(raw.state) === "closed" ? "closed" : "open",
    children: prior?.children ?? [],
    released: prior?.released ?? released,
    labels: labels(raw.labels),
    ...(prior?.finalCommentRef ? { finalCommentRef: prior.finalCommentRef } : {}),
    ...(prior?.updatedAt !== undefined ? { updatedAt: prior.updatedAt } : {}),
    ...(prior?.updatedAtSource !== undefined ? { updatedAtSource: prior.updatedAtSource } : {}),
  };
  const ancestor = parent ?? prior?.parent;
  if (ancestor) node.parent = ancestor;
  state.issues[key] = node;
  return node;
}

function filtered(comment: JsonRecord, config: ReducerConfig): boolean {
  const author = stringValue(asRecord(comment.user)?.login) ?? stringValue(comment.author);
  return (
    (stringValue(comment.body) ?? "").includes("<!-- legion:") ||
    (author !== undefined && config.appLogins.includes(author))
  );
}

function issueForBranch(repo: string, branch: string): IssueKey | undefined {
  const match = /^legion\/issue-(\d+)$/.exec(branch);
  return match ? keyFor(repo, Number(match[1])) : undefined;
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
  sha: string | undefined,
  headUpdatedAt: number | undefined,
  source: UpdateSource
): PrState | undefined {
  const key = (branch ? issueForBranch(repo, branch) : undefined) ?? keyFor(repo, number);
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
  const pr = registerPr(state, repo, number, branch, sha, headUpdatedAt, source);
  if (pr) delete state.prTombstones[prKey];
  return pr;
}

export function resetPrHead(pr: PrState, headSha: string): void {
  if (pr.verdict === "red") pr.fixAttempts += 1;
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

function removeBranchMappings(state: LegionState, prKey: string): void {
  for (const [branch, mapped] of Object.entries(state.prByBranch)) {
    if (mapped === prKey) delete state.prByBranch[branch];
  }
}

function ingress(
  state: LegionState,
  payload: JsonRecord,
  config: ReducerConfig,
  source: UpdateSource
): Effect[] | undefined {
  const raw = asRecord(payload.issue) ?? asRecord(asRecord(payload.projects_v2_item)?.content);
  const action = stringValue(payload.action);
  if (!raw || !boardEvent(payload, config) || (action !== "opened" && action !== "created"))
    return undefined;
  const repo = repository(payload);
  const number = numberValue(raw.number);
  if (!repo || number === undefined) return [];
  const key = keyFor(repo, number);
  if (!key) return [];
  if (isDispatchThread(raw.labels)) return [];
  const currentLabels = labels(raw.labels);
  if (currentLabels.includes("legion-child") || currentLabels.includes("legion-backlog")) return [];

  const existing = state.issues[key];
  const rawUpdatedAt = updatedAt(raw);
  if (
    supersededBy(
      { updatedAt: rawUpdatedAt, source },
      { updatedAt: existing?.updatedAt, source: existing?.updatedAtSource ?? "webhook" }
    )
  ) {
    console.debug(
      `[legion] ignored stale ingress event for ${key}: issue.updated_at is older than the last applied event`
    );
    return [];
  }
  // A tree already exists: this issue was already triaged once, so a
  // redelivered or duplicate "opened"/"created" webhook must not re-triage
  // it or clobber children/labels/state a newer event has already applied.
  if (state.trees[key]) return [];

  const preexistingChildren = childKeys(repo, raw);
  const node = addNode(state, key, raw, true);
  node.children = preexistingChildren;
  if (rawUpdatedAt !== undefined) {
    node.updatedAt = rawUpdatedAt;
    node.updatedAtSource = source;
  }
  const rawSubIssues = raw.sub_issues;
  const values = Array.isArray(rawSubIssues) ? rawSubIssues : asRecord(rawSubIssues)?.nodes;
  if (Array.isArray(values)) {
    for (const value of values) {
      const child = asRecord(value);
      if (isDispatchThread(child?.labels)) continue;
      const childNumber = numberValue(child?.number);
      const childKey = childNumber === undefined ? undefined : keyFor(repo, childNumber);
      if (child && childKey) addNode(state, childKey, child, false, key);
    }
  }
  return [
    {
      kind: "controller",
      payload: { type: "triage", issue: key, preexistingChildren },
    },
  ];
}

function subIssue(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson,
  source: UpdateSource
): Effect[] | undefined {
  const rawParent = asRecord(payload.parent_issue);
  const rawChild = asRecord(payload.sub_issue);
  if (!rawParent || !rawChild) return undefined;
  const repo = repository(payload);
  const parentNumber = numberValue(rawParent.number);
  const childNumber = numberValue(rawChild.number);
  if (!repo || parentNumber === undefined || childNumber === undefined) return [];
  const parentKey = keyFor(repo, parentNumber);
  const childKey = keyFor(repo, childNumber);
  const parent = parentKey ? state.issues[parentKey] : undefined;
  if (!parentKey || !childKey || !parent) return [];

  const parentUpdatedAt = updatedAt(rawParent);
  if (parentUpdatedAt === undefined) {
    throw new Error(
      `sub_issue event for ${parentKey} is missing a parseable parent_issue.updated_at (GitHub always sends one; payload is malformed)`
    );
  }
  if (
    supersededBy(
      { updatedAt: parentUpdatedAt, source },
      { updatedAt: parent.updatedAt, source: parent.updatedAtSource ?? "webhook" }
    )
  ) {
    console.debug(
      `[legion] ignored stale sub_issue event for ${parentKey}: parent_issue.updated_at is older than the last applied event`
    );
    return [];
  }

  if (payload.action === "sub_issue_added") {
    // Never adopt a dispatch thread as a child (see isDispatchThread).
    if (isDispatchThread(rawChild.labels)) return [];
    const known = parent.children.includes(childKey);
    if (!known) parent.children.push(childKey);
    const child = state.issues[childKey] ?? addNode(state, childKey, rawChild, false, parentKey);
    child.parent = parentKey;
    if (parentUpdatedAt !== undefined) {
      parent.updatedAt = parentUpdatedAt;
      parent.updatedAtSource = source;
    }
    if (known || treeFor(state, parentKey)?.status !== "active") return [];
    return routeActive(
      state,
      parentKey,
      {
        type: "child-adopted",
        child: childKey,
        remaining: openChildren(state, parent),
      },
      envelope
    );
  }

  if (payload.action !== "sub_issue_removed" || !parent.children.includes(childKey)) return [];
  const child = state.issues[childKey];
  const wasOpen = child?.state === "open";
  parent.children = parent.children.filter((key) => key !== childKey);
  if (child?.parent === parentKey) delete child.parent;
  if (parentUpdatedAt !== undefined) {
    parent.updatedAt = parentUpdatedAt;
    parent.updatedAtSource = source;
  }
  const result = routeActive(
    state,
    parentKey,
    {
      type: "child-removed",
      child: childKey,
      remaining: openChildren(state, parent),
    },
    envelope
  );
  if (wasOpen && openChildren(state, parent) === 0) {
    result.push(...routeActive(state, parentKey, { type: "children-complete" }, envelope));
  }
  return result;
}

function issueEvent(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson,
  source: UpdateSource
): Effect[] | undefined {
  const raw = asRecord(payload.issue);
  if (!raw || payload.comment !== undefined || raw.pull_request !== undefined) return undefined;
  const repo = repository(payload);
  const number = numberValue(raw.number);
  const key = repo && number !== undefined ? keyFor(repo, number) : undefined;
  const node = key ? state.issues[key] : undefined;
  if (!key || !node) return [];

  const issueUpdatedAt = updatedAt(raw);
  if (issueUpdatedAt === undefined) {
    throw new Error(
      `issue event for ${key} is missing a parseable updated_at (GitHub always sends one; payload is malformed)`
    );
  }
  if (
    supersededBy(
      { updatedAt: issueUpdatedAt, source },
      { updatedAt: node.updatedAt, source: node.updatedAtSource ?? "webhook" }
    )
  ) {
    console.debug(
      `[legion] ignored stale issue event for ${key}: issue.updated_at is older than the last applied event`
    );
    return [];
  }

  if (payload.action === "labeled" || payload.action === "unlabeled") {
    const label = stringValue(asRecord(payload.label)?.name);
    if (!label || !SURVIVING_LABELS[label]) return [];
    if (payload.action === "labeled") {
      if (node.labels.includes(label)) return [];
      node.labels.push(label);
      node.updatedAt = issueUpdatedAt;
      node.updatedAtSource = source;
      return label === "human-approved"
        ? routeActive(state, key, { type: "human-approved" }, envelope)
        : [];
    }
    if (!node.labels.includes(label)) return [];
    node.labels = node.labels.filter((value) => value !== label);
    node.updatedAt = issueUpdatedAt;
    node.updatedAtSource = source;
    return [];
  }

  if (payload.action === "closed") {
    const parent = node.parent ? state.issues[node.parent] : undefined;
    const wasOpen = node.state === "open";
    node.state = "closed";
    node.updatedAt = issueUpdatedAt;
    node.updatedAtSource = source;
    if (!parent || !node.parent) {
      return state.trees[key]?.status === "active" ? [{ kind: "linger", tree: key }] : [];
    }
    if (!wasOpen) return [];
    const result = routeActive(
      state,
      node.parent,
      {
        type: "child-closed",
        child: key,
        completion: stringValue(raw.state_reason) ?? "closed",
        remaining: openChildren(state, parent),
        finalCommentRef:
          stringValue(raw.final_comment_ref) ??
          stringValue(payload.final_comment_ref) ??
          node.finalCommentRef ??
          null,
      },
      envelope
    );
    if (openChildren(state, parent) === 0) {
      result.push(...routeActive(state, node.parent, { type: "children-complete" }, envelope));
    }
    return result;
  }

  if (payload.action !== "reopened") return [];
  node.state = "open";
  node.updatedAt = issueUpdatedAt;
  node.updatedAtSource = source;
  delete node.finalCommentRef;
  if (node.parent)
    return routeActive(state, node.parent, { type: "child-reopened", child: key }, envelope);
  const tree = state.trees[key];
  if (!tree) {
    return [
      {
        kind: "controller",
        payload: {
          type: "triage",
          issue: key,
          preexistingChildren: node.children,
        },
      },
    ];
  }
  if (tree.status === "lingering") {
    return routeToken(
      state,
      key,
      roleToken(state.project, key, "architect"),
      { type: "reopened" },
      envelope
    );
  }
  return [
    { kind: "controller", payload: { type: "reactivation", issue: key } },
    { kind: "probe", tree: key },
  ];
}

function issueComment(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson,
  config: ReducerConfig
): Effect[] | undefined {
  const rawIssue = asRecord(payload.issue);
  const comment = asRecord(payload.comment);
  if (!rawIssue || !comment) return undefined;
  const repo = repository(payload);
  const number = numberValue(rawIssue.number);
  if (!repo || number === undefined) return [];
  if (payload.action !== "created" || filtered(comment, config)) return [];
  const author = stringValue(asRecord(comment.user)?.login) ?? "";
  const body = stringValue(comment.body) ?? "";
  const url = stringValue(comment.html_url) ?? "";
  if (rawIssue.pull_request !== undefined) {
    const pr = state.prs[`${repo}#${number}`];
    return pr
      ? routeActive(state, pr.key, { type: "pr-comment", author, body, url }, envelope)
      : [];
  }
  const key = keyFor(repo, number);
  return key ? routeActive(state, key, { type: "issue-comment", author, body, url }, envelope) : [];
}

function reviewComment(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson,
  config: ReducerConfig
): Effect[] | undefined {
  const pullRequest = asRecord(payload.pull_request);
  const comment = asRecord(payload.comment);
  if (!pullRequest || !comment || asRecord(payload.issue)) return undefined;
  const repo = repository(payload);
  const number = numberValue(pullRequest.number);
  if (!repo || number === undefined || payload.action !== "created" || filtered(comment, config))
    return [];
  const pr = state.prs[`${repo}#${number}`];
  if (!pr) return [];
  return routeActive(
    state,
    pr.key,
    {
      type: "pr-review-comment",
      author: stringValue(asRecord(comment.user)?.login) ?? "",
      body: stringValue(comment.body) ?? "",
      path: stringValue(comment.path) ?? "",
      url: stringValue(comment.html_url) ?? "",
    },
    envelope
  );
}

function review(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson
): Effect[] | undefined {
  const pullRequest = asRecord(payload.pull_request);
  const rawReview = asRecord(payload.review);
  if (!pullRequest || !rawReview) return undefined;
  const repo = repository(payload);
  const number = numberValue(pullRequest.number);
  if (!repo || number === undefined || payload.action !== "submitted") return [];
  const pr = state.prs[`${repo}#${number}`];
  if (!pr) return [];
  const decision = (stringValue(rawReview.state) ?? "").toLowerCase();
  const isCurrentHead = stringValue(rawReview.commit_id) === pr.headSha;
  const prior = pr.reviewDecision;
  if (isCurrentHead && (decision === "approved" || decision === "changes_requested")) {
    pr.reviewDecision = decision;
  }
  const result = routeActive(
    state,
    pr.key,
    {
      type: "pr-review",
      state: decision,
      author: stringValue(asRecord(rawReview.user)?.login) ?? "",
      body: stringValue(rawReview.body) ?? "",
    },
    envelope
  );
  result.push({ kind: "approval-status", repo, pr: number, sha: pr.headSha });
  if (isCurrentHead && decision === "approved" && prior !== "approved" && pr.verdict === "green") {
    result.push(...routeActive(state, pr.key, { type: "pr-ready", pr: number }, envelope));
  }
  return result;
}

function pullRequest(
  state: LegionState,
  payload: JsonRecord,
  envelope: EnvelopeJson,
  source: UpdateSource
): Effect[] | undefined {
  if (payload.kind !== "pr") return undefined;
  const repo = stringValue(payload.repo);
  const number = numberValue(payload.number);
  if (!repo || number === undefined) return [];
  const prKey = `${repo}#${number}`;
  const branch = stringValue(payload.head_ref);
  const sha = stringValue(payload.head_sha);
  const headUpdatedAt = updatedAt(payload);

  if (payload.action === "opened") {
    const pr = registerPrFenced(state, repo, number, branch, sha, headUpdatedAt, source);
    if (!pr) return [];
    return routeActive(
      state,
      pr.key,
      { type: "pr-opened", pr: number, url: stringValue(payload.url) ?? "" },
      envelope
    );
  }

  let pr: PrState | undefined = state.prs[prKey];
  if (!pr && payload.action === "synchronize") {
    pr = registerPrFenced(state, repo, number, branch, sha, headUpdatedAt, source);
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
      return [{ kind: "approval-status", repo, pr: number, sha }];
    }
    resetPrHead(pr, sha);
    if (headUpdatedAt === undefined) {
      delete pr.headUpdatedAt;
      delete pr.headUpdatedAtSource;
    } else {
      pr.headUpdatedAt = headUpdatedAt;
      pr.headUpdatedAtSource = source;
    }
    return [{ kind: "approval-status", repo, pr: number, sha }];
  }
  if (payload.action === "closed" && payload.merged === "false") {
    delete state.prs[prKey];
    removeBranchMappings(state, prKey);
    if (headUpdatedAt !== undefined) state.prTombstones[prKey] = headUpdatedAt;
    return routeActive(state, pr.key, { type: "pr-closed-unmerged", pr: number }, envelope);
  }
  return [];
}

export function reduceGithubEvent(
  state: LegionState,
  topic: string,
  envelope: EnvelopeJson,
  config: ReducerConfig
): Effect[] {
  if (/^notifications\.github\.[^.]+\.[^.]+\.pr\.\d+\.checks$/.test(topic)) return [];
  const payload = payloadFrom(envelope);
  if (!payload) return [];
  const repo = repository(payload);
  // Pushes to legion issue branches carry no reducer-visible state transitions.
  if (repo && stringValue(payload.ref)?.startsWith("refs/heads/legion/issue-")) return [];
  // "resync" is the daemon's own sentinel topic for reducer input (board GraphQL
  // reads, not an external webhook) — GitHub's authoritative read wins a
  // same-clock tie against a webhook (see `supersededBy`).
  const source: UpdateSource = topic === "resync" ? "resync" : "webhook";
  // Only pullRequest understands Envoy's normalized GitHub envelopes. The issue, issue-comment,
  // review, and projects_v2_item reducers still require raw GitHub nesting and ignore Envoy payloads.
  return collapseClosedTreeWakes(
    ingress(state, payload, config, source) ??
      subIssue(state, payload, envelope, source) ??
      issueComment(state, payload, envelope, config) ??
      reviewComment(state, payload, envelope, config) ??
      review(state, payload, envelope) ??
      pullRequest(state, payload, envelope, source) ??
      issueEvent(state, payload, envelope, source) ??
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
  const envelope = {
    event_id: `ci:${repo}#${number}:${emission.sha}`,
    issued_at: pr.ciSettledAt,
  };
  if (emission.type === "ci-green") {
    return pr.reviewDecision === "approved"
      ? routeActive(state, pr.key, { type: "pr-ready", pr: number }, envelope)
      : [];
  }
  return pr.fixAttempts >= config.maxFixAttempts
    ? routeActive(
        state,
        pr.key,
        { type: "pr-blocked", pr: number, attempts: pr.fixAttempts },
        envelope
      )
    : [];
}
