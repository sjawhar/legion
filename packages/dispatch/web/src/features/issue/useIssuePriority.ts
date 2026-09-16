import { type QueryKey, useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";

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
  /** Saves `priority`. A pick made while a save is in flight shows at once and is saved when
   *  that save settles; the newest such pick is the one saved. */
  submit: (priority: IssuePriority | null) => void;
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
 * on failure, and the lists refetch once the server has answered. Requests go one at a time, so
 * the server sees picks in the order they were made: a pick during a save is queued behind it,
 * and the in-flight answer is not merged over a newer pick the reader can already see.
 */
export function useIssuePriority(issueKey: string): IssuePriorityWrite {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  // A pick made during a save, with the cache as it stood before the pick was shown: the base
  // the chained request rolls back to, so its failure never lands on the pick that failed.
  const queued = useRef<{ priority: IssuePriority | null; snapshot: Snapshot } | null>(null);
  const chainedSnapshot = useRef<Snapshot | null>(null);
  const detailKey = ["issue", issueKey] as const;
  const takeSnapshot = (): Snapshot => ({
    details: queryClient.getQueryData<IssueDetails>(detailKey),
    inbox: queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey }),
    lists: queryClient.getQueriesData<IssueSummary[]>({ queryKey: projectListsKey }),
  });
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
  const mutation = useMutation({
    mutationFn: (priority: IssuePriority | null) => api.patchIssue(issueKey, { priority }),
    onMutate: async (priority): Promise<Snapshot> => {
      await Promise.all(
        [detailKey, projectListsKey, inboxKey].map((queryKey) =>
          queryClient.cancelQueries({ queryKey })
        )
      );
      const snapshot = chainedSnapshot.current ?? takeSnapshot();
      chainedSnapshot.current = null;
      showPriority(priority);
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
      if (queued.current !== null) {
        // The cache is back at this save's base, which is now the queued pick's base too.
        queued.current.snapshot = snapshot;
      }
    },
    onSuccess: (next) => {
      if (queued.current === null) {
        mergeIssue(queryClient, next);
      }
    },
    onSettled: () => {
      guard.release();
      const next = queued.current;
      if (next !== null) {
        queued.current = null;
        chainedSnapshot.current = next.snapshot;
        guard.guard(() => mutation.mutate(next.priority));
        return;
      }
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
    submit: (priority) => {
      if (guard.guard(() => mutation.mutate(priority))) {
        return;
      }
      queued.current = { priority, snapshot: queued.current?.snapshot ?? takeSnapshot() };
      showPriority(priority);
    },
  };
}
