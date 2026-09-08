// Shared fixtures for the event-pump and resync tests: a fake core-NATS
// connection, the daemon config, and the acme/widgets issue, tree, roles and PR
// every CI scenario starts from.
import { formatIssueKey, type IssueKey, roleToken } from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { type LegionState, newLegionState, type PrState } from "../legion-state";

interface Subscription {
  subject: string;
  callback: (subject: string, data: string) => void;
}

export class FakeNats {
  readonly subscriptions: Subscription[] = [];

  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void {
    const subscription = { subject, callback };
    this.subscriptions.push(subscription);
    return () => {
      const index = this.subscriptions.indexOf(subscription);
      if (index >= 0) this.subscriptions.splice(index, 1);
    };
  }

  publish(): void {}

  emit(subject: string, data: string): void {
    for (const subscription of this.subscriptions) {
      if (matches(subscription.subject, subject)) subscription.callback(subject, data);
    }
  }
}

function matches(pattern: string, subject: string): boolean {
  const patternTokens = pattern.split(".");
  const subjectTokens = subject.split(".");
  for (let index = 0; index < patternTokens.length; index += 1) {
    const token = patternTokens[index];
    if (token === ">") return index < subjectTokens.length;
    if (token !== "*" && token !== subjectTokens[index]) return false;
  }
  return patternTokens.length === subjectTokens.length;
}

export function config(): DaemonConfig {
  return {
    project: "omp",
    legionId: "acme/1",
    port: 13370,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    boardProjectIds: ["PVT_board"],
    appLogins: ["legion[bot]"],
    admissionCap: 4,
    workerBudget: 6,
    maxRecursionDepth: 8,
    lingerHours: 72,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir: "/state",
  };
}

export function stateForIssue(released = true): {
  state: LegionState;
  issue: IssueKey;
  architect: string;
  implementer: string;
} {
  const state = newLegionState("omp", 2);
  const issue = formatIssueKey("acme", "widgets", 1);
  const architect = roleToken("omp", issue, "architect");
  const implementer = roleToken("omp", issue, "implementer");
  state.issues[issue] = {
    key: issue,
    title: "Issue one",
    state: "open",
    children: [],
    released,
    labels: [],
  };
  state.trees[issue] = {
    root: issue,
    generation: 1,
    status: "active",
    launchFailures: 0,
    heldEvents: [],
  };
  state.roles[architect] = { issue, role: "architect" };
  state.roles[implementer] = { issue, role: "implementer" };
  return { state, issue, architect, implementer };
}

/** An unfenced, unsettled PR at `headSha`; override the fields a scenario varies. */
export function checkPr(issue: IssueKey, overrides: Partial<PrState> = {}): PrState {
  return {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha: "head-1",
    verdict: null,
    failing: [],
    failingStatuses: [],
    ciSettledAt: null,
    ciCheckRuns: null,
    ciSettlementGeneration: null,
    ciSnapshot: null,
    ciReconciled: false,
    fixAttempts: 0,
    ...overrides,
  };
}

/** The released issue with its PR registered: the starting state of every CI scenario. */
export function stateForCi(): { state: LegionState; architect: string; implementer: string } {
  const { state, issue, architect, implementer } = stateForIssue();
  state.prs["acme/widgets#7"] = checkPr(issue);
  return { state, architect, implementer };
}

/** A settled-checks payload as the listener publishes it: green on one run unless overridden. */
export function settledChecks(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "checks",
    repo: "acme/widgets",
    number: "7",
    sha: "head-1",
    is_head: true,
    check_runs: [{ name: "build", id: 1 }],
    generation: 0,
    snapshot: "state-hash-1",
    failed: { count: 0, checks: [] },
    running: { count: 0, checks: [] },
    passed: { count: 1, checks: ["unit"] },
    queued: { count: 0, checks: [] },
    skipped: { count: 0, checks: [] },
    cancelled: { count: 0, checks: [] },
    failing_checks: [],
    ...overrides,
  };
}

export function prPayload(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "pr",
    action: "synchronize",
    repo: "acme/widgets",
    number: "7",
    title: "PR title",
    author: "author",
    url: "https://github.com/acme/widgets/pull/7",
    head_sha: "head-1",
    head_ref: "legion/issue-1",
    base_ref: "main",
    merged: "false",
    merge_commit_sha: "",
    merged_by: "",
    body: "",
    updated_at: "2026-09-07T03:00:00Z",
    ...overrides,
  };
}
