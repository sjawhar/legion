import { useQueryClient } from "@tanstack/react-query";

import type { InboxRow, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { inboxKey, projectListsKey, useIssueFieldWrite } from "./useIssueFieldWrite";

/** `value` as a priority. The route table carries it as a plain number; Dispatch writes 0 to 3
 *  and nothing else, and a number outside that reads as no priority rather than as a lane and a
 *  label no surface has. */
export function issuePriority(value: number | null | undefined): IssuePriority | null {
  return value === 0 || value === 1 || value === 2 || value === 3 ? value : null;
}

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
    label: (priority) => `${issueKey} set to ${priority === null ? "no priority" : `P${priority}`}`,
    patch: (priority) => ({ priority }),
    show: showPriority,
  });
  return { failed: write.failed, pending: write.pending, retry: write.retry, submit: write.submit };
}
