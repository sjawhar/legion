import { describe, expect, it } from "bun:test";
import { formatIssueKey, roleToken } from "@legion/contracts";
import { type LegionState, newLegionState } from "../legion-state";
import type { Effect, EnvelopeJson } from "../reducers";
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
    });
  });
});
