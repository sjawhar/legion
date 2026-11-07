import type { ReactNode } from "react";

import { approvalPillClassName } from "../../components/Pill";
import { textMutedOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { ApprovalChip, approvalVersion } from "../doc/ApprovalChip";
import { AgentName } from "../refs/AgentName";
import type { StepEntry } from "./gated-steps";

/** The state of one step: its document's chip, or the dashed pill that says nobody has asked for
 *  this document to be reviewed - a step with no document, a document with no approval, and a
 *  draft all read the same to someone waiting on the gate, and none of the three has a review to
 *  open, so the pill is a span rather than a button. */
function StepState({
  entry,
  primaryArtifactId,
}: {
  entry: StepEntry;
  primaryArtifactId: string;
}): ReactNode {
  const { document } = entry;
  if (document?.approval === undefined || document.approval.state === "draft") {
    return <span className={approvalPillClassName("draft")}>Not requested</span>;
  }
  return (
    // The chip is reached only for a document with an approval that is not a draft - the
    // header's own condition, kept whole - so the only thing left to decide here is whether
    // this is the issue's primary document, where the actions have always sat.
    <ApprovalChip
      artifact={document}
      layout="contents"
      showActions={document.id === primaryArtifactId}
      variant="header"
    />
  );
}

/** Why a step's reviewer sent its document back, as one wrapping line: the chip says only
 *  `Changes requested`, and the reason is the whole of what the writer must act on. It is given
 *  no width, no clamp and no tooltip, so a phone reads all of it. */
function StepReason({ entry }: { entry: StepEntry }): ReactNode {
  const approval = entry.document?.approval;
  if (approval === undefined || approval.state !== "changes_requested") {
    return null;
  }
  const version = approvalVersion(approval);
  const reason = approval.reason ?? null;
  return (
    <p
      className={`min-w-0 text-xs [overflow-wrap:anywhere] ${textSecondaryOnSurface}`}
      data-approval-step-reason={entry.step}
    >
      {`${entry.label}: `}
      {approval.by === undefined ? (
        `Changes requested on v${version}`
      ) : (
        <>
          <AgentName actor={approval.by} inline />
          {` requested changes on v${version}`}
        </>
      )}
      {reason === null ? null : ` · ${reason}`}
    </p>
  );
}

/**
 * The design gate's steps in a root issue's header: one line per gated step, in the order the
 * deployment publishes them, each naming the document that step reviews and where that document
 * stands. It replaces the header's one chip, which spoke for the primary document alone and left
 * a reader guessing which of three documents the gate was waiting on.
 *
 * The caller decides which steps there are (`gatedStepEntries`); this draws them.
 */
export function ApprovalSteps({
  entries,
  primaryArtifactId,
}: {
  entries: StepEntry[];
  primaryArtifactId: string;
}): ReactNode {
  return (
    // The list takes the header row's full width and wraps within itself: three steps, their
    // chips and the two actions never fit a phone's line, and a row that cannot wrap scrolls
    // the page sideways.
    <div className="flex w-full min-w-0 flex-col gap-2">
      <ul
        aria-label="Approvals"
        className="flex w-full min-w-0 flex-wrap items-center gap-x-4 gap-y-2"
        data-testid="issue-approval-steps"
      >
        {entries.map((entry) => (
          <li
            className="flex min-w-0 flex-wrap items-center gap-2"
            data-approval-step={entry.step}
            key={entry.step}
          >
            <span className={`text-xs font-medium ${textMutedOnSurface}`}>{entry.label}</span>
            <StepState entry={entry} primaryArtifactId={primaryArtifactId} />
          </li>
        ))}
      </ul>
      {entries.map((entry) => (
        <StepReason entry={entry} key={entry.step} />
      ))}
    </div>
  );
}
