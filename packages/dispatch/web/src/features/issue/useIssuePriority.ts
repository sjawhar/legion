import { type QueryKey, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type { InboxRow, IssueDetails, IssuePriority, IssueSummary } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export interface IssuePriorityWrite {
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `priority` unless a save is already in flight; returns whether it started. */
  submit: (priority: IssuePriority | null) => boolean;
}

interface Snapshot {
  details: IssueDetails | undefined;
  inbox: [QueryKey, InboxRow[] | undefined][];
  lists: [QueryKey, IssueSummary[] | undefined][];
}

const projectListsKey = ["issues", "project"] as const;
const inboxKey = ["inbox"] as const;

/**
 * The one way the SPA writes an issue's priority. Every surface that renders it (issue header,
 * board cards, list rows, inbox rows) shares this: the new value lands optimistically in the
 * issue detail, every project issue list and every inbox query that shows the issue, rolls back
 * on failure, and the lists refetch once the server has answered.
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  const detailKey = ["issue", issueKey] as const;
  const mutation = useMutation({
    mutationFn: (priority: IssuePriority | null) => api.patchIssue(issueKey, { priority }),
    onMutate: async (priority): Promise<Snapshot> => {
      await Promise.all(
        [detailKey, projectListsKey, inboxKey].map((queryKey) =>
          queryClient.cancelQueries({ queryKey })
        )
      );
      const snapshot: Snapshot = {
        details: queryClient.getQueryData<IssueDetails>(detailKey),
        inbox: queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey }),
        lists: queryClient.getQueriesData<IssueSummary[]>({ queryKey: projectListsKey }),
      };
      queryClient.setQueryData<IssueDetails>(detailKey, (current) =>
        current === undefined ? undefined : { ...current, priority }
      );
      queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
        current?.map((issue) => (issue.key === issueKey ? { ...issue, priority } : issue))
      );
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) => (row.issue_key === issueKey ? { ...row, priority } : row))
      );
      return snapshot;
    },
    onError: (_error, _priority, snapshot) => {
      if (snapshot === undefined) {
        return;
      }
      if (snapshot.details !== undefined) {
        queryClient.setQueryData<IssueDetails>(detailKey, snapshot.details);
      }
      for (const [key, data] of snapshot.lists) {
        queryClient.setQueryData(key, data);
      }
      for (const [key, data] of snapshot.inbox) {
        queryClient.setQueryData(key, data);
      }
    },
    onSuccess: (next) => {
      mergeIssue(queryClient, next);
    },
    onSettled: () => {
      guard.release();
      // Another write to the same issue (a status change from the header) may have settled in
      // between; refetching the detail makes it server-authoritative rather than trusting the
      // arrival order of the two responses or a pre-write snapshot.
      void queryClient.invalidateQueries({ queryKey: detailKey });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    failed: mutation.isError,
    pending: mutation.isPending,
    retry: () => {
      guard.retryLast(mutation);
    },
    submit: (priority) => guard.guard(() => mutation.mutate(priority)),
  };
}
