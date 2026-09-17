import { useQueryClient } from "@tanstack/react-query";

import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import { inboxKey, projectListsKey, useIssueFieldWrite } from "./useIssueFieldWrite";

export interface IssueAssigneeWrite {
  /** The server's reason after a refused save (an unlisted login), until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `assignee` (a login, or null to unassign). A pick made while a save is in flight
   *  shows at once and is saved when that save settles; the newest such pick is the one saved. */
  submit: (assignee: string | null) => void;
}

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands optimistically in the issue detail, every
 * project issue list and every inbox row that belongs to the issue (so the row moves between
 * the inbox's Mine and Unassigned partitions at once), rolls back on failure, and the lists
 * refetch once the server has answered (see `useIssueFieldWrite`).
 */
export function useIssueAssignee(issueKey: string): IssueAssigneeWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issueKey] as const;
  const showAssignee = (assignee: string | null) => {
    queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
      current === undefined ? undefined : { ...current, assignee }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, assignee } : issue))
    );
    queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
      current?.map((row) =>
        row.issue_key === issueKey && row.issue !== undefined
          ? { ...row, issue: { ...row.issue, assignee } }
          : row
      )
    );
  };
  return useIssueFieldWrite<string | null>(issueKey, {
    patch: (assignee) => ({ assignee }),
    show: showAssignee,
  });
}
