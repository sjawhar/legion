import { GATED_STEPS } from "@legion/contracts";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import type { Artifact, GatedStep, IssueDetails, Project } from "../../api/types";

/** What the page calls the document each design-gate step reviews: Dispatch's own name for that
 *  document, so a reader is told which document waits rather than the enumeration's word for the
 *  step (`criteria` names the Verification document). */
export const STEP_LABELS: Record<GatedStep, string> = {
  decisions: "Decisions",
  criteria: "Verification",
  spec: "Spec",
};

/** One design-gate step of a root issue: the step, the name the page draws for it, and the
 *  document it reviews - undefined while the details are unread and for an issue that holds no
 *  document for the step, which is the step nobody has written yet. */
export interface StepEntry {
  step: GatedStep;
  label: string;
  document: Artifact | undefined;
}

/** The document the issue holds for `step`, by the id the issue names for it, never by the
 *  document's role or name among `issue.artifacts`. */
function stepDocument(issue: IssueDetails, step: GatedStep): Artifact | undefined {
  switch (step) {
    case "spec":
      return primarySpec(issue);
    case "criteria":
      return roleDocument(issue, "verification");
    case "decisions":
      return roleDocument(issue, "decisions");
  }
}

/**
 * The design-gate steps the issue's header draws, one for each step the project's deployment
 * gates, in the order the deployment published them. It is empty - which leaves the header its
 * one chip for the issue's primary document - until the project and the issue are both read, for
 * a project whose deployment gates nothing, and for an issue that is not its tree's root, since
 * only a root issue stands in the gate.
 *
 * A step word this build does not know is left out rather than drawn without a name: a Dispatch
 * newer than this build may gate a fourth step, and the header draws the steps it knows.
 */
export function gatedStepEntries(
  project: Project | undefined,
  issue: IssueDetails | undefined
): StepEntry[] {
  const steps = project?.gated_steps;
  if (steps === undefined || steps.length === 0 || issue === undefined || issue.parent !== null) {
    return [];
  }
  return steps
    .filter((step) => GATED_STEPS.includes(step))
    .map((step) => ({ step, label: STEP_LABELS[step], document: stepDocument(issue, step) }));
}
