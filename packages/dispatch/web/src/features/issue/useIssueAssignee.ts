import { type QueryKey, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import type { InboxRow, IssueDetails, IssueSummary } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export interface IssueAssigneeWrite {
  /** The server's reason after a refused save (an unlisted login), until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `assignee` (a login, or null to unassign) unless a save is in flight; returns whether
   *  it started. */
  submit: (assignee: string | null) => boolean;
}

interface Snapshot {
  details: IssueDetails | undefined;
  inbox: [QueryKey, InboxRow[] | undefined][];
  lists: [QueryKey, IssueSummary[] | undefined][];
}

const projectListsKey = ["issues", "project"] as const;
const inboxKey = ["inbox"] as const;

/**
 * The one way the SPA writes an issue's assignee. The issue header's picker and the inbox's
 * "Assign to me" share this: the new login lands optimistically in the issue detail, every
 * project issue list and every inbox row that belongs to the issue (so the row moves between
 * the inbox's Mine and Unassigned partitions at once), rolls back on failure, and the lists
 * refetch once the server has answered.
 */
export function useIssueAssignee(issueKey: string): IssueAssigneeWrite {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  const detailKey = ["issue", issueKey] as const;
  const mutation = useMutation({
    mutationFn: (assignee: string | null) => api.patchIssue(issueKey, { assignee }),
    onMutate: async (assignee): Promise<Snapshot> => {
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
      return snapshot;
    },
    onError: (_error, _assignee, snapshot) => {
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
      // Another write to the same issue may have settled in between; refetching makes the
      // caches server-authoritative rather than trusting the arrival order of two responses.
      void queryClient.invalidateQueries({ queryKey: detailKey });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    error: mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    retry: () => {
      guard.retryLast(mutation);
    },
    submit: (assignee) => guard.guard(() => mutation.mutate(assignee)),
  };
}
