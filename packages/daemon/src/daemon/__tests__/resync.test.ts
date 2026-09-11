import { describe, expect, it, vi } from "bun:test";
import { type IssueKey, roleToken } from "@legion/contracts";
import type { CiFetchResult } from "../../state/fetch";
import { type LegionState, newLegionState, type PrState } from "../legion-state";
import { type Effect, type EnvelopeJson, reduceGithubEvent } from "../reducers";
import { type RunResyncDeps, runResync } from "../resync";
import { checkPr, fakeDispatchClient } from "./ci-fixtures";
import issueClosed from "./fixtures/dispatch/issue-closed.json";

const issue = "LEGION-42";

function resyncDeps(state: LegionState): RunResyncDeps {
  return {
    state,
    config: {
      resyncIntervalMs: 600_000,
      dispatchProject: "LEGSMOKE",
      appLogins: [],
      maxFixAttempts: 3,
    },
    dispatchClient: fakeDispatchClient(),
    saveState: async () => {},
    fetchCiStatusBatch: async () => ({}),
    applyEffects: async () => {},
    setApprovalStatus: async () => ({ written: true }),
    now: () => Date.parse("2026-08-24T00:00:00.000Z"),
  };
}

/**
 * Registers `issue` as its own tree root with an active implementer phase, so
 * a settled CI verdict for its PR routes to a worker instead of vanishing for
 * want of a tracked tree.
 */
function trackIssue(state: LegionState): void {
  state.issues[issue] = {
    key: issue,
    title: "Resync this Legion tree",
    status: "in_progress",
    children: [],
  };
  state.trees[issue] = {
    root: issue,
    generation: 1,
    status: "active",
    launchFailures: 0,
  };
  state.phases[issue] = { phase: "implementer", sessionId: "resync-test-worker" };
}

