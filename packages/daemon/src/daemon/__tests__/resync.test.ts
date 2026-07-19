import { describe, expect, it, mock } from "bun:test";
import { IssueStatus, WorkerMode, type WorkerModeLiteral } from "../../state/types";
import {
  type IssueRef,
  noPrPhaseArtifacts,
  type PhaseArtifactBatch,
  type PhaseArtifacts,
} from "../phase-artifacts";
import {
  computeResyncRecommendations,
  type Recommendation,
  type ResyncInput,
  type RunResyncDeps,
  runResyncPass,
} from "../resync";

// allow: SIZE_OK — binding keeps the resync unit and integration matrices in this file.

const issueRef: IssueRef = {
  issueId: "acme-api-42",
  status: IssueStatus.IN_PROGRESS,
  source: { owner: "acme", repo: "api", number: 42 },
  prRef: { owner: "acme", repo: "api", number: 101 },
};

function artifacts(overrides: Partial<PhaseArtifacts> = {}): PhaseArtifacts {
  return { ...noPrPhaseArtifacts(), ...overrides };
}

function resolvedArtifacts(): PhaseArtifacts["resolved"] {
  return {
    ...artifacts().resolved,
    headSha: "resolved",
    testerCheckOnHead: "resolved",
    architectCheckOnHead: "resolved",
    nativeReviewOnHead: "resolved",
    autoMergeEnabledOrMerged: "resolved",
  };
}

function resyncDeps(overrides: Partial<RunResyncDeps> = {}): RunResyncDeps {
  return {
    listNonTerminalIssues: () => [issueRef],
    fetchPhaseArtifactsBatch: async () => ({
      artifacts: { [issueRef.issueId]: artifacts() },
      errors: [],
    }),
    getLiveWorkers: async () => ({}),
    getSessionStatus: async () => ({ data: undefined }),
    emitToController: () => {},
    ...overrides,
  };
}

function resyncInput(overrides: Partial<ResyncInput> = {}): ResyncInput {
  return {
    issueId: issueRef.issueId,
    status: IssueStatus.IN_PROGRESS,
    artifacts: artifacts(),
    liveWorkerModes: [],
    ...overrides,
  };
}

function recommendation(
  mode: WorkerModeLiteral,
  reason: "artifact_no_live_owner" | "idle_missing_phase_artifact"
): Recommendation {
  return { issueId: issueRef.issueId, mode, reason };
}

const noOwnerRecommendation = [recommendation(WorkerMode.IMPLEMENT, "artifact_no_live_owner")];
const architectVetoRecommendation: readonly Recommendation[] = [
  { issueId: issueRef.issueId, mode: null, reason: "architect_veto" },
];

function resyncEvent(
  recommendations: readonly Recommendation[],
  errors: readonly { issueId: string; message: string }[]
) {
  return { type: "legion.resync", recommendations, errors };
}

function prArtifactsWithArchitectCheck(
  architectCheckOnHead: PhaseArtifacts["architectCheckOnHead"]
): PhaseArtifacts {
  return artifacts({
    hasNonDraftPr: true,
    headSha: "head",
    testerCheckOnHead: "success",
    nativeReviewOnHead: "approved",
    architectCheckOnHead,
    resolved: resolvedArtifacts(),
  });
}

describe("computeResyncRecommendations", () => {
  const cases = [
    {
      name: "recommends a tester when a ready PR has no tester verdict or live owner",
      input: {
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          resolved: resolvedArtifacts(),
        }),
      },
      expected: [recommendation(WorkerMode.TEST, "artifact_no_live_owner")],
    },
    {
      name: "does not recommend an owner that is already live",
      input: {
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          resolved: resolvedArtifacts(),
        }),
        liveWorkerModes: [WorkerMode.TEST],
      },
      expected: [],
    },
    {
      name: "does not recommend work for terminal issues",
      input: { status: IssueStatus.DONE },
      expected: [],
    },
    {
      name: "does not recommend work while native auto-merge is waiting",
      input: {
        status: IssueStatus.NEEDS_REVIEW,
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          testerCheckOnHead: "success",
          nativeReviewOnHead: "approved",
          architectCheckOnHead: "success",
          autoMergeEnabledOrMerged: true,
          resolved: resolvedArtifacts(),
        }),
      },
      expected: [],
    },
  ];

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(computeResyncRecommendations([resyncInput(testCase.input)])).toEqual(
        testCase.expected
      );
    });
  }
});

