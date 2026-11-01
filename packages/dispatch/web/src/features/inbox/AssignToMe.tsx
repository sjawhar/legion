import { type ReactNode, useEffect } from "react";

import { QueryError } from "../../components/QueryError";
import {
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { useIssueAssignee } from "../issue/useIssueAssignee";

/** Takes an issue nobody holds for the viewer: one PATCH through the shared assignee write, so
 *  the row is theirs at once and rolls back if the server refuses. Under the Unassigned filter
 *  the row stops matching while the save is in flight, so the control tells the Inbox when its
 *  write is live (pending or failed) and the Inbox keeps the row listed until then: `Assigning…`,
 *  and on refusal the server's reason with Retry, are shown by this same instance rather than
 *  lost to a remount.
 *
 *  Its own module rather than `Inbox.tsx`'s, because the Inbox's reading pane draws it too
 *  (`ReadingPaneHead`): the Inbox mounts the pane, so a control the pane's head imports from the
 *  Inbox would close a cycle between the three files. */
export function AssignToMe({
  askId,
  issueKey,
  onLive,
  viewer,
}: {
  /** The row this control sits in; liveness is registered per row, never per issue, so a
   *  sibling row of the same issue leaving the list cannot drop this row's registration. */
  askId: string;
  issueKey: string;
  /** Registers this row while its write is in flight or failed; unregistered once it settles
   *  cleanly or the control unmounts. */
  onLive: (askId: string, live: boolean) => void;
  viewer: string;
}): ReactNode {
  const write = useIssueAssignee(issueKey);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live) return;
    onLive(askId, true);
    return () => onLive(askId, false);
  }, [askId, live, onLive]);
  return (
    <>
      <button
        aria-label={`Assign ${issueKey} to me`}
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        disabled={write.pending}
        onClick={() => write.submit(viewer)}
        type="button"
      >
        {write.pending ? "Assigning…" : "Assign to me"}
      </button>
      {write.failed ? (
        <div className="basis-full">
          <QueryError
            message={write.error ?? `Could not assign ${issueKey} to you.`}
            onRetry={write.retry}
            retrying={write.pending}
          />
        </div>
      ) : null}
    </>
  );
}
