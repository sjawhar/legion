import { describe, expect, it } from "bun:test";
import { formatIssueKey } from "@legion/contracts";
import { type LegionState, newLegionState } from "../legion-state";
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
    config: { resyncIntervalMs: 600_000 },
    fetchGitHubProjectItems: async () => ({ items }),
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
      excludedNullContentItems: 0,
    });
  });

  it("suppresses every anomaly for a deliberately backlogged issue", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state, { backlogMarker: "waiting for maintainer" });

    const event = await runResync(resyncDeps(state, [boardIssue()]));

    expect(event).toEqual({ type: "resync", anomalies: [], excludedNullContentItems: 0 });
  });

  it("reports an open board issue missing from the daemon state without dispatching it", async () => {
    const state = newLegionState("omp", 1);

    const event = await runResync(resyncDeps(state, [boardIssue()]));

    expect(event).toEqual({
      type: "resync",
      anomalies: [
        {
          kind: "missed-open",
          issue,
          detail: "open board issue is absent from Legion state",
        },
      ],
      excludedNullContentItems: 0,
    });
  });

  it("reports an open board issue with an error project status for controller verification", async () => {
    const state = newLegionState("omp", 1);
    recordOpenReleasedIssue(state);

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
      excludedNullContentItems: 0,
    });
  });
  it("completes the resync while reporting board items excluded for null content", async () => {
    const state = newLegionState("omp", 1);
    const event = await runResync({
      ...resyncDeps(state, []),
      fetchGitHubProjectItems: async () => ({ items: [], excludedNullContentItems: 1 }),
    });

    expect(event).toEqual({
      type: "resync",
      anomalies: [],
      excludedNullContentItems: 1,
    });
  });
});
