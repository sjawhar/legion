import { describe, expect, it } from "bun:test";
import { formatIssueKey, roleToken } from "@legion/contracts";
import type { CiFetchResult } from "../../state/fetch";
import { type LegionState, newLegionState, type PrState } from "../legion-state";
import { type Effect, type EnvelopeJson, reduceGithubEvent } from "../reducers";
import { type RunResyncDeps, runResync } from "../resync";

const issue = formatIssueKey("sjawhar", "legion", 42);

function boardIssue(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    content: {
      type: "Issue",
      number: 42,
      title: "Resync this Legion tree",
      repository: "sjawhar/legion",
    },
    status: "Todo",
    labels: [],
    ...overrides,
  };
}

function resyncDeps(state: LegionState, items: Record<string, unknown>[]): RunResyncDeps {
  return {
    state,
    config: {
      resyncIntervalMs: 600_000,
      boardProjectIds: ["PVT_board"],
      appLogins: [],
      maxFixAttempts: 3,
    },
    fetchGitHubProjectItems: async () => ({ items }),
    fetchCiStatusBatch: async () => ({}),
    applyEffects: async () => {},
    now: () => Date.parse("2026-08-24T00:00:00.000Z"),
  };
}

function recordOpenReleasedIssue(
  state: LegionState,
  overrides: Partial<LegionState["issues"][typeof issue]> = {}
): void {
  state.issues[issue] = {
    key: issue,
    title: "Resync this Legion tree",
    state: "open",
    children: [],
    released: true,
    labels: [],
    ...overrides,
  };
}

