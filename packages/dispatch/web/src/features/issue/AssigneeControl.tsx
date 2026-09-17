import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { badgeLow, badgePrimary } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "./badge-select";
import { useIssueAssignee } from "./useIssueAssignee";

/**
 * The single assignee editor: shows who answers the issue's asks as a badge (their lowercase
 * login, or a muted "Unassigned" tag) and, on click, tap, Enter or Space, opens a native
 * `<select>` over the sign-in allowlist (`GET /users`) with Unassigned first — the same picker
 * on desktop and iPhone, laid invisibly over the badge exactly as `PriorityControl` does.
 * The allowlist is read only once the reader reaches for the control (pointer over it, a
 * touch on it, or focus) and then kept for the session: a fresh issue load makes no request
 * for it, and the badge never waits on it. Anyone on the allowlist may reassign. The write
 * goes through `useIssueAssignee`, so the issue detail, the project lists and the inbox's
 * partitions update at once and roll back together; a refused login shows the server's
 * reason. Like `PriorityControl`, the select stays enabled while a save is in flight - a
 * disabled control drops the focus it holds and leaves the tab order - and a pick made meanwhile
 * is queued behind the save. Labelled `Assignee of <KEY>`.
 */
export function AssigneeControl({
  assignee,
  disabled = false,
  issueKey,
}: {
  assignee: string | null;
  disabled?: boolean;
  issueKey: string;
}): ReactNode {
  const [wanted, setWanted] = useState(false);
  const users = useQuery({
    enabled: wanted,
    queryKey: ["users"],
    queryFn: () => api.listUsers(),
    staleTime: Number.POSITIVE_INFINITY,
  });
  const want = () => setWanted(true);
  const write = useIssueAssignee(issueKey);
  const logins = (users.data ?? []).map((user) => user.login);
  // A login the allowlist no longer carries still names who holds the issue; keep it selectable
  // so the control never shows a value its options lack.
  const options = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  const tone = assignee === null ? badgeLow : badgePrimary;
  return (
    <>
      <span className={`${badgeSelectWrapper} ${disabled ? "opacity-50" : ""}`}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${tone.bg} ${tone.text}`}>
          {assignee ?? "Unassigned"}
        </span>
        <select
          aria-label={`Assignee of ${issueKey}`}
          className={badgeSelectOverlay}
          disabled={disabled}
          onFocus={want}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onPointerEnter={want}
          onTouchStart={(event) => {
            event.stopPropagation();
            want();
          }}
          onChange={(event) => {
            const { value } = event.target;
            write.submit(value === "" ? null : value);
          }}
          value={assignee ?? ""}
        >
          <option value="">Unassigned</option>
          {options.map((login) => (
            <option key={login} value={login}>
              {login}
            </option>
          ))}
        </select>
      </span>
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the assignee of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </>
  );
}
