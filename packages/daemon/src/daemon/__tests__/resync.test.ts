import { describe, expect, it, mock } from "bun:test";
import { IssueStatus, WorkerMode } from "../../state/types";
import type { IssueRef, PhaseArtifactBatch, PhaseArtifacts } from "../phase-artifacts";
import { computeResyncRecommendations, type RunResyncDeps, runResyncPass } from "../resync";

const issueRef: IssueRef = {
  issueId: "acme-api-42",
  status: IssueStatus.IN_PROGRESS,
  source: { owner: "acme", repo: "api", number: 42 },
  prRef: { owner: "acme", repo: "api", number: 101 },
};

function artifacts(overrides: Partial<PhaseArtifacts> = {}): PhaseArtifacts {
  return {
    hasNonDraftPr: false,
    headSha: null,
    testerCheckOnHead: null,
    architectCheckOnHead: null,
    nativeReviewOnHead: null,
    autoMergeEnabledOrMerged: false,
    merged: false,
    planHandoff: "unresolvable",
    resolved: {
      hasNonDraftPr: "resolved",
      headSha: "unresolvable",
      testerCheckOnHead: "unresolvable",
      architectCheckOnHead: "unresolvable",
      nativeReviewOnHead: "unresolvable",
      autoMergeEnabledOrMerged: "unresolvable",
      merged: "resolved",
      planHandoff: "unresolvable",
    },
    ...overrides,
  };
}

describe("computeResyncRecommendations", () => {
  const cases = [
    {
      name: "recommends a tester when a ready PR has no tester verdict or live owner",
      input: {
        issueId: issueRef.issueId,
        status: IssueStatus.IN_PROGRESS,
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          resolved: {
            ...artifacts().resolved,
            headSha: "resolved",
            testerCheckOnHead: "resolved",
            architectCheckOnHead: "resolved",
            nativeReviewOnHead: "resolved",
            autoMergeEnabledOrMerged: "resolved",
          },
        }),
        hasLiveWorker: false,
      },
      expected: [
        {
          issueId: issueRef.issueId,
          mode: WorkerMode.TEST,
          reason: "artifact_no_live_owner" as const,
        },
      ],
    },
    {
      name: "does not recommend an owner that is already live",
      input: {
        issueId: issueRef.issueId,
        status: IssueStatus.IN_PROGRESS,
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          resolved: {
            ...artifacts().resolved,
            headSha: "resolved",
            testerCheckOnHead: "resolved",
            architectCheckOnHead: "resolved",
            nativeReviewOnHead: "resolved",
            autoMergeEnabledOrMerged: "resolved",
          },
        }),
        hasLiveWorker: true,
      },
      expected: [],
    },
    {
      name: "does not recommend work for terminal issues",
      input: {
        issueId: issueRef.issueId,
        status: IssueStatus.DONE,
        artifacts: artifacts(),
        hasLiveWorker: false,
      },
      expected: [],
    },
    {
      name: "does not recommend work while native auto-merge is waiting",
      input: {
        issueId: issueRef.issueId,
        status: IssueStatus.NEEDS_REVIEW,
        artifacts: artifacts({
          hasNonDraftPr: true,
          headSha: "head",
          testerCheckOnHead: "success",
          nativeReviewOnHead: "approved",
          architectCheckOnHead: "success",
          autoMergeEnabledOrMerged: true,
          resolved: {
            ...artifacts().resolved,
            headSha: "resolved",
            testerCheckOnHead: "resolved",
            architectCheckOnHead: "resolved",
            nativeReviewOnHead: "resolved",
            autoMergeEnabledOrMerged: "resolved",
          },
        }),
        hasLiveWorker: false,
      },
      expected: [],
    },
  ];

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(computeResyncRecommendations([testCase.input])).toEqual(testCase.expected);
    });
  }
});

