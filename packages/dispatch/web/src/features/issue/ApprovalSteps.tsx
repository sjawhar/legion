import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { approvalPillClassName } from "../../components/Pill";
import { textMutedOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { AgentName } from "../refs/AgentName";
import type { StepEntry } from "./gated-steps";

/**
 * Today's header condition for the review actions, applied to the one entry that holds the
 * issue's primary document: the actions sit where they have always sat, beside that document's
 * state, and never once per step. A draft document carries them only while an approval request
 * is open - that is the header's rule, kept whole, though `ApprovalChip` draws nothing at all
 * for a draft and so draws no actions either.
 */
function showsActions(
  document: Artifact,
  primaryArtifactId: string,
  approvalRequestOpen: boolean
): boolean {
  const { approval } = document;
  return (
    document.id === primaryArtifactId &&
    approval !== undefined &&
    (approval.state !== "draft" || approvalRequestOpen)
  );
}

/** The state of one step: its document's chip, or the dashed pill that says nobody has asked for
 *  this document to be reviewed - a step with no document, a document with no approval, and a
 *  draft all read the same to someone waiting on the gate, and none of the three has a review to
 *  open, so the pill is a span rather than a button. */
function StepState({
  approvalRequestOpen,
  entry,
  primaryArtifactId,
}: {
  approvalRequestOpen: boolean;
  entry: StepEntry;
  primaryArtifactId: string;
}): ReactNode {
  const { document } = entry;
  if (document?.approval === undefined || document.approval.state === "draft") {
    return <span className={approvalPillClassName("draft")}>Not requested</span>;
  }
  return (
    <ApprovalChip
      artifact={document}
      layout="contents"
      showActions={showsActions(document, primaryArtifactId, approvalRequestOpen)}
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
  const version = approval.version ?? approval.latest_version;
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
  approvalRequestOpen,
  entries,
  primaryArtifactId,
}: {
  /** Whether the issue has an open approval ask, which is what lets a draft document still carry
   *  the actions. */
  approvalRequestOpen: boolean;
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
            <StepState
              approvalRequestOpen={approvalRequestOpen}
              entry={entry}
              primaryArtifactId={primaryArtifactId}
            />
          </li>
        ))}
      </ul>
      {entries.map((entry) => (
        <StepReason entry={entry} key={entry.step} />
      ))}
    </div>
  );
}
