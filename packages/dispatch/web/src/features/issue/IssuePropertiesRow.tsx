import type { ReactNode } from "react";

import type { Artifact, IssueDetails } from "../../api/types";
import {
  bgTransparent,
  borderDefault,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  statusPill,
  statusPillDot,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { AssigneeControl } from "./AssigneeControl";
import { IssueAddMenu } from "./IssueAddMenu";
import type { IssueField } from "./IssueFieldForms";
import { IssueLabels } from "./IssueLabels";
import { PriorityEditor } from "./PriorityControl";
import { ProgressChips } from "./ProgressChips";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * Row three of the identity block: how the issue stands and who it is for - the status, the
 * priority, the assignee and its labels - with the one add control for a property it has no
 * value for, and the waiting marker at the right.
 *
 * The row wraps rather than scrolling: every control is visible at 390 px as a second line, and
 * nothing here is reached by dragging a row sideways (R11, R17).
 */
export function IssuePropertiesRow({
  documentArtifact,
  isClosed,
  issue,
  onOpenField,
  priorityWrite,
  writes,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
  priorityWrite: IssuePriorityWrite;
  writes: IssueHeaderWrites;
}): ReactNode {
  const saving = writes.updateIssue.isPending;
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  const openAsks = issue.open_asks.filter((ask) => ask.state === "open");
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the marker stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const waitingForHuman = waitingOnHuman(openAsks);
  const whoseTurn =
    openAsks.length === 0 || !knowsWhoseTurn
      ? null
      : waitingForHuman.length > 0
        ? `${PROPERTY_LABELS.waiting_on_you} (${waitingForHuman.length})`
        : `${PROPERTY_LABELS.waiting_on_agents} (${openAsks.length - waitingForHuman.length})`;
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  const showApprovalActions =
    documentArtifact?.approval !== undefined &&
    (documentArtifact.approval.state !== "draft" || approvalRequestOpen);

  return (
    <div
      className="flex w-full min-w-0 flex-wrap items-center gap-2"
      data-testid="issue-state-actions"
    >
      <label
        className={`flex min-h-11 shrink-0 items-center gap-2 rounded-full border px-2 text-sm font-semibold uppercase tracking-wide sm:px-3 md:min-h-8 xl:px-2 ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
      >
        <span
          aria-hidden="true"
          className={`hidden h-2 w-2 rounded-full sm:block ${statusPillDot}`}
        />
        <select
          aria-label={PROPERTY_LABELS.status}
          className={`min-w-0 appearance-none font-inherit outline-none disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${statusPill.text}`}
          disabled={isClosed || saving}
          onChange={(event) => writes.drafts.requestStatusSubmit(event.target.value)}
          value={writes.pendingStatus ?? issue.status}
        >
          {selectableStatuses.map((status) => (
            <option key={status} value={status}>
              {statusLabel(status)}
            </option>
          ))}
        </select>
      </label>
      <PriorityEditor
        disabled={isClosed || saving}
        issueKey={issue.key}
        priority={issue.priority}
        write={priorityWrite}
      />
      <AssigneeControl
        assignee={issue.assignee}
        disabled={isClosed || saving}
        issueKey={issue.key}
      />
      <IssueLabels
        disabled={isClosed}
        labels={issue.labels}
        onSave={(labels) => writes.labels.mutateAsync(labels)}
        project={issue.project}
        saving={writes.labels.isPending}
        variant="rail"
      />
      <IssueAddMenu isClosed={isClosed} issue={issue} onOpenField={onOpenField} />
      {/* How far the work has got, like the status: the children section draws the same counts
          as a bar, and the task count stays here. */}
      <ProgressChips bar progress={issue.progress} />
      {documentArtifact === undefined ? null : (
        <ApprovalChip
          artifact={documentArtifact}
          layout="contents"
          showActions={showApprovalActions}
          variant="header"
        />
      )}
      {whoseTurn === null ? null : (
        <span
          className={`ml-auto inline-flex shrink-0 items-center rounded-full border px-3 py-1 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
          data-testid="issue-whose-turn"
          {...pageChrome}
        >
          {whoseTurn}
        </span>
      )}
    </div>
  );
}