describe("runResync", () => {
  it("settles a reconciled green PR and notifies its approved architect", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "in_progress",
      children: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.roles[roleToken("omp", issue, "architect")] = { issue, role: "architect" };
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      reviewDecision: "approved",
    });
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
    let now = Date.parse("2026-08-24T00:00:00.000Z");
    const fetches: Record<string, unknown>[] = [];
    const deps: RunResyncDeps = {
      ...resyncDeps(state),
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
      failingStatuses: [],
      ciSettledAt: now,
      ciCheckRuns: null,
    });
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "publish",
            role: roleToken("omp", issue, "architect"),
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

  it("options.force bypasses the interval throttle and re-reports an untriaged root, while an unforced run within the same interval stays throttled", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Untriaged root",
      status: "triage",
      children: [],
    };
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
    const deps: RunResyncDeps = {
      ...resyncDeps(state),
      applyEffects: async (effects, envelope) => {
        dispatched.push({ effects, envelope });
      },
    };
    const anomaly = {
      kind: "untriaged-open" as const,
      issue,
      detail: "tracked triage issue has no Legion tree or admission entry",
    };

    const first = await runResync(deps);
    expect(first.anomalies).toEqual([anomaly]);
    expect(dispatched).toEqual([
      {
        effects: [
          {
            kind: "controller",
            payload: { type: "triage", issue, preexistingChildren: [] },
          },
        ],
        envelope: { event_id: `resync:${issue}:triage`, issued_at: expect.any(Number) },
      },
    ]);

    dispatched.length = 0;
    const throttled = await runResync(deps);
    expect(throttled).toEqual({
      type: "resync",
      anomalies: [],
      healed: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    });
    expect(dispatched).toEqual([]);

    const forced = await runResync(deps, { force: true });
    expect(forced.anomalies).toEqual([anomaly]);
    expect(dispatched).toHaveLength(1);
  });

  it("reconciles an unsettled red PR with failing check names", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.prs["sjawhar/legion#7"] = checkPr(issue, { repo: "sjawhar/legion" });
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
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
    trackIssue(state);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
      reviewDecision: "approved",
    });
    const dispatched: Array<{ effects: Effect[]; envelope: EnvelopeJson }> = [];
    let fetches = 0;

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
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
      status: "in_progress",
      children: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.phases[issue] = { phase: "implementer", sessionId: "resync-test-worker" };
    const pr: PrState = {
      key: issue,
      repo: "sjawhar/legion",
      number: 7,
      headSha: "head-a",
      headUpdatedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      verdict: "green",
      failing: [],
      failingStatuses: [],
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
      resyncDeps(state).config
    );

  it("skips a fetched head when a same-sha lifecycle update lands during the read", async () => {
    const state = newLegionState("omp", 1);
    const before = structuredClone(prAtHeadA(state));
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      headUpdatedAtSource: "webhook",
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
      ...resyncDeps(state),
      fetchCiStatusBatch: async () => fetchedHeadB,
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(state.prs["sjawhar/legion#7"]).toEqual(before);
    expect(dispatched).toEqual([]);
  });

  it("a same-head read advances the lifecycle clock so a delayed intervening-head synchronize is rejected", async () => {
    const state = newLegionState("omp", 1);
    const pr = prAtHeadA(state);
    const before = structuredClone(pr);

    // GitHub confirms head A at t3 (it had been at B between t0 and t3).
    await runResync({
      ...resyncDeps(state),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-a",
          updatedAt: "2026-08-24T00:00:03.000Z",
          checkRuns: [{ name: "build", id: 900 }],
          isOpen: true,
        },
      }),
    });
    expect(pr).toMatchObject({
      headSha: "head-a",
      headUpdatedAt: Date.parse("2026-08-24T00:00:03.000Z"),
    });

    // The delayed synchronize for B at t2 is older than what GitHub confirmed: rejected.
    reduceGithubEvent(
      state,
      "notifications.github.sjawhar.legion.pull_request.synchronize",
      {
        event_id: "delayed-head-b",
        issued_at: Date.parse("2026-08-24T00:00:02.000Z"),
        payload: {
          kind: "pr",
          action: "synchronize",
          repo: "sjawhar/legion",
          number: "7",
          head_sha: "head-b",
          updated_at: "2026-08-24T00:00:02.000Z",
        },
      },
      resyncDeps(state).config
    );
    // Still head A at t3, green, live fence intact; B never landed.
    expect(pr).toMatchObject({
      headSha: "head-a",
      headUpdatedAt: Date.parse("2026-08-24T00:00:03.000Z"),
      verdict: "green",
      ciCheckRuns: before.ciCheckRuns,
      ciSettlementGeneration: before.ciSettlementGeneration,
      reviewDecision: "approved",
    });
  });

  it("out-of-order equal-clock synchronizes converge on GitHub's read of the current head", async () => {
    // Two pushes within one second: B then C, delivered C first. The stale B
    // arrives at the same clock and is accepted (a webhook cannot tell); the
    // next read of the current head C, at that same clock, must win.
    const state = newLegionState("omp", 1);
    const pr = prAtHeadA(state);
    const T = "2026-08-24T00:00:05.000Z";
    const synchronize = (sha: string) =>
      reduceGithubEvent(
        state,
        "notifications.github.sjawhar.legion.pull_request.synchronize",
        {
          event_id: `sync-${sha}`,
          issued_at: Date.parse(T),
          payload: {
            kind: "pr",
            action: "synchronize",
            repo: "sjawhar/legion",
            number: "7",
            head_sha: sha,
            updated_at: T,
          },
        },
        resyncDeps(state).config
      );
    synchronize("head-c");
    synchronize("head-b");
    expect(pr).toMatchObject({ headSha: "head-b", headUpdatedAt: Date.parse(T) });

    const dispatched: Effect[][] = [];
    await runResync({
      ...resyncDeps(state),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-c",
          updatedAt: T,
          checkRuns: [{ name: "build", id: 990 }],
          isOpen: true,
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });
    expect(pr).toMatchObject({
      headSha: "head-c",
      headUpdatedAt: Date.parse(T),
      verdict: "green",
      ciCheckRuns: [{ name: "build", id: 990 }],
    });
    expect(
      dispatched.flat().map((effect) => (effect.kind === "publish" ? effect.payload : effect.kind))
    ).toEqual([{ type: "ci-green", sha: "head-c" }]);
  });

  it("a resync-sourced head read outranks a same-clock webhook synchronize that disagrees with it", async () => {
    // Unlike two webhooks racing each other at the same clock (the test
    // above, a legitimate same-second sequence), a webhook that arrives
    // after GitHub's own authoritative read has already settled that
    // instant must not re-open the tie: resync wins regardless of order.
    const state = newLegionState("omp", 1);
    const pr = prAtHeadA(state);
    const T = "2026-08-24T00:00:05.000Z";

    await runResync({
      ...resyncDeps(state),
      fetchCiStatusBatch: async () => ({
        "sjawhar/legion#7": {
          ciStatus: "passing" as const,
          mergeableStatus: null,
          headSha: "head-c",
          updatedAt: T,
          checkRuns: [{ name: "build", id: 990 }],
          isOpen: true,
        },
      }),
    });
    expect(pr).toMatchObject({
      headSha: "head-c",
      headUpdatedAt: Date.parse(T),
      headUpdatedAtSource: "resync",
    });

    reduceGithubEvent(
      state,
      "notifications.github.sjawhar.legion.pull_request.synchronize",
      {
        event_id: "webhook-head-b",
        issued_at: Date.parse(T),
        payload: {
          kind: "pr",
          action: "synchronize",
          repo: "sjawhar/legion",
          number: "7",
          head_sha: "head-b",
          updated_at: T,
        },
      },
      resyncDeps(state).config
    );

    expect(pr).toMatchObject({
      headSha: "head-c",
      headUpdatedAt: Date.parse(T),
      headUpdatedAtSource: "resync",
    });
  });
  it("fences a resynced head against a redelivered older synchronize", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "in_progress",
      children: [],
    };
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      headUpdatedAt: Date.parse("2026-08-24T00:00:00.000Z"),
    });

    await runResync({
      ...resyncDeps(state),
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
      resyncDeps(state).config
    );

    expect(state.prs["sjawhar/legion#7"]).toMatchObject({
      headSha: "head-2",
      headUpdatedAt: Date.parse("2026-08-24T00:00:02.000Z"),
      ciCheckRuns: [{ name: "build", id: 900 }],
    });
  });

  it("records the resynced check-run id to fence delayed live settlements", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = checkPr(issue, { repo: "sjawhar/legion" });

    await runResync({
      ...resyncDeps(state),
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
    trackIssue(state);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
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
    trackIssue(state);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
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
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    let fetches = 0;
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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

  it("a cancelled-only failing rollup uncertifies a green head instead of settling red", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    let fetches = 0;
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([]);
  });

  it("preserves a stored red verdict when GitHub reports a pending rollup", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "red",
      failing: ["unit"],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 5 }],
    });
    expect(dispatched).toEqual([]);
  });

  it("skips a closed GitHub PR", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = checkPr(issue, { repo: "sjawhar/legion" });
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
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
      failingStatuses: [],
      ciSettledAt: null,
      ciCheckRuns: null,
    });
    expect(dispatched).toEqual([]);
  });

  it("leaves a PR untouched and reports an owner CI fetch failure", async () => {
    const state = newLegionState("omp", 1);
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
    });
    const before = structuredClone(state.prs["sjawhar/legion#7"]);

    const event = await runResync({
      ...resyncDeps(state),
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
    state.prs["sjawhar/legion#7"] = checkPr(issue, {
      repo: "sjawhar/legion",
      verdict: "green",
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
    });
    const fetchStarted = Promise.withResolvers<void>();
    const fetchedStatuses = Promise.withResolvers<Record<string, CiFetchResult>>();
    const resync = runResync({
      ...resyncDeps(state),
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
  it("retries a pending non-done write when the remote status still matches the record fence", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "in_progress",
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "in_progress", statusAtRecord: "todo" };
    const statusWrites: Array<{ issue: string; status: string }> = [];

    await runResync({
      ...resyncDeps(state),
      dispatchClient: fakeDispatchClient({
        getIssue: async () => ({ status: "todo" }) as never,
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });
    expect(statusWrites).toEqual([{ issue, status: "in_progress" }]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
  });
  it("replays a missed terminal root status as issue.closed without a detail read", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    const dispatched: Effect[][] = [];
    let detailReads = 0;

    await runResync({
      ...resyncDeps(state),
      dispatchClient: fakeDispatchClient({
        listIssues: async () =>
          [
            {
              key: issue,
              title: "Resync this Legion tree",
              status: "done",
              parent: null,
              updated_at: "2026-09-10T00:00:00Z",
              last_seq: 41,
              open_asks: 0,
            },
          ] as never,
        getIssue: async () => {
          detailReads += 1;
          return issueClosed.payload as never;
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(dispatched).toEqual([[{ kind: "linger", tree: issue }]]);
    expect(state.issues[issue]).toMatchObject({ status: "done", lastAppliedSeq: 41 });
    expect(detailReads).toBe(0);
  });

  it("replays a missed terminal child status as child-closed and children-complete", async () => {
    const child = "LEGION-43" as IssueKey;
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.issues[issue].children.push(child);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: issue,
      status: "in_progress",
      children: [],
    };
    state.roles[roleToken("omp", issue, "architect")] = { issue, role: "architect" };
    const dispatched: Effect[][] = [];
    let detailReads = 0;

    await runResync({
      ...resyncDeps(state),
      dispatchClient: fakeDispatchClient({
        listIssues: async () =>
          [
            {
              key: child,
              title: "Child",
              status: "done",
              parent: issue,
              updated_at: "2026-09-10T00:00:00Z",
              last_seq: 52,
              open_asks: 0,
            },
          ] as never,
        getIssue: async () => {
          detailReads += 1;
          return { ...issueClosed.payload, key: child, parent: issue, last_seq: 52 } as never;
        },
      }),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(dispatched).toEqual([
      [
        {
          kind: "publish",
          role: roleToken("omp", issue, "implementer"),
          payload: { type: "child-closed", child, remaining: 0 },
        },
        {
          kind: "publish",
          role: roleToken("omp", issue, "implementer"),
          payload: { type: "children-complete" },
        },
      ],
    ]);
    expect(state.issues[child]).toMatchObject({ status: "done", lastAppliedSeq: 52 });
    expect(detailReads).toBe(0);
  });

  it("drops a pending done write only after Dispatch confirms the issue is already done", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "done",
      lastAppliedSeq: 12,
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "done", statusAtRecord: "retro" };
    let detailReads = 0;
    let saves = 0;
    const statusWrites: Array<{ issue: string; status: string }> = [];

    await runResync({
      ...resyncDeps(state),
      saveState: async () => {
        saves += 1;
      },
      dispatchClient: fakeDispatchClient({
        getIssue: async () => {
          detailReads += 1;
          return issueClosed.payload as never;
        },
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });

    expect(detailReads).toBe(1);
    expect(statusWrites).toEqual([]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
    expect(saves).toBe(1);
  });

  it("never overwrites a remote human reopen with a pending done write", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "done",
      lastAppliedSeq: 12,
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "done", statusAtRecord: "retro" };
    const statusWrites: Array<{ issue: string; status: string }> = [];
    let detailReads = 0;

    await runResync({
      ...resyncDeps(state),
      dispatchClient: fakeDispatchClient({
        listIssues: async () =>
          [
            {
              key: issue,
              title: "Resync this Legion tree",
              status: "backlog",
              parent: null,
              updated_at: "2026-09-10T00:00:00Z",
              last_seq: 13,
              open_asks: 0,
            },
          ] as never,
        getIssue: async () => {
          detailReads += 1;
          return { ...issueClosed.payload, status: "backlog", last_seq: 13 } as never;
        },
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });

    expect(detailReads).toBe(1);
    expect(statusWrites).toEqual([]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
    expect(state.issues[issue]).toMatchObject({ status: "backlog", lastAppliedSeq: 13 });
  });
  it("drops a pending in-progress write after a remote human icebox update", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "in_progress",
      lastAppliedSeq: 12,
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "in_progress", statusAtRecord: "todo" };
    const statusWrites: Array<{ issue: string; status: string }> = [];

    await runResync({
      ...resyncDeps(state),
      dispatchClient: fakeDispatchClient({
        listIssues: async () =>
          [
            {
              key: issue,
              title: "Resync this Legion tree",
              status: "icebox",
              parent: null,
              updated_at: "2026-09-10T00:00:00Z",
              last_seq: 13,
              open_asks: 0,
            },
          ] as never,
        getIssue: async () => ({ status: "icebox" }) as never,
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });

    expect(statusWrites).toEqual([]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
    expect(state.issues[issue]).toMatchObject({ status: "icebox", lastAppliedSeq: 13 });
  });

  it("persists pending-write cleanup after a successful retry without a drift effect", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "testing",
      lastAppliedSeq: 8,
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "testing", statusAtRecord: "in_progress" };
    const statusWrites: Array<{ issue: string; status: string }> = [];
    let saves = 0;

    await runResync({
      ...resyncDeps(state),
      saveState: async () => {
        saves += 1;
      },
      dispatchClient: fakeDispatchClient({
        getIssue: async () => ({ status: "in_progress" }) as never,
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });

    expect(statusWrites).toEqual([{ issue, status: "testing" }]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
    expect(saves).toBe(1);
  });

  it("drops a pending write once the remote status no longer matches the record fence", async () => {
    const state = newLegionState("omp", 1);
    state.issues[issue] = {
      key: issue,
      title: "Resync this Legion tree",
      status: "backlog",
      lastAppliedSeq: 13,
      children: [],
    };
    state.pendingStatusWrites[issue] = { status: "in_progress", statusAtRecord: "todo" };
    const statusWrites: Array<{ issue: string; status: string }> = [];
    let saves = 0;

    await runResync({
      ...resyncDeps(state),
      saveState: async () => {
        saves += 1;
      },
      dispatchClient: fakeDispatchClient({
        getIssue: async () => ({ status: "backlog" }) as never,
        setStatus: async (writtenIssue, status) => {
          statusWrites.push({ issue: writtenIssue, status });
        },
      }),
    });

    expect(statusWrites).toEqual([]);
    expect(state.pendingStatusWrites[issue]).toBeUndefined();
    expect(saves).toBe(1);
  });

  it("retries a pending approval-status write against the failing sha and clears it on success", async () => {
    const state = newLegionState("omp", 1);
    state.approvalStatusPending["acme/widgets#7"] = {
      sha: "head-1",
      lastError: "boom",
      attempts: 2,
      at: Date.parse("2026-08-23T00:00:00.000Z"),
    };
    const attempted: Array<{ repo: string; pr: number; sha: string }> = [];

    await runResync({
      ...resyncDeps(state),
      setApprovalStatus: async (effect) => {
        attempted.push({ repo: effect.repo, pr: effect.pr, sha: effect.sha });
        return { written: true };
      },
    });

    expect(attempted).toEqual([{ repo: "acme/widgets", pr: 7, sha: "head-1" }]);
    expect(state.approvalStatusPending["acme/widgets#7"]).toBeUndefined();
  });

  it("keeps retrying a still-failing approval-status write and bumps its attempt count without a second warning", async () => {
    const state = newLegionState("omp", 1);
    state.approvalStatusPending["acme/widgets#7"] = {
      sha: "head-1",
      lastError: "old reason",
      attempts: 1,
      at: Date.parse("2026-08-23T00:00:00.000Z"),
    };
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    try {
      await runResync({
        ...resyncDeps(state),
        setApprovalStatus: async () => ({
          written: false,
          permanent: true,
          reason: "still failing",
        }),
      });

      expect(state.approvalStatusPending["acme/widgets#7"]).toMatchObject({
        sha: "head-1",
        lastError: "still failing",
        attempts: 2,
      });
      // Already warned once for this sha (attempts started at 1, not 0): resync's retry must
      // not repeat the same permission-denied warning on every cycle.
      expect(warn).not.toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });

  it("probes every active tree with a confirmed ready root and a recorded locator", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.trees[issue].locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
    };
    state.trees[issue].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    const dispatched: Effect[][] = [];

    const event = await runResync({
      ...resyncDeps(state),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    // The probe is emitted even though this run is otherwise anomaly-free: it is a liveness
    // backstop, not a symptom of a bookkeeping anomaly `reportRootAnomalies` would already catch.
    expect(event.anomalies).toEqual([]);
    expect(dispatched).toContainEqual([{ kind: "probe", tree: issue }]);
  });

  it("does not probe an active tree whose root has not yet confirmed ready", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.trees[issue].locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
    };
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(dispatched).toEqual([]);
  });

  it("does not probe a lingering tree even with a confirmed, located root", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.trees[issue].status = "lingering";
    state.trees[issue].locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
    };
    state.trees[issue].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(dispatched).toEqual([]);
  });

  it("does not probe an active tree with no recorded locator", async () => {
    const state = newLegionState("omp", 1);
    trackIssue(state);
    state.trees[issue].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    const dispatched: Effect[][] = [];

    await runResync({
      ...resyncDeps(state),
      applyEffects: async (effects) => {
        dispatched.push(effects);
      },
    });

    expect(dispatched).toEqual([]);
  });
});
