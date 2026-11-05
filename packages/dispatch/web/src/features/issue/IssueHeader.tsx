import { type ReactNode, useRef, useState } from "react";

import type { Artifact, IssueDetails, UserIssueState } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  textMutedOnSurface,
} from "../../theme/classes";
import type { IssueAction } from "./IssueActionsMenu";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { IssueIdentityRow } from "./IssueIdentityRow";
import { IssuePropertiesRow } from "./IssuePropertiesRow";
import { IssueTitleRow } from "./IssueTitleRow";
import { IssueWorkRow } from "./IssueWorkRow";
import { useIdentityBlock } from "./identity-block";
import { HELP_LINES, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { statusPickOnly, useIssueHeaderWrites } from "./useIssueHeaderWrites";
import { useIssuePalette } from "./useIssuePalette";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * The identity block: the issue, as a reader has to see it before anything else
 * (dispatch://INFRA-1196 R11), in four rows that are all visible with no control opened.
 *
 * 1. Where the issue sits and what a reader does to it: the parent as a breadcrumb, the key, the
 *    copy control, the pin, and one actions control holding close, reopen and setting the parent.
 * 2. The title.
 * 3. How it stands: the status, the priority, the assignee, the labels, the one add control for
 *    a property with no value, and the waiting marker.
 * 4. Where its messages go, always, and who holds it while a claim is on it.
 *
 * Every other property is in the panel's sections (`IssueMargin.tsx`): the repository, the
 * components, the links out, who opened it, its subscribers and what refers to it. No property
 * is behind a control named "more", nothing here scrolls sideways at any width, and the two
 * tooltips that used to carry meaning - what a route is, and who opened the issue - are drawn as
 * text (R11, R21).
 *
 * The block is sticky in a condensed form (`identity-block.ts`), which also publishes its own
 * height, so the tree's focused row can clear it rather than guess at it.
 *
 * `data-issue-header` is load-bearing for the keyboard: the `issue` scope's bindings find the
 * Status, priority, labels, title and pin controls through it, and scoping to it is what keeps a
 * child issue's row - which carries the same accessible names - out of their reach.
 */
export function IssueHeader({
  documentArtifact,
  isClosed,
  issue,
  priorityWrite,
  restoringTo,
  state,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  /** The page's one priority write, shared with the keyboard's `0`–`3` and the palette's Set
   *  priority rows, so a refusal of any of them is reported once and by this header. */
  priorityWrite: IssuePriorityWrite;
  /** Where the page is about to put the reader back, in pixels down the document, while it is
   *  returning them to a tab they left: the block's form is decided for that place, not for
   *  where the page stands now (`identity-block.ts`). */
  restoringTo?: number;
  state: UserIssueState;
}): ReactNode {
  const writes = useIssueHeaderWrites(issue, state);
  const { drafts, pendingStatus, updateIssue } = writes;
  const [editingTitle, setEditingTitle] = useState(false);
  // One editor at a time: the three fields a reader types rather than picks share the row under
  // the block, so opening one closes the one before it.
  const [editingField, setEditingField] = useState<IssueField | null>(null);
  const block = useRef<HTMLElement | null>(null);
  const { condensed } = useIdentityBlock(block, restoringTo);
  useIssuePalette({
    drafts,
    isClosed,
    priorityWrite,
    saving: updateIssue.isPending,
  });

  const actions: IssueAction[] = [
    isClosed
      ? {
          disabled: updateIssue.isPending,
          name: CONTROL_LABELS.reopen_issue,
          run: () => drafts.requestStatusSubmit("backlog"),
          text:
            pendingStatus === "backlog"
              ? CONTROL_LABELS.saving_reopen
              : CONTROL_LABELS.reopen_into_backlog,
        }
      : {
          disabled: updateIssue.isPending,
          name: CONTROL_LABELS.close_issue,
          run: () => drafts.requestStatusSubmit("done"),
          text: pendingStatus === "done" ? CONTROL_LABELS.saving_close : CONTROL_LABELS.close_issue,
        },
    {
      disabled: isClosed,
      name: issue.parent === null ? CONTROL_LABELS.set_parent : CONTROL_LABELS.edit_parent,
      run: () => setEditingField("parent"),
      text: issue.parent === null ? CONTROL_LABELS.set_parent : CONTROL_LABELS.edit_parent,
    },
  ];

  return (
    <header
      className={`sticky top-0 z-20 mb-3 flex min-w-0 flex-col rounded-xl border ${
        condensed ? "gap-1 p-2" : "gap-2 p-3"
      } ${card}`}
      data-condensed={condensed ? "true" : "false"}
      data-issue-header=""
      data-testid="issue-header"
      ref={block}
    >
      {isClosed ? (
        <p
          className={`rounded-lg px-3 py-1 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
          {...pageChrome}
        >
          {HELP_LINES.closed}
        </p>
      ) : null}
      <IssueIdentityRow
        actions={actions}
        issueKey={issue.key}
        onPin={() => writes.pinGuard.guard(() => writes.pin.mutate(!state.pinned))}
        parent={issue.parent}
        pinSaving={writes.pin.isPending}
        pinned={state.pinned}
      />
      <IssueTitleRow
        condensed={condensed}
        drafts={drafts}
        editing={editingTitle}
        isClosed={isClosed}
        onEditingChange={setEditingTitle}
      />
      <IssuePropertiesRow
        documentArtifact={documentArtifact}
        isClosed={isClosed}
        issue={issue}
        onOpenField={setEditingField}
        priorityWrite={priorityWrite}
        writes={writes}
      />
      <IssueWorkRow
        isClosed={isClosed}
        issue={issue}
        onOpenField={setEditingField}
        writes={writes}
      />
      <IssueFieldForms
        field={editingField}
        isClosed={isClosed}
        issue={issue}
        onClose={() => setEditingField(null)}
        writes={writes}
      />
      {drafts.titleError === null ? null : (
        <p className={`text-sm ${dangerText}`} id="issue-title-help">
          {drafts.titleError}
        </p>
      )}
      {writes.statusSaving ? (
        <span className={`block text-xs ${textMutedOnSurface}`} role="status" {...pageChrome}>
          {pendingStatus === "done"
            ? CONTROL_LABELS.saving_close
            : pendingStatus === "backlog" && isClosed
              ? CONTROL_LABELS.saving_reopen
              : CONTROL_LABELS.saving_field}
        </span>
      ) : null}
      {writes.pin.isError ? (
        <QueryError
          message="Could not save pin status."
          onRetry={() => writes.pinGuard.retryLast(writes.pin)}
          retrying={writes.pin.isPending}
        />
      ) : null}
      {writes.releaseClaim.isError ? (
        <QueryError
          message="Could not release this claim."
          onRetry={() => writes.releaseClaim.mutate()}
          retrying={writes.releaseClaim.isPending}
        />
      ) : null}
      {updateIssue.isError && !statusPickOnly(updateIssue.variables) ? (
        writes.parentSaveFailed ? (
          <p className={`text-sm ${dangerText}`} id="issue-parent-error">
            {writes.parentError}
          </p>
        ) : writes.titleRefusal !== null ? (
          <p className={`text-sm ${dangerText}`} id="issue-title-error" role="alert">
            {writes.titleRefusal}
          </p>
        ) : (
          <QueryError
            message="Could not update this issue."
            onRetry={drafts.retry}
            retrying={updateIssue.isPending}
          />
        )
      ) : null}
    </header>
  );
}
