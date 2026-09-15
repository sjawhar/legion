import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { badgeLow, badgePrimary, hasFocusVisibleRing } from "../../theme/classes";
import { useIssueAssignee } from "./useIssueAssignee";

/**
 * The single assignee editor: shows who answers the issue's asks as a badge (their lowercase
 * login, or a muted "Unassigned" tag) and, on click, tap, Enter or Space, opens a native
 * `<select>` over the sign-in allowlist (`GET /users`) with Unassigned first — the same picker
 * on desktop and iPhone, laid invisibly over the badge exactly as `PriorityControl` does.
 * Anyone on the allowlist may reassign. The write goes through `useIssueAssignee`, so the
 * issue detail, the project lists and the inbox's partitions update at once and roll back
 * together; a refused login shows the server's reason. Labelled `Assignee of <KEY>`.
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
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.listUsers() });
  const write = useIssueAssignee(issueKey);
  const logins = (users.data ?? []).map((user) => user.login);
  // A login the allowlist no longer carries still names who holds the issue; keep it selectable
  // so the control never shows a value its options lack.
  const options = assignee === null || logins.includes(assignee) ? logins : [assignee, ...logins];
  const tone = assignee === null ? badgeLow : badgePrimary;
  return (
    <>
      <span
        className={`relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing} ${
          disabled ? "opacity-50" : ""
        }`}
      >
        <span
          aria-hidden="true"
          className={`inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${tone.bg} ${tone.text}`}
        >
          {assignee ?? "Unassigned"}
        </span>
        <select
          aria-label={`Assignee of ${issueKey}`}
          className="absolute inset-0 size-full cursor-pointer appearance-none opacity-0 disabled:cursor-not-allowed"
          disabled={disabled || write.pending}
          onMouseDown={(event) => event.stopPropagation()}
          onPointerDown={(event) => event.stopPropagation()}
          onTouchStart={(event) => event.stopPropagation()}
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
