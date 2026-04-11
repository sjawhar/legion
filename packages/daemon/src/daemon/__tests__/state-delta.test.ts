import { describe, expect, it } from "bun:test";

import type { IssueStateDict } from "../../state/types";
import { computeStateDelta } from "../state-delta";

function createIssueState(overrides: Partial<IssueStateDict> = {}): IssueStateDict {
  return {
    status: "Todo",
    labels: [],
    hasPr: false,
    prIsDraft: null,
    ciStatus: null,
    mergeableStatus: null,
    hasLiveWorker: false,
    workerMode: null,
    workerStatus: null,
    suggestedAction: "skip",
    sessionId: "session-1",
    hasUserFeedback: false,
    isBlocked: false,
    blockedByIds: [],
    source: null,
    ...overrides,
  };
}

describe("computeStateDelta", () => {
  it("returns null for two empty snapshots", () => {
    expect(computeStateDelta({}, {})).toBeNull();
  });

  it("detects new issues", () => {
    const originalDateNow = Date.now;
    Date.now = () => 111;

    try {
      const current = {
        "issue-1": createIssueState(),
      };

      expect(computeStateDelta({}, current)).toEqual({
        type: "state_delta",
        timestamp: 111,
        changes: {
          new: [
            {
              issueId: "issue-1",
              state: current["issue-1"],
            },
          ],
          removed: [],
          changed: [],
          summary: "1 new, 0 removed, 0 changed",
        },
      });
    } finally {
      Date.now = originalDateNow;
    }
  });

  it("detects removed issues", () => {
    const originalDateNow = Date.now;
    Date.now = () => 222;

    try {
      const previous = {
        "issue-1": createIssueState(),
      };

      expect(computeStateDelta(previous, {})).toEqual({
        type: "state_delta",
        timestamp: 222,
        changes: {
          new: [],
          removed: ["issue-1"],
          changed: [],
          summary: "0 new, 1 removed, 0 changed",
        },
      });
    } finally {
      Date.now = originalDateNow;
    }
  });

  it("detects changed issues with a single tracked field diff", () => {
    const originalDateNow = Date.now;
    Date.now = () => 333;

    try {
      const previous = {
        "issue-1": createIssueState({ status: "Todo" }),
      };
      const current = {
        "issue-1": createIssueState({ status: "In Progress" }),
      };

      expect(computeStateDelta(previous, current)).toEqual({
        type: "state_delta",
        timestamp: 333,
        changes: {
          new: [],
          removed: [],
          changed: [
            {
              issueId: "issue-1",
              state: current["issue-1"],
              changedFields: ["status"],
            },
          ],
          summary: "0 new, 0 removed, 1 changed",
        },
      });
    } finally {
      Date.now = originalDateNow;
    }
  });

  it("detects changed issues with multiple tracked field diffs", () => {
    const previous = {
      "issue-1": createIssueState({ status: "Todo", hasPr: false, isBlocked: false }),
    };
    const current = {
      "issue-1": createIssueState({ status: "Testing", hasPr: true, isBlocked: true }),
    };

    expect(computeStateDelta(previous, current)).toEqual({
      type: "state_delta",
      timestamp: expect.any(Number),
      changes: {
        new: [],
        removed: [],
        changed: [
          {
            issueId: "issue-1",
            state: current["issue-1"],
            changedFields: ["status", "hasPr", "isBlocked"],
          },
        ],
        summary: "0 new, 0 removed, 1 changed",
      },
    });
  });

  it("returns null for identical non-empty snapshots", () => {
    const snapshot = {
      "issue-1": createIssueState({ status: "Testing", hasPr: true, labels: ["worker-done"] }),
    };

    expect(computeStateDelta(snapshot, snapshot)).toBeNull();
  });

  it("treats label order as invariant", () => {
    const previous = {
      "issue-1": createIssueState({ labels: ["worker-done", "test-passed"] }),
    };
    const current = {
      "issue-1": createIssueState({ labels: ["test-passed", "worker-done"] }),
    };

    expect(computeStateDelta(previous, current)).toBeNull();
  });

  it("deduplicates labels before comparing them", () => {
    const previous = {
      "issue-1": createIssueState({ labels: ["worker-done", "test-passed", "test-passed"] }),
    };
    const current = {
      "issue-1": createIssueState({ labels: ["test-passed", "worker-done"] }),
    };

    expect(computeStateDelta(previous, current)).toBeNull();
  });

  it("handles mixed new removed and changed issues in one delta", () => {
    const previous = {
      changed: createIssueState({ status: "Todo" }),
      removed: createIssueState({ status: "Backlog" }),
    };
    const current = {
      changed: createIssueState({ status: "In Progress" }),
      added: createIssueState({ status: "Testing" }),
    };

    expect(computeStateDelta(previous, current)).toEqual({
      type: "state_delta",
      timestamp: expect.any(Number),
      changes: {
        new: [
          {
            issueId: "added",
            state: current.added,
          },
        ],
        removed: ["removed"],
        changed: [
          {
            issueId: "changed",
            state: current.changed,
            changedFields: ["status"],
          },
        ],
        summary: "1 new, 1 removed, 1 changed",
      },
    });
  });

  it("ignores non-tracked field changes", () => {
    const previous = {
      "issue-1": createIssueState({
        workerMode: "implement",
        workerStatus: "running",
        sessionId: "session-1",
        mergeableStatus: "unknown",
        hasUserFeedback: false,
      }),
    };
    const current = {
      "issue-1": createIssueState({
        workerMode: "review",
        workerStatus: "dead",
        sessionId: "session-2",
        mergeableStatus: "mergeable",
        hasUserFeedback: true,
      }),
    };

    expect(computeStateDelta(previous, current)).toBeNull();
  });

  it("detects ciStatus changes from null to passing", () => {
    const previous = {
      "issue-1": createIssueState({ ciStatus: null }),
    };
    const current = {
      "issue-1": createIssueState({ ciStatus: "passing" }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["ciStatus"],
      },
    ]);
  });

  it("detects ciStatus changes from passing to null", () => {
    const previous = {
      "issue-1": createIssueState({ ciStatus: "passing" }),
    };
    const current = {
      "issue-1": createIssueState({ ciStatus: null }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["ciStatus"],
      },
    ]);
  });

  it("detects prIsDraft changes", () => {
    const previous = {
      "issue-1": createIssueState({ prIsDraft: true }),
    };
    const current = {
      "issue-1": createIssueState({ prIsDraft: false }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["prIsDraft"],
      },
    ]);
  });

  it("detects hasPr changes", () => {
    const previous = {
      "issue-1": createIssueState({ hasPr: false }),
    };
    const current = {
      "issue-1": createIssueState({ hasPr: true }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["hasPr"],
      },
    ]);
  });

  it("detects isBlocked changes", () => {
    const previous = {
      "issue-1": createIssueState({ isBlocked: false }),
    };
    const current = {
      "issue-1": createIssueState({ isBlocked: true }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["isBlocked"],
      },
    ]);
  });

  it("detects suggestedAction changes", () => {
    const previous = {
      "issue-1": createIssueState({ suggestedAction: "skip" }),
    };
    const current = {
      "issue-1": createIssueState({ suggestedAction: "dispatch_tester" }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["suggestedAction"],
      },
    ]);
  });

  it("detects hasLiveWorker changes", () => {
    const previous = {
      "issue-1": createIssueState({ hasLiveWorker: false }),
    };
    const current = {
      "issue-1": createIssueState({ hasLiveWorker: true }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["hasLiveWorker"],
      },
    ]);
  });

  it("detects label set changes", () => {
    const previous = {
      "issue-1": createIssueState({ labels: ["worker-done"] }),
    };
    const current = {
      "issue-1": createIssueState({ labels: ["worker-done", "test-passed"] }),
    };

    expect(computeStateDelta(previous, current)?.changes.changed).toEqual([
      {
        issueId: "issue-1",
        state: current["issue-1"],
        changedFields: ["labels"],
      },
    ]);
  });
});
