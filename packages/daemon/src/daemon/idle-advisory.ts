import type { WorkerModeLiteral } from "../state/types";
import type { PhaseArtifacts } from "./phase-artifacts";

type ResolvableArtifactDescriptor =
  | {
      readonly mode: "implement";
      readonly kind: "artifact";
      readonly resolution: "hasNonDraftPr";
      readonly artifact: "hasNonDraftPr";
      readonly presence: "boolean_true";
    }
  | {
      readonly mode: "test";
      readonly kind: "artifact";
      readonly resolution: "testerCheckOnHead";
      readonly artifact: "testerCheckOnHead";
      readonly presence: "non_null";
    }
  | {
      readonly mode: "review";
      readonly kind: "artifact";
      readonly resolution: "nativeReviewOnHead";
      readonly artifact: "nativeReviewOnHead";
      readonly presence: "non_null";
    }
  | {
      readonly mode: "architect";
      readonly kind: "artifact";
      readonly resolution: "architectCheckOnHead";
      readonly artifact: "architectCheckOnHead";
      readonly presence: "non_null";
    }
  | {
      readonly mode: "merge";
      readonly kind: "artifact";
      readonly resolution: "autoMergeEnabledOrMerged";
      readonly artifact: "autoMergeEnabledOrMerged";
      readonly presence: "boolean_true";
    };

interface UnresolvableArtifactDescriptor {
  readonly mode: "plan";
  readonly kind: "unresolvable";
}

type ExpectedArtifactDescriptor = ResolvableArtifactDescriptor | UnresolvableArtifactDescriptor;

export const EXPECTED_ARTIFACT_BY_MODE = {
  architect: {
    mode: "architect",
    kind: "artifact",
    resolution: "architectCheckOnHead",
    artifact: "architectCheckOnHead",
    presence: "non_null",
  },
  plan: { mode: "plan", kind: "unresolvable" },
  implement: {
    mode: "implement",
    kind: "artifact",
    resolution: "hasNonDraftPr",
    artifact: "hasNonDraftPr",
    presence: "boolean_true",
  },
  test: {
    mode: "test",
    kind: "artifact",
    resolution: "testerCheckOnHead",
    artifact: "testerCheckOnHead",
    presence: "non_null",
  },
  review: {
    mode: "review",
    kind: "artifact",
    resolution: "nativeReviewOnHead",
    artifact: "nativeReviewOnHead",
    presence: "non_null",
  },
  merge: {
    mode: "merge",
    kind: "artifact",
    resolution: "autoMergeEnabledOrMerged",
    artifact: "autoMergeEnabledOrMerged",
    presence: "boolean_true",
  },
} as const satisfies Readonly<Record<WorkerModeLiteral, ExpectedArtifactDescriptor>>;

export interface IdleAdvisoryInput {
  readonly issueId: string;
  readonly mode: string;
  readonly sessionStatusType: string;
  readonly artifacts: PhaseArtifacts;
}

export interface IdleAdvisory {
  readonly issueId: string;
  readonly mode: WorkerModeLiteral;
  readonly reason: "idle_missing_phase_artifact";
}

function assertNever(value: never): never {
  throw new Error(`Unhandled expected artifact descriptor: ${JSON.stringify(value)}`);
}

function isExpectedWorkerMode(mode: string): mode is WorkerModeLiteral {
  return Object.hasOwn(EXPECTED_ARTIFACT_BY_MODE, mode);
}

function artifactExists(
  artifacts: PhaseArtifacts,
  descriptor: ResolvableArtifactDescriptor
): boolean {
  if (artifacts.resolved[descriptor.resolution] !== "resolved") return true;

  switch (descriptor.presence) {
    case "boolean_true":
      return artifacts[descriptor.artifact] === true;
    case "non_null":
      return artifacts[descriptor.artifact] !== null;
    default:
      return assertNever(descriptor);
  }
}

function advisoryFor(input: IdleAdvisoryInput): IdleAdvisory | null {
  if (input.sessionStatusType !== "idle" || !isExpectedWorkerMode(input.mode)) return null;

  const descriptor = EXPECTED_ARTIFACT_BY_MODE[input.mode];
  switch (descriptor.kind) {
    case "unresolvable":
      return null;
    case "artifact":
      return artifactExists(input.artifacts, descriptor)
        ? null
        : {
            issueId: input.issueId,
            mode: descriptor.mode,
            reason: "idle_missing_phase_artifact",
          };
    default:
      return assertNever(descriptor);
  }
}

export function computeIdleAdvisories(
  inputs: readonly IdleAdvisoryInput[]
): readonly IdleAdvisory[] {
  const advisories: IdleAdvisory[] = [];
  for (const input of inputs) {
    const advisory = advisoryFor(input);
    if (advisory) advisories.push(advisory);
  }
  return advisories;
}
