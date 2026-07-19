import { describe, expect, it, mock } from "bun:test";
import { WorkerMode } from "../../state/types";
import {
  computeIdleAdvisories,
  EXPECTED_ARTIFACT_BY_MODE,
  type IdleAdvisory,
  type IdleAdvisoryInput,
} from "../idle-advisory";
import type { PhaseArtifactResolutions, PhaseArtifacts } from "../phase-artifacts";

interface ArtifactOverrides extends Partial<Omit<PhaseArtifacts, "resolved">> {
  readonly resolved?: Partial<PhaseArtifactResolutions>;
}

const baseResolutions: PhaseArtifactResolutions = {
  hasNonDraftPr: "resolved",
  headSha: "resolved",
  testerCheckOnHead: "resolved",
  architectCheckOnHead: "resolved",
  nativeReviewOnHead: "resolved",
  autoMergeEnabledOrMerged: "resolved",
  merged: "resolved",
  planHandoff: "unresolvable",
};

function phaseArtifacts(overrides: ArtifactOverrides = {}): PhaseArtifacts {
  const { resolved: resolutionOverrides, ...artifactOverrides } = overrides;
  return {
    hasNonDraftPr: false,
    headSha: "current-head",
    testerCheckOnHead: null,
    architectCheckOnHead: null,
    nativeReviewOnHead: null,
    autoMergeEnabledOrMerged: false,
    merged: false,
    planHandoff: "unresolvable",
    ...artifactOverrides,
    resolved: { ...baseResolutions, ...resolutionOverrides },
  };
}

function worker(
  mode: string,
  artifacts: PhaseArtifacts = phaseArtifacts(),
  sessionStatusType = "idle"
): IdleAdvisoryInput {
  return {
    issueId: "acme-api-42",
    mode,
    sessionStatusType,
    artifacts,
  };
}

function advisory(mode: IdleAdvisory["mode"]): IdleAdvisory {
  return {
    issueId: "acme-api-42",
    mode,
    reason: "idle_missing_phase_artifact",
  };
}

describe("EXPECTED_ARTIFACT_BY_MODE", () => {
  it("encodes the planner artifact as permanently unresolvable", () => {
    expect(EXPECTED_ARTIFACT_BY_MODE[WorkerMode.PLAN]).toEqual({
      mode: "plan",
      kind: "unresolvable",
    });
  });
});

describe("computeIdleAdvisories", () => {
  it("returns one advisory when an idle implementer has no non-draft pull request", () => {
    const advisories = computeIdleAdvisories([worker(WorkerMode.IMPLEMENT)]);

    expect(advisories).toEqual([advisory(WorkerMode.IMPLEMENT)]);
  });

  it("returns no advisory when an idle implementer has a non-draft pull request", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.IMPLEMENT, phaseArtifacts({ hasNonDraftPr: true })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory when an idle implementer is waiting for a tester verdict", () => {
    const advisories = computeIdleAdvisories([
      worker(
        WorkerMode.IMPLEMENT,
        phaseArtifacts({ hasNonDraftPr: true, testerCheckOnHead: null })
      ),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory when an idle tester has a tester check on the current head", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.TEST, phaseArtifacts({ testerCheckOnHead: "success" })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns one advisory when an idle tester has no tester check on the current head", () => {
    const advisories = computeIdleAdvisories([worker(WorkerMode.TEST)]);

    expect(advisories).toEqual([advisory(WorkerMode.TEST)]);
  });

  it("returns no advisory when an idle reviewer has a native review on the current head", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.REVIEW, phaseArtifacts({ nativeReviewOnHead: "approved" })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns one advisory when an idle reviewer has no native review on the current head", () => {
    const advisories = computeIdleAdvisories([worker(WorkerMode.REVIEW)]);

    expect(advisories).toEqual([advisory(WorkerMode.REVIEW)]);
  });

  it("returns no advisory when an idle architect has an architect check on the current head", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.ARCHITECT, phaseArtifacts({ architectCheckOnHead: "success" })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns one advisory when an idle architect has no architect check on the current head", () => {
    const advisories = computeIdleAdvisories([worker(WorkerMode.ARCHITECT)]);

    expect(advisories).toEqual([advisory(WorkerMode.ARCHITECT)]);
  });

  it("never returns an advisory for an idle planner", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.PLAN, phaseArtifacts({ planHandoff: "absent" })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory when an idle merger has auto-merge enabled", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.MERGE, phaseArtifacts({ autoMergeEnabledOrMerged: true })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory when an idle merger has a merged pull request", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.MERGE, phaseArtifacts({ autoMergeEnabledOrMerged: true, merged: true })),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns one advisory when an idle merger has neither auto-merge nor a merged pull request", () => {
    const advisories = computeIdleAdvisories([worker(WorkerMode.MERGE)]);

    expect(advisories).toEqual([advisory(WorkerMode.MERGE)]);
  });

  it("returns no advisory when the worker is busy", () => {
    const advisories = computeIdleAdvisories([
      worker(WorkerMode.IMPLEMENT, phaseArtifacts(), "busy"),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory when a required artifact is unresolvable", () => {
    const advisories = computeIdleAdvisories([
      worker(
        WorkerMode.TEST,
        phaseArtifacts({
          resolved: { testerCheckOnHead: "unresolvable" },
        })
      ),
    ]);

    expect(advisories).toEqual([]);
  });

  it("returns no advisory for an unknown worker mode", () => {
    const advisories = computeIdleAdvisories([worker("unsupported-mode")]);

    expect(advisories).toEqual([]);
  });

  it("does not invoke abort, deletion, or re-seat methods exposed by a caller", () => {
    const abortSession = mock(() => {});
    const deleteSession = mock(() => {});
    const reseatWorker = mock(() => {});
    const workerWithForbiddenMethods = {
      ...worker(WorkerMode.IMPLEMENT),
      abortSession,
      deleteSession,
      reseatWorker,
    };

    const advisories = computeIdleAdvisories([workerWithForbiddenMethods]);

    expect(advisories).toEqual([advisory(WorkerMode.IMPLEMENT)]);
    expect(abortSession).not.toHaveBeenCalled();
    expect(deleteSession).not.toHaveBeenCalled();
    expect(reseatWorker).not.toHaveBeenCalled();
  });
});
