import { type ReactNode, useCallback, useState } from "react";
import { Link } from "react-router-dom";

import type { Artifact, InboxRow } from "../../api/types";
import { linkHoverText, linkText, secondaryButtonCompact } from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { PriorityControl } from "../issue/PriorityControl";
import { blockFragment } from "../refs/block-reference";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, buildProjectPath, itemRoute } from "../refs/routes";
import { InboxOwnerLink } from "./AskOwnerLink";
import { AssignToMe } from "./AssignToMe";
import { askIssueKey, controlName } from "./ask-name";
import { EarlierApproval } from "./EarlierApproval";
import { SnoozeControl } from "./SnoozeControl";
import { isUnassigned } from "./sections";
import { isSnoozed } from "./snooze";

/** The page "Open page" opens: the question's issue, or — for a project document, which has no
 *  issue page — the document itself at the section the column is scrolled to. The section is the
 *  heading the pane found; before the document has drawn, the question's own block, and for an
 *  anchored question the block its quote sits in. */
function openPagePath(ask: InboxRow, sectionBlockId: string | undefined): string | undefined {
  const issueKey = askIssueKey(ask);
  if (issueKey !== null) {
    return buildIssuePath({ key: issueKey, kind: "issue" });
  }
  if (ask.document === undefined) {
    return undefined;
  }
  const document = buildProjectPath({
    kind: "document",
    project: ask.document.project,
    slug: ask.document.slug,
  });
  const blockId = sectionBlockId ?? ask.block_id ?? ask.anchor?.block_id;
  return blockId === undefined || blockId === null
    ? document
    : `${document}${blockFragment(blockId)}`;
}

export interface ReadingPaneHeadProps {
  /** The document the column shows, once it is read; undefined for a question in no document and
   *  while the read is out. The approval actions are this document's. */
  artifact: Artifact | undefined;
  ask: InboxRow;
  /** The reader's own answer or review landed. */
  onAnswered: (askId: string) => void;
  /** Moves the column to the next block marked as changed; undefined when nothing is marked. */
  onNextChange: (() => void) | undefined;
  /** Moves the column to the decision block; undefined when the question is not one. */
  onSkipToAnswer: (() => void) | undefined;
  /** Registers the question while this head's Snooze write is in flight or failed, exactly as
   *  a row's own control registers its own: the Inbox keeps a live row listed, so the control
   *  holding `Snoozing…`, a refusal and its Retry is not unmounted by the fold the optimistic
   *  snooze causes - the column follows that row, so the fold would take this head with it. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** The heading of the section the column scrolled to, once the document has drawn. */
  sectionBlockId: string | undefined;
  viewer: string | undefined;
}

/**
 * The reading column's head: what the selected question is and the controls that act on it.
 *
 * An issue question carries the issue key and title, "Open page", the issue's priority, "Assign
 * to me" while nobody holds the issue, Snooze and Copy ref. A question in a project document
 * carries the project key and the document name, "Open page" at the section, Snooze and Copy ref
 * — a project document has no assignee and no priority, so neither control applies to it.
 *
 * With the column on, `h`, `o` and `y` look inside the column rather than inside the selected
 * row (`hand()` returns the column in `Inbox.tsx`), and the short row beside it draws no Snooze
 * and no copy-ref control at all. So this head draws the same three controls the card row draws
 * — `[data-inbox-snooze]`, `[data-inbox-owner]` and the copy-ref control — and the keys find
 * them here.
 */
export function ReadingPaneHead({
  artifact,
  ask,
  onAnswered,
  onNextChange,
  onSkipToAnswer,
  onSnoozeLive,
  sectionBlockId,
  viewer,
}: ReadingPaneHeadProps): ReactNode {
  // The Inbox row keeps "Assign to me" mounted while its write is in flight or failed, so
  // `Assigning…` and a refusal are shown by the instance that wrote rather than lost to the
  // optimistic update that takes the issue out of `isUnassigned`. The head does the same.
  const [assignLive, setAssignLive] = useState(false);
  const onAssignLive = useCallback((_askId: string, live: boolean) => setAssignLive(live), []);
  const name = controlName(ask, undefined);
  const page = openPagePath(ask, sectionBlockId);
  const reference = itemRoute("ask", ask, ask.document);
  const approval = ask.kind === "approval" ? ask.approval : undefined;
  // The chip draws Approve only while the document is reviewable and not already approved; where
  // it draws none, the head says which approval this request follows instead.
  const approvalState = artifact?.approval?.state;
  const approveDrawn =
    approvalState !== undefined && approvalState !== "draft" && approvalState !== "approved";

  return (
    <header className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <InboxOwnerLink ask={ask} rowOwner />
      {page === undefined ? null : (
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to={page}>
          Open page
        </Link>
      )}
      {ask.issue_key === null ? null : (
        <PriorityControl issueKey={ask.issue_key} priority={ask.priority} />
      )}
      {ask.issue_key !== null && viewer !== undefined && (isUnassigned(ask) || assignLive) ? (
        <AssignToMe askId={ask.id} issueKey={ask.issue_key} onLive={onAssignLive} viewer={viewer} />
      ) : null}
      <SnoozeControl
        askId={ask.id}
        label={name}
        onLive={onSnoozeLive}
        snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
      />
      {reference === undefined ? null : <CopyRefButton route={reference} />}
      {onSkipToAnswer === undefined ? null : (
        <button className={secondaryButtonCompact} onClick={onSkipToAnswer} type="button">
          Skip to the answer
        </button>
      )}
      {onNextChange === undefined ? null : (
        <button className={secondaryButtonCompact} onClick={onNextChange} type="button">
          Next change
        </button>
      )}
      {artifact === undefined || approval === undefined ? null : (
        <>
          <ApprovalChip
            artifact={artifact}
            onReviewed={() => onAnswered(ask.id)}
            variant="header"
          />
          {approveDrawn ? null : <EarlierApproval approval={approval} />}
        </>
      )}
    </header>
  );
}
