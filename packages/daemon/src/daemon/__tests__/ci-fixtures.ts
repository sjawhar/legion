// Shared fixtures for the event-pump and resync tests: a fake core-NATS
// connection, the daemon config, and the issue, tree, roles and PR literals the
// CI scenarios build on.
import { type IssueKey, roleToken } from "@legion/contracts";
import type { DaemonConfig } from "../config";
import type { DispatchClient } from "../dispatch-client";
import { type LegionState, newLegionState, type PrState } from "../legion-state";
import type { DurableMessageControl } from "../nats-transport";

/** A `DispatchClient` double for tests that need one wired (every `LegionApiDeps`,
 * `ProcessManagerDeps`, and `RunResyncDeps` requires one) but do not assert on Dispatch writes
 * themselves — `listIssues` returns empty, `setStatus` is a no-op spy, `getIssue` throws unless a
 * scenario overrides it. */
export function fakeDispatchClient(overrides: Partial<DispatchClient> = {}): DispatchClient {
  return {
    listIssues: async () => [],
    getIssue: async (key) => {
      throw new Error(`fakeDispatchClient.getIssue not stubbed for ${key}`);
    },
    setStatus: async () => {},
    ...overrides,
  };
}

/** Polls `predicate` every 10 ms until it holds or `timeoutMs` elapses. Real sockets and child
 * processes cannot be driven by fake timers, so a test awaits the observable condition itself
 * rather than a guessed duration. */
export async function waitFor(predicate: () => boolean, timeoutMs = 5_000): Promise<void> {
  for (let attempt = 0; attempt < timeoutMs / 10; attempt += 1) {
    if (predicate()) return;
    await Bun.sleep(10);
  }
  throw new Error("condition never became true");
}

interface Subscription {
  subject: string;
  callback: (subject: string, data: string, control: DurableMessageControl) => void;
}

interface DurableConsumer {
  stream: string;
  durable: string;
  filterSubjects: string[];
}

/** A durable delivery's `ack`/`nak`/`term` calls, recorded for assertions. */
export interface FakeDurableControlCalls {
  acks: number;
  naks: Array<number | undefined>;
  terms: Array<string | undefined>;
}

function fakeControl(
  overrides: Partial<DurableMessageControl> = {},
  calls?: FakeDurableControlCalls
): DurableMessageControl {
  return {
    streamSequence: 1,
    deliverySequence: 1,
    ack: () => {
      if (calls) calls.acks += 1;
    },
    nak: (delayMs) => {
      if (calls) calls.naks.push(delayMs);
    },
    term: (reason) => {
      if (calls) calls.terms.push(reason);
    },
    ...overrides,
  };
}

export class FakeNats {
  readonly subscriptions: Subscription[] = [];
  readonly durableConsumers: DurableConsumer[] = [];

  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void {
    const subscription = {
      subject,
      callback: (s: string, d: string) => callback(s, d),
    };
    this.subscriptions.push(subscription);
    return () => {
      const index = this.subscriptions.indexOf(subscription);
      if (index >= 0) this.subscriptions.splice(index, 1);
    };
  }

  consumeDurable(
    stream: string,
    durable: string,
    filterSubjects: string[],
    callback: (subject: string, data: string, control: DurableMessageControl) => void
  ): () => void {
    this.durableConsumers.push({ stream, durable, filterSubjects });
    const registered = filterSubjects.map((subject) => {
      const subscription = { subject, callback };
      this.subscriptions.push(subscription);
      return subscription;
    });
    return () => {
      for (const subscription of registered) {
        const index = this.subscriptions.indexOf(subscription);
        if (index >= 0) this.subscriptions.splice(index, 1);
      }
    };
  }

  publish(): void {}

  flushCalls = 0;
  flush(): Promise<void> {
    this.flushCalls += 1;
    return Promise.resolve();
  }

  /**
   * Dispatches `data` to every subscription matching `subject`. `control`
   * overrides the fake `ack`/`nak`/`term` control object (a plain function
   * is treated as `ack`, matching the common single-callback test shape);
   * pass `calls` to record which of `ack`/`nak`/`term` fired.
   */
  emit(
    subject: string,
    data: string,
    control: Partial<DurableMessageControl> | (() => void) = {},
    calls?: FakeDurableControlCalls
  ): void {
    const overrides = typeof control === "function" ? { ack: control } : control;
    const fullControl = fakeControl(overrides, calls);
    for (const subscription of this.subscriptions) {
      if (matches(subscription.subject, subject)) subscription.callback(subject, data, fullControl);
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
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repos: ["acme/widgets"],
    repo: "acme/widgets",
    admissionCap: 4,
    workerCap: 6,
    maxRecursionDepth: 8,
    lingerHours: 72,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 10,
    treeStopTimeoutSeconds: 60,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: 5,
    workerStreamPort: 13371,
    gates: { design: "root-issues" },
    githubApps: {},
    stateDir: "/state",
  };
}

export function stateForIssue(): {
  state: LegionState;
  issue: IssueKey;
  architect: string;
  implementer: string;
} {
  const state = newLegionState("omp", 2);
  const issue: IssueKey = "WIDGETS-1";
  const architect = roleToken("omp", issue, "architect");
  const implementer = roleToken("omp", issue, "implementer");
  state.issues[issue] = {
    key: issue,
    title: "Issue one",
    status: "in_progress",
    children: [],
  };
  state.trees[issue] = {
    root: issue,
    generation: 1,
    status: "active",
    launchFailures: 0,
  };
  state.roles[architect] = { issue, role: "architect" };
  state.roles[implementer] = { issue, role: "implementer" };
  return { state, issue, architect, implementer };
}

/** An unfenced, unsettled PR; override only the fields a scenario varies. */
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

/** A normalized `pull_request_review` payload as Envoy delivers it (see `githubPayload` in
 * `packages/envoy/internal/contracts/normalize.go`): flat strings, `commit_id` the sha the review
 * was submitted against, `head_sha` the PR's head at delivery time. */
export function reviewPayload(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "review",
    action: "submitted",
    repo: "acme/widgets",
    number: "7",
    title: "PR title",
    parent_kind: "pr",
    author: "sami",
    url: "https://github.com/acme/widgets/pull/7#pullrequestreview-1",
    state: "approved",
    body: "Looks good",
    commit_id: "head-1",
    head_sha: "head-1",
    ...overrides,
  };
}

/** A normalized `pull_request_review_comment` (`path` present) or `issue_comment` on a PR
 * (`path` absent) payload as Envoy delivers it. Pass `parent_kind: "issue"` for a plain GitHub
 * issue comment, which the daemon never acts on. */
export function commentPayload(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "comment",
    action: "created",
    repo: "acme/widgets",
    number: "7",
    title: "PR title",
    parent_kind: "pr",
    author: "reviewer",
    url: "https://github.com/acme/widgets/pull/7#issuecomment-1",
    body: "Please rename this",
    ...overrides,
  };
}