describe("runResync", () => {
  it("reports one zero-owner-tree anomaly for an unmarked released open issue with no active tree", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state);

    const event = await runResync(resyncDeps(state, [boardIssue()]));

    expect(event).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "zero-owner-tree",
          issue,
          detail: "released open issue has no active Legion tree",
        },
      ],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });
  it("reports and re-emits triage for an unadmitted tracked open issue without a tree", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state, { released: false });
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];

    const first = await runResync({
      ...resyncDeps(state, [boardIssue()]),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    });

    expect(first).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "untriaged-open",
          issue,
          detail: "tracked open issue has no Legion tree or admission entry",
        },
      ],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "controller",
            payload: { type: "triage", issue, preexistingChildren: [] },
          },
        ],
        envelope: {
          event_id: `resync:${issue}:triage`,
          issued_at: Date.parse("2026-08-24T00:00:00.000Z"),
        },
      },
    ]);

    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
    };
    dispatched.length = 0;

    const second = await runResync({
      ...resyncDeps(state, [boardIssue()]),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
      now: () => Date.parse("2026-08-24T00:10:00.000Z"),
    });

    expect(second).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
    expect(dispatched).toEqual([]);
  });

  it("suppresses every anomaly for a deliberately backlogged issue", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state, { backlogMarker: "waiting for maintainer" });

    const event = await runResync(resyncDeps(state, [boardIssue()]));

    expect(event).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });

  it("heals an open board issue missing from state and enqueues triage through the live event path", async () => {
    const state = newLegionState("omp", 1);
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];

    const event = await runResync({
      ...resyncDeps(state, [boardIssue()]),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    });

    expect(state.issues[issue]).toMatchObject({
      key: issue,
      title: "Resync this Legion tree",
      state: "open",
      released: true,
    });
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "controller",
            payload: { type: "triage", issue, preexistingChildren: [] },
          },
        ],
        envelope: expect.objectContaining({
          payload: {
            action: "opened",
            project: { id: "PVT_board" },
            projects_v2_item: { content: boardIssue().content },
            repository: { full_name: "sjawhar/legion" },
          },
        }),
      },
    ]);
    expect(event).toEqual({
      type: "resync",
      anomalies: [],
      healed: 1,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });

  it("reconciles a human approval label through the reducer and executes its architect wake", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state, { labels: ["needs-approval"] });
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      heldEvents: [],
      launchFailures: 0,
    };
    const architect = roleToken(state.project, issue, "architect");
    state.roles[architect] = { issue, role: "architect" };
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];

    const event = await runResync({
      ...resyncDeps(state, [boardIssue({ labels: ["human-approved"] })]),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    });

    expect(state.issues[issue]?.labels).toEqual(["human-approved"]);
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "publish",
            role: architect,
            payload: { type: "human-approved" },
          },
        ],
        envelope: {
          event_id: `resync:${issue}:labeled:human-approved`,
          issued_at: Date.parse("2026-08-24T00:00:00.000Z"),
          payload: {
            action: "labeled",
            issue: { number: 42 },
            label: { name: "human-approved" },
            repository: { full_name: "sjawhar/legion" },
          },
        },
      },
      {
        effects: [],
        envelope: {
          event_id: `resync:${issue}:unlabeled:needs-approval`,
          issued_at: Date.parse("2026-08-24T00:00:00.000Z"),
          payload: {
            action: "unlabeled",
            issue: { number: 42 },
            label: { name: "needs-approval" },
            repository: { full_name: "sjawhar/legion" },
          },
        },
      },
    ]);
    expect(event).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 2,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });

  it("leaves a missed-open anomaly when no configured board project can drive the reducer", async () => {
    const state = newLegionState("omp", 1);
    const effects: Effect[][] = [];
    const deps = resyncDeps(state, [boardIssue()]);
    deps.config = { ...deps.config, boardProjectIds: [] };
    deps.applyEffects = async (received) => {
      effects.push(received);
    };

    expect(await runResync(deps)).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "missed-open",
          issue,
          detail: "open board issue is absent from Legion state",
        },
      ],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
    expect(state.issues[issue]).toBeUndefined();
    expect(effects).toEqual([]);
  });

  it("keeps an error-status anomaly after healing its missing board issue", async () => {
    const state = newLegionState("omp", 1);

    const event = await runResync(resyncDeps(state, [boardIssue({ status: "Error" })]));

    expect(event).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "erroring-issue",
          issue,
          detail: "open board issue has an error project status",
        },
      ],
      healed: 1,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });

  it("surfaces a launch-failed tree even when the board refresh has no matching item", async () => {
    const state = newLegionState("omp", 1);
    state.trees[issue] = {
      root: issue,
      generation: 7,
      status: "launch-failed",
      launchFailures: 3,
      heldEvents: [],
    };

    const event = await runResync(resyncDeps(state, []));

    expect(event).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "launch-failed",
          issue,
          detail: "tree launch failed 3 times",
        },
      ],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });

  it("suppresses a deliberately backlogged launch-failed tree", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state, { backlogMarker: "waiting for maintainer" });
    state.trees[issue] = {
      root: issue,
      generation: 7,
      status: "launch-failed",
      launchFailures: 3,
      heldEvents: [],
    };

    expect(await runResync(resyncDeps(state, []))).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });
  it("completes the resync while reporting board items excluded for null content", async () => {
    const state = newLegionState("omp", 1);
    const event = await runResync({
      ...resyncDeps(state, []),
      fetchGitHubProjectItems: async () => ({
        items: [],
        excludedNullContentItems: 1,
      }),
    });

    expect(event).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      reconciledLabels: 0,
      excludedNullContentItems: 1,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
  });
  it("settles a reconciled green PR and notifies its approved architect", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
    };
    state.roles[roleToken("omp", issue, "architect")] = { issue, role: "architect" };
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
      reviewDecision: "approved",
    };
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
    let now = Date.parse("2026-08-24T00:00:00.000Z");
    const fetches: Record<string, unknown>[] = [];
    const deps: RunResyncDeps = {
      ...resyncDeps(state, []),
      now: () => now,
      fetchCiStatusBatch: async (refs: Record<string, unknown>) => {
        fetches.push(refs);
        return {
          "sjawhar/legion#7": {
            ciStatus: "passing" as const,
            mergeableStatus: null,
            headSha: "head-1",
            updatedAt: "2026-08-24T00:00:00.000Z",
            checkRuns: [],
            isOpen: true,
          },
        };
      },
      applyEffects: async (effects: Effect[], envelope: EnvelopeJson) => {
        dispatched.push({ effects, envelope });
      },
    };

    await runResync(deps);

    expect(fetches).toEqual([
      {
        "sjawhar/legion#7": { owner: "sjawhar", repo: "legion", number: 7 },
      },
    ]);
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "green",
      failing: [],
      ciSettledAt: now,
      ciCheckRuns: null,
    });
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "publish",
            role: roleToken("omp", issue, "implementer"),
            payload: { type: "ci-green", sha: "head-1" },
          },
          {
            kind: "publish",
            role: roleToken("omp", issue, "architect"),
            payload: { type: "pr-ready", pr: 7 },
          },
        ],
        envelope: {
          event_id: "resync:sjawhar/legion#7:ci",
          issued_at: now,
        },
      },
    ]);

    dispatched.length = 0;
    now += 600_000;
    await runResync(deps);
    expect(dispatched).toEqual([]);
  });

  it("reconciles an unsettled red PR with failing check names", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "failing" as const,
          mergeableStatus: null,
          failingChecks: ["lint", "unit"],
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [],
          isOpen: true,
        },
      }),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "red",
      failing: ["lint", "unit"],
      ciSettledAt: Date.parse("2026-08-24T00:00:00.000Z"),
      ciCheckRuns: null,
    });
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "publish",
            role: roleToken("omp", issue, "implementer"),
            payload: {
              type: "ci-settled-red",
              failing: ["lint", "unit"],
              sha: "head-1",
            },
          },
        ],
        envelope: {
          event_id: "resync:sjawhar/legion#7:ci",
          issued_at: Date.parse("2026-08-24T00:00:00.000Z"),
        },
      },
    ]);
  });

  it("moves a stale local head before applying GitHub's green rollup", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
      reviewDecision: "approved",
    };
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
    let fetches = 0;

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => {
        fetches += 1;
        return {
          "sjawhar/legion#7": {
            ciStatus: "passing" as const,
            mergeableStatus: null,
            headSha: "head-2",
            updatedAt: "2026-08-24T00:00:00.000Z",
            checkRuns: [],
            isOpen: true,
          },
        };
      },
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    });

    expect(fetches).toBe(1);
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      headSha: "head-2",
      verdict: "green",
      failing: [],
      ciSettledAt: Date.parse("2026-08-24T00:00:00.000Z"),
      ciCheckRuns: null,
      fixAttempts: 1,
    });
    expect(state.prs["sjawhar/legion#7"]?.reviewDecision).toBeUndefined();
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "publish",
            role: roleToken("omp", issue, "implementer"),
            payload: { type: "ci-green", sha: "head-2" },
          },
        ],
        envelope: {
          event_id: "resync:sjawhar/legion#7:ci",
          issued_at: Date.parse("2026-08-24T00:00:00.000Z"),
        },
      },
    ]);
  });

  // A -> B -> A: GitHub briefly had head B while the daemon read; A came back
  // with a later lifecycle update. The fetched B view is stale either way.
  function prAtHeadA(state: LegionState): PrState {
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    const pr: PrState = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-a",
      headUpdatedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
      ciSettlementGeneration: 1,
      ciSnapshot: "hash-a",
      ciReconciled: false,
      reviewDecision: "approved",
      fixAttempts: 0,
    };
    state.prs["sjawhar/legion#7"] = pr;
    return pr;
  }
  const fetchedHeadB = {
    "sjawhar/legion#7": {
      ciStatus: "failing" as const,
      mergeableStatus: null,
      failingChecks: ["build"],
      headSha: "head-b",
      updatedAt: "2026-08-24T00:00:02.000Z",
      checkRuns: [{ name: "build", id: 950 }],
      isOpen: true,
    },
  };
  const synchronizeBackToA = (state: LegionState) =>
    reduceGithubEvent(
      state,
      "notifications.github.sjawhar.legion.pull_request.synchronize",
      {
        event_id: "back-to-head-a",
        issued_at: Date.parse("2026-08-24T00:00:03.000Z"),
        payload: {
          kind: "pr",
          action: "synchronize",
          repo: "sjawhar/legion",
          number: "7",
          head_sha: "head-a",
          updated_at: "2026-08-24T00:00:03.000Z",
        },
      },
      resyncDeps(state, []).config
    );

  it("skips a fetched head when a same-sha lifecycle update lands during the read", async () => {
    const state = newLegionState("omp", 1);
    const before = structuredClone(prAtHeadA(state));
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => {
        // synchronize(A, t3) arrives while the fetch (which saw B at t2) is outstanding.
        synchronizeBackToA(state);
        return fetchedHeadB;
      },
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toEqual({
      ...before,
      headUpdatedAt: Date.parse("2026-08-24T00:00:03.000Z"),
    });
    expect(dispatched).toEqual([]);
  });

  it("skips a fetched head older than a lifecycle update observed before the read", async () => {
    const state = newLegionState("omp", 1);
    prAtHeadA(state);
    synchronizeBackToA(state);
    const before = structuredClone(state.prs["sjawhar/legion#7"]);
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => fetchedHeadB,
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toEqual(before);
    expect(dispatched).toEqual([]);
  });

  it("fences a resynced head against a redelivered older synchronize", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      headUpdatedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-2",
          updatedAt: "2026-08-24T00:00:02.000Z",
          checkRuns: [{ name: "build", id: 900 }],
          isOpen: true,
        },
      }),
    });

    reduceGithubEvent(
      state,
      "notifications.github.sjawhar.legion.pull_request.synchronize",
      {
        event_id: "redelivered-head-1",
        issued_at: Date.parse("2026-08-24T00:00:01.000Z"),
        payload: {
          kind: "pr",
          action: "synchronize",
          repo: "sjawhar/legion",
          number: "7",
          head_sha: "head-1",
          updated_at: "2026-08-24T00:00:01.000Z",
        },
      },
      resyncDeps(state, []).config
    );

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      headSha: "head-2",
      headUpdatedAt: Date.parse("2026-08-24T00:00:02.000Z"),
      ciCheckRuns: [{ name: "build", id: 900 }],
    });
  });

  it("records the resynced check-run id to fence delayed live settlements", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 900 }],
          isOpen: true,
        },
      }),
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "green",
      ciCheckRuns: [{ name: "build", id: 900 }],
    });
  });

  it("reconciles a stored red verdict to green", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 5 }],
          isOpen: true,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "green",
      failing: [],
      ciSettledAt: Date.parse("2026-08-24T00:00:00.000Z"),
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([
      [
        {
          kind: "publish",
          role: roleToken("omp", issue, "implementer"),
          payload: { type: "ci-green", sha: "head-1" },
        },
      ],
    ]);
  });

  it("reconciles a stored green verdict to red with GitHub's failing checks", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "failing" as const,
          mergeableStatus: null,
          failingChecks: ["lint", "unit"],
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 5 }],
          isOpen: true,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "red",
      failing: ["lint", "unit"],
      ciSettledAt: Date.parse("2026-08-24T00:00:00.000Z"),
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([
      [
        {
          kind: "publish",
          role: roleToken("omp", issue, "implementer"),
          payload: {
            type: "ci-settled-red",
            failing: ["lint", "unit"],
            sha: "head-1",
          },
        },
      ],
    ]);
  });

  it("quietly refreshes an unchanged green verdict", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    let fetches = 0;
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => {
        fetches += 1;
        return {
          "sjawhar/legion#7": {
            ciStatus: "passing" as const,
            mergeableStatus: null,
            headSha: "head-1",
            updatedAt: "2026-08-24T00:00:00.000Z",
            checkRuns: [{ name: "build", id: 5 }],
            isOpen: true,
          },
        };
      },
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(fetches).toBe(1);
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "green",
      ciSettledAt: Date.parse("2026-08-24T00:00:00.000Z"),
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([]);
  });

  it("a pending rollup advances the check-run fence so a delayed older live settlement is ignored", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 1_000 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "pending" as const,
          mergeableStatus: null,
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 1_100 }],
          isOpen: true,
        },
      }),
      applyEffects: async () => {},
    });

    // The rerun's newer run id is the fence now: an old settlement at 1050
    // that arrives late is stale, exactly as if the daemon had seen 1100 live.
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: null,
      ciCheckRuns: [{ name: "build", id: 1_100 }],
      ciSettledAt: 1_000,
    });
  });

  it("a cancelled-only failing rollup uncertifies a green head instead of settling red", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "failing" as const,
          mergeableStatus: null,
          failingChecks: [],
          cancelledCount: 2,
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 9 }],
          isOpen: true,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    // Live intake treats a cancelled-only settlement as "no longer certified",
    // never as red; resync must read GitHub's rollup the same way.
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: null,
      failing: [],
      ciCheckRuns: [{ name: "build", id: 9 }],
      fixAttempts: 0,
    });
    expect(dispatched).toEqual([]);
  });

  it("clears a stored green verdict when GitHub reports a pending rollup", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    let fetches = 0;
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => {
        fetches += 1;
        return {
          "sjawhar/legion#7": {
            ciStatus: "pending" as const,
            mergeableStatus: null,
            headSha: "head-1",
            updatedAt: "2026-08-24T00:00:00.000Z",
            checkRuns: [{ name: "build", id: 5 }],
            isOpen: true,
          },
        };
      },
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(fetches).toBe(1);
    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: null,
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([]);
  });

  it("preserves a stored red verdict when GitHub reports a pending rollup", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "pending" as const,
          mergeableStatus: null,
          headSha: "head-1",
          updatedAt: "2026-08-24T00:00:00.000Z",
          checkRuns: [{ name: "build", id: 5 }],
          isOpen: true,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([]);
  });

  it("skips a closed GitHub PR", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-1",
          updatedAt: null,
          checkRuns: [],
          isOpen: false,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      verdict: null,
      failing: [],
      ciSettledAt: null,
      ciCheckRuns: null,
    });
    expect(dispatched).toEqual([]);
  });

  it("leaves a PR untouched and reports an owner CI fetch failure", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const before = structuredClone(state.prs["sjawhar/legion#7"]);

    const event = await runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          owner: "sjawhar",
          error: "GitHub App token request failed",
        },
      }),
    });

    expect(state.prs["sjawhar/legion#7"]).toEqual(before);
    expect(event).toMatchObject({
      ciFetchFailures: 1,
      ciFetchFailureDetails: [{ owner: "sjawhar", error: "GitHub App token request failed" }],
    });
  });
  it("counts a CI fetch failure even when the PR changed while the fetch was in flight", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const fetchStarted = Promise.withResolvers<void>();
    const fetchedStatuses = Promise.withResolvers<Record<string, CiFetchResult>>();
    const resync = runResync({
      ...resyncDeps(state, []),
      fetchCiStatusBatch: async () => {
        fetchStarted.resolve();
        return fetchedStatuses.promise;
      },
    });
    await fetchStarted.promise;
    state.prs["sjawhar/legion#7"].ciSettledAt = 2_000;
    fetchedStatuses.resolve({
      "sjawhar/legion#7": { owner: "sjawhar", error: "GitHub App token request failed" },
    });

    const event = await resync;

    expect(event).toMatchObject({
      ciFetchFailures: 1,
      ciFetchFailureDetails: [{ owner: "sjawhar", error: "GitHub App token request failed" }],
    });
  });
});