describe("runResyncPass", () => {
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
    const deps: RunResyncDeps & {
      fetchPhaseArtifacts(ref: IssueRef): Promise<PhaseArtifacts>;
    } = {
      listNonTerminalIssues: () => [issueRef, failedRef],
      fetchPhaseArtifacts: async (ref) => {
        if (ref.issueId === failedRef.issueId) {
          throw new Error("GitHub response omitted PR alias");
        }
        return artifacts();
      },
      fetchPhaseArtifactsBatch,
      getLiveWorkers: async () => ({}),
      getSessionStatus: async () => ({ data: undefined }),
      emitToController,
    };

    const result = await runResyncPass(deps);

    expect(fetchPhaseArtifactsBatch).toHaveBeenCalledTimes(1);
    expect(result).toEqual({
      recommendations: [
        {
          issueId: issueRef.issueId,
          mode: WorkerMode.IMPLEMENT,
          reason: "artifact_no_live_owner",
        },
      ],
      errors: [{ issueId: failedRef.issueId, message: "GitHub response omitted PR alias" }],
    });
    expect(emitToController).toHaveBeenCalledWith([
      {
        issueId: issueRef.issueId,
        mode: WorkerMode.IMPLEMENT,
        reason: "artifact_no_live_owner",
      },
    ]);
  });

  it("degrades a failed artifact batch into per-issue unresolvable errors", async () => {
    const secondRef: IssueRef = {
      ...issueRef,
      issueId: "acme-api-43",
      prRef: { owner: "acme", repo: "api", number: 102 },
    };
    const deps: RunResyncDeps & {
      fetchPhaseArtifacts(ref: IssueRef): Promise<PhaseArtifacts>;
    } = {
      listNonTerminalIssues: () => [issueRef, secondRef],
      fetchPhaseArtifacts: async () => {
        throw new Error("GitHub unavailable");
      },
      fetchPhaseArtifactsBatch: async () => {
        throw new Error("GitHub unavailable");
      },
      getLiveWorkers: async () => ({}),
      getSessionStatus: async () => ({ data: undefined }),
      emitToController: mock(() => {}),
    };

    const result = await runResyncPass(deps);

    expect(result.recommendations).toEqual([]);
    expect(result.errors).toEqual([
      { issueId: issueRef.issueId, message: "GitHub unavailable" },
      { issueId: secondRef.issueId, message: "GitHub unavailable" },
    ]);
  });

  it("emits recommendations but calls no injected write or worker-mutation methods", async () => {
    const writeStateFile = mock(() => {});
    const transitionIssue = mock(() => {});
    const removeLabel = mock(() => {});
    const createSession = mock(() => {});
    const deleteSession = mock(() => {});
    const emitToController = mock(() => {});

    const deps: RunResyncDeps & {
      writeStateFile: typeof writeStateFile;
      transitionIssue: typeof transitionIssue;
      removeLabel: typeof removeLabel;
      createSession: typeof createSession;
      deleteSession: typeof deleteSession;
    } = {
      listNonTerminalIssues: () => [issueRef],
      fetchPhaseArtifactsBatch: async () => ({
        artifacts: { [issueRef.issueId]: artifacts() },
        errors: [],
      }),
      getLiveWorkers: async () => ({}),
      getSessionStatus: async () => ({ data: undefined }),
      emitToController,
      writeStateFile,
      transitionIssue,
      removeLabel,
      createSession,
      deleteSession,
    };

    await runResyncPass(deps);

    expect(emitToController).toHaveBeenCalledWith([
      {
        issueId: issueRef.issueId,
        mode: WorkerMode.IMPLEMENT,
        reason: "artifact_no_live_owner",
      },
    ]);
    expect(writeStateFile).not.toHaveBeenCalled();
    expect(transitionIssue).not.toHaveBeenCalled();
    expect(removeLabel).not.toHaveBeenCalled();
    expect(createSession).not.toHaveBeenCalled();
    expect(deleteSession).not.toHaveBeenCalled();
  });
});
