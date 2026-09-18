import { type QueryKey, useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";

import { api } from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { inboxQuery } from "../../api/queries";
import type { InboxRow, IssueDetails, IssueSummary, UpdateIssueInput } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export interface IssueFieldWrite<Value> {
  /** The server's reason after a refused save, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed save; a no-op while one is in flight. */
  retry: () => void;
  /** Saves `value`. A pick made while a save is in flight shows at once and is saved when that
   *  save settles; the newest such pick is the one saved. */
  submit: (value: Value) => void;
}

interface Snapshot {
  details: IssueDetails | undefined;
  inbox: [QueryKey, InboxRow[] | undefined][];
  lists: [QueryKey, IssueSummary[] | undefined][];
}

export const projectListsKey = ["issues", "project"] as const;
export const inboxKey = inboxQuery().queryKey;

/**
 * The optimistic write engine behind every single-field issue write (priority, assignee). The
 * caller supplies `patch`, the request body for a value, and `show`, which lands the value in
 * the issue detail, every project issue list and every inbox row that shows the issue. The engine
 * snapshots those caches before showing a pick, rolls them back on failure, merges the server's
 * answer on success, and refetches once the save settles. Requests go one at a time, so the
 * server sees picks in the order they were made: a pick during a save is queued behind it, and
 * the in-flight answer is not merged over a newer pick the reader can already see.
 */
export function useIssueFieldWrite<Value>(
  issueKey: string,
  field: { patch: (value: Value) => UpdateIssueInput; show: (value: Value) => void }
): IssueFieldWrite<Value> {
  const queryClient = useQueryClient();
  const guard = useSubmitGuard();
  // A pick made during a save, with the cache as it stood before the pick was shown: the base
  // the chained request rolls back to, so its failure never lands on the pick that failed.
  const queued = useRef<{ value: Value; snapshot: Snapshot } | null>(null);
  const chainedSnapshot = useRef<Snapshot | null>(null);
  const detailKey = ["issue", issueKey] as const;
  const takeSnapshot = (): Snapshot => ({
    details: queryClient.getQueryData<IssueDetails>(detailKey),
    inbox: queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey }),
    lists: queryClient.getQueriesData<IssueSummary[]>({ queryKey: projectListsKey }),
  });
  const mutation = useMutation({
    mutationFn: (value: Value) => api.patchIssue(issueKey, field.patch(value)),
    onMutate: async (value): Promise<Snapshot> => {
      await Promise.all(
        [detailKey, projectListsKey, inboxKey].map((queryKey) =>
          queryClient.cancelQueries({ queryKey })
        )
      );
      const snapshot = chainedSnapshot.current ?? takeSnapshot();
      chainedSnapshot.current = null;
      field.show(value);
      return snapshot;
    },
    onError: (_error, _value, snapshot) => {
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
        guard.guard(() => mutation.mutate(next.value));
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
    error: mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    retry: () => {
      guard.retryLast(mutation);
    },
    submit: (value) => {
      if (guard.guard(() => mutation.mutate(value))) {
        return;
      }
      queued.current = { value, snapshot: queued.current?.snapshot ?? takeSnapshot() };
      field.show(value);
    },
  };
}