describe("runResyncPass", () => {
  const idleAdvisoryCases = [
    {
      name: "emits an idle advisory when a live implementer has no pull request",
      artifacts: artifacts(),
      mode: WorkerMode.IMPLEMENT,
      status: IssueStatus.IN_PROGRESS,
      sessionStatus: { data: { type: "idle" } },
      expected: [recommendation(WorkerMode.IMPLEMENT, "idle_missing_phase_artifact")],
    },
    {
      name: "does not emit an idle advisory when a live implementer is busy",
      artifacts: artifacts(),
      mode: WorkerMode.IMPLEMENT,
      status: IssueStatus.IN_PROGRESS,
      sessionStatus: { data: { type: "busy" } },
      expected: [],
    },
    {
      name: "does not emit an idle advisory when a live implementer has a pull request",
      artifacts: artifacts({ hasNonDraftPr: true }),
      mode: WorkerMode.IMPLEMENT,
      status: IssueStatus.IN_PROGRESS,
      sessionStatus: { data: { type: "idle" } },
      expected: [],
    },
    {
      name: "does not emit an idle advisory for a live planner",
      artifacts: artifacts(),
      mode: WorkerMode.PLAN,
      status: IssueStatus.TODO,
      sessionStatus: { data: { type: "idle" } },
      expected: [],
    },
    {
      name: "does not emit an idle advisory when the session status is unresolvable",
      artifacts: artifacts(),
      mode: WorkerMode.IMPLEMENT,
      status: IssueStatus.IN_PROGRESS,
      sessionStatus: { data: undefined },
      expected: [],
    },
  ];

  for (const testCase of idleAdvisoryCases) {
    it(testCase.name, async () => {
      // Given a live worker and its already-fetched phase artifacts.
      const emitToController = mock(() => {});
      const getSessionStatus = mock(async () => testCase.sessionStatus);
      const deps = resyncDeps({
        listNonTerminalIssues: () => [{ ...issueRef, status: testCase.status }],
        fetchPhaseArtifactsBatch: async () => ({
          artifacts: { [issueRef.issueId]: testCase.artifacts },
          errors: [],
        }),
        getLiveWorkers: async () => ({
          [issueRef.issueId]: [
            {
              sessionId: "ses-worker",
              mode: testCase.mode,
              status: "running",
            },
          ],
        }),
        getSessionStatus,
        emitToController,
      });

      // When the resync pass observes the worker session.
      const result = await runResyncPass(deps);

      // Then it emits only the expected advisory recommendation.
      expect(getSessionStatus).toHaveBeenCalledWith("ses-worker");
      expect(result.recommendations).toEqual(testCase.expected);
      if (testCase.expected.length > 0) {
        expect(emitToController).toHaveBeenCalledWith(resyncEvent(testCase.expected, []));
      } else {
        expect(emitToController).not.toHaveBeenCalled();
      }
    });
  }

  it("emits a tester recommendation when only a stale implementer is live", async () => {
    // Given a ready pull request, but only its earlier implementer is still tracked.
    const testerArtifacts = artifacts({
      hasNonDraftPr: true,
      headSha: "head",
      resolved: resolvedArtifacts(),
    });
    const deps = resyncDeps({
      fetchPhaseArtifactsBatch: async () => ({
        artifacts: { [issueRef.issueId]: testerArtifacts },
        errors: [],
      }),
      getLiveWorkers: async () => ({
        [issueRef.issueId]: [
          { sessionId: "ses-implement", mode: WorkerMode.IMPLEMENT, status: "running" },
        ],
      }),
      getSessionStatus: async () => ({ data: { type: "busy" } }),
    });

    // When resync determines the missing phase owner.
    const result = await runResyncPass(deps);

    // Then the stale implementer does not suppress the required tester.
    expect(result.recommendations).toEqual([
      recommendation(WorkerMode.TEST, "artifact_no_live_owner"),
    ]);
  });

  it("suppresses a tester recommendation when the expected tester is live", async () => {
    // Given a ready pull request whose expected tester is still active.
    const getSessionStatus = mock(async () => ({ data: { type: "busy" } }));
    const deps = resyncDeps({
      fetchPhaseArtifactsBatch: async () => ({
        artifacts: {
          [issueRef.issueId]: artifacts({
            hasNonDraftPr: true,
            headSha: "head",
            resolved: resolvedArtifacts(),
          }),
        },
        errors: [],
      }),
      getLiveWorkers: async () => ({
        [issueRef.issueId]: [{ sessionId: "ses-test", mode: WorkerMode.TEST, status: "running" }],
      }),
      getSessionStatus,
    });

    // When resync checks the active tester.
    const result = await runResyncPass(deps);

    // Then it preserves the no-duplicate recommendation behavior and observes that tester.
    expect(result.recommendations).toEqual([]);
    expect(getSessionStatus).toHaveBeenCalledWith("ses-test");
  });

  for (const testCase of [
    { name: "without a live worker", liveWorkerModes: [] },
    { name: "with a live architect", liveWorkerModes: [WorkerMode.ARCHITECT] },
  ] satisfies readonly {
    readonly name: string;
    readonly liveWorkerModes: readonly WorkerModeLiteral[];
  }[]) {
    it(`emits one architect veto recommendation ${testCase.name}`, async () => {
      // Given a resolved architect gate failure after all earlier gates have passed.
      const emitToController = mock(() => {});
      const deps = resyncDeps({
        fetchPhaseArtifactsBatch: async () => ({
          artifacts: { [issueRef.issueId]: prArtifactsWithArchitectCheck("failure") },
          errors: [],
        }),
        getLiveWorkers: async () => ({
          [issueRef.issueId]: testCase.liveWorkerModes.map((mode) => ({
            sessionId: "ses-architect",
            mode,
            status: "running",
          })),
        }),
        getSessionStatus: async () => ({ data: { type: "busy" } }),
        emitToController,
      });

      // When the universal repair pass observes the missed architect veto.
      const result = await runResyncPass(deps);

      // Then the controller receives only the veto judgment, not a worker-owner recommendation.
      expect(result.recommendations).toEqual(architectVetoRecommendation);
      expect(emitToController).toHaveBeenCalledTimes(1);
      expect(emitToController).toHaveBeenCalledWith(resyncEvent(architectVetoRecommendation, []));
    });
  }

  it("does not emit an architect veto when the architect check succeeds", async () => {
    // Given a successful architect check and auto-merge enabled on a ready pull request.
    const emitToController = mock(() => {});
    const deps = resyncDeps({
      fetchPhaseArtifactsBatch: async () => ({
        artifacts: {
          [issueRef.issueId]: artifacts({
            ...prArtifactsWithArchitectCheck("success"),
            autoMergeEnabledOrMerged: true,
          }),
        },
        errors: [],
      }),
      emitToController,
    });

    // When the resync pass evaluates the completed gates.
    const result = await runResyncPass(deps);

    // Then it preserves the existing no-recommendation outcome.
    expect(result.recommendations).toEqual([]);
    expect(emitToController).not.toHaveBeenCalled();
  });

  it("evaluates each live worker for one issue against its own phase artifact", async () => {
    // Given an idle implementer with a pull request and an idle tester without its verdict.
    const getSessionStatus = mock(async () => ({ data: { type: "idle" } }));
    const deps = resyncDeps({
      fetchPhaseArtifactsBatch: async () => ({
        artifacts: {
          [issueRef.issueId]: artifacts({
            hasNonDraftPr: true,
            headSha: "head",
            resolved: resolvedArtifacts(),
          }),
        },
        errors: [],
      }),
      getLiveWorkers: async () => ({
        [issueRef.issueId]: [
          { sessionId: "ses-implement", mode: WorkerMode.IMPLEMENT, status: "running" },
          { sessionId: "ses-test", mode: WorkerMode.TEST, status: "running" },
        ],
      }),
      getSessionStatus,
    });

    // When resync inspects every live worker for the issue.
    const result = await runResyncPass(deps);

    // Then only the tester receives an idle advisory.
    expect(result.recommendations).toEqual([
      recommendation(WorkerMode.TEST, "idle_missing_phase_artifact"),
    ]);
    expect(getSessionStatus).toHaveBeenCalledTimes(2);
    expect(getSessionStatus).toHaveBeenCalledWith("ses-implement");
    expect(getSessionStatus).toHaveBeenCalledWith("ses-test");
  });

  it("isolates a failed issue and surfaces its artifact error", async () => {
    const failedRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };
    const emitToController = mock(() => {});
    const fetchPhaseArtifactsBatch = mock(
      async (): Promise<PhaseArtifactBatch> => ({
        artifacts: {
          [issueRef.issueId]: artifacts(),
          [failedRef.issueId]: artifacts({
            resolved: {
              ...artifacts().resolved,
              hasNonDraftPr: "unresolvable",
              merged: "unresolvable",
            },
          }),
        },
        errors: [{ issueId: failedRef.issueId, message: "GitHub response omitted PR alias" }],
      })
    );
    const deps = resyncDeps({
      listNonTerminalIssues: () => [issueRef, failedRef],
      fetchPhaseArtifactsBatch,
      emitToController,
    });

    const result = await runResyncPass(deps);

    expect(fetchPhaseArtifactsBatch).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      recommendations: noOwnerRecommendation,
      errors: [{ issueId: failedRef.issueId, message: "GitHub response omitted PR alias" }],
    });
    expect(emitToController).toHaveBeenCalledWith(
      resyncEvent(noOwnerRecommendation, [
        { issueId: failedRef.issueId, message: "GitHub response omitted PR alias" },
      ])
    );
  });

  it("degrades a failed artifact batch into per-issue unresolvable errors", async () => {
    const secondRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };
    const deps = resyncDeps({
      listNonTerminalIssues: () => [issueRef, secondRef],
      fetchPhaseArtifactsBatch: async () => {
        throw new Error("GitHub unavailable");
      },
      emitToController: mock(() => {}),
    });

    const result = await runResyncPass(deps);

    expect(result.recommendations).toEqual([]);
    expect(result.errors).toEqual([
      { issueId: issueRef.issueId, message: "GitHub unavailable" },
      { issueId: secondRef.issueId, message: "GitHub unavailable" },
    ]);
  });

  it("does not wake the controller for an empty pass", async () => {
    // Given no non-terminal issues and no artifact errors.
    const emitToController = mock(() => {});
    const deps = resyncDeps({
      listNonTerminalIssues: () => [],
      fetchPhaseArtifactsBatch: async () => ({ artifacts: {}, errors: [] }),
      emitToController,
    });

    // When the resync pass finds no repair work.
    const result = await runResyncPass(deps);

    // Then it returns an empty result without waking the controller.
    expect(result).toEqual({ recommendations: [], errors: [] });
    expect(emitToController).not.toHaveBeenCalled();
  });

  it("wakes the controller when an errors-only pass degrades artifact resolution", async () => {
    // Given an artifact batch failure for an otherwise non-actionable pull request.
    const emitToController = mock(() => {});
    const deps = resyncDeps({
      fetchPhaseArtifactsBatch: async () => {
        throw new Error("GitHub unavailable");
      },
      emitToController,
    });

    // When the resync pass degrades the failed artifact read into an error.
    const result = await runResyncPass(deps);

    // Then it surfaces the error and still wakes the controller with no recommendations.
    expect(result).toEqual({
      recommendations: [],
      errors: [{ issueId: issueRef.issueId, message: "GitHub unavailable" }],
    });
    expect(emitToController).toHaveBeenCalledWith(
      resyncEvent([], [{ issueId: issueRef.issueId, message: "GitHub unavailable" }])
    );
  });

  it("emits recommendations but calls no injected write or worker-mutation methods", async () => {
    const writeStateFile = mock(() => {});
    const transitionIssue = mock(() => {});
    const removeLabel = mock(() => {});
    const createSession = mock(() => {});
    const deleteSession = mock(() => {});
    const emitToController = mock(() => {});

    const deps = {
      ...resyncDeps({ emitToController }),
      writeStateFile,
      transitionIssue,
      removeLabel,
      createSession,
      deleteSession,
    };

    await runResyncPass(deps);

    expect(emitToController).toHaveBeenCalledWith(resyncEvent(noOwnerRecommendation, []));
    expect(writeStateFile).not.toHaveBeenCalled();
    expect(transitionIssue).not.toHaveBeenCalled();
    expect(removeLabel).not.toHaveBeenCalled();
    expect(createSession).not.toHaveBeenCalled();
    expect(deleteSession).not.toHaveBeenCalled();
  });
});
