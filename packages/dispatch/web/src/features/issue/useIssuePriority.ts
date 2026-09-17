import { useQueryClient } from "@tanstack/react-query";

import type { InboxRow, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { inboxKey, projectListsKey, useIssueFieldWrite } from "./useIssueFieldWrite";

export interface IssuePriorityWrite {
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `priority`. A pick made while a save is in flight shows at once and is saved when
   *  that save settles; the newest such pick is the one saved. */
  submit: (priority: IssuePriority | null) => void;
}

/**
 * The one way the SPA writes an issue's priority. Every surface that renders it (issue header,
 * board cards, list rows, inbox rows) shares this: the new value lands optimistically in the
 * issue detail, every project issue list and every inbox query that shows the issue, rolls back
 * on failure, and the lists refetch once the server has answered (see `useIssueFieldWrite`).
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const queryClient = useQueryClient();
  const detailKey = ["issue", issueKey] as const;
  const showPriority = (priority: IssuePriority | null) => {
    queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
      current === undefined ? undefined : { ...current, priority }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, priority } : issue))
    );
    queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
      current?.map((row) => (row.issue_key === issueKey ? { ...row, priority } : row))
    );
  };
  const write = useIssueFieldWrite<IssuePriority | null>(issueKey, {
    patch: (priority) => ({ priority }),
    show: showPriority,
  });
  return { failed: write.failed, pending: write.pending, retry: write.retry, submit: write.submit };
}
